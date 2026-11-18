package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/human"
)

// AssetState describes an optional archive of the backup (LFS objects or
// attachments) and whether it has been extracted.
type AssetState struct {
	Name      string `json:"name"`           // "lfs" or "uploads"
	Present   bool   `json:"present"`        // the backup contains the archive
	Size      int64  `json:"size"`           // compressed size in the backup
	Disk      int64  `json:"disk"`           // space needed once extracted (from the database, else Size)
	Enabled   bool   `json:"enabled"`        // --lfs / --uploads
	Mode      string `json:"mode,omitempty"` // lfs: "all" or "head" (objects on default branches)
	Extracted bool   `json:"extracted"`      // ready to serve
	Running   bool   `json:"running"`
	Done      int64  `json:"done"` // compressed bytes read so far
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	Error     string `json:"error,omitempty"`
}

// Percent of the extraction, or -1.
func (a AssetState) Percent() int {
	if a.Size <= 0 {
		return -1
	}
	return int(a.Done * 100 / a.Size)
}

type assets struct {
	mu    sync.Mutex
	state map[string]*AssetState
}

type assetMarker struct {
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	Mode  string `json:"mode,omitempty"`
}

// LFS extraction modes.
const (
	LFSAll  = "all"
	LFSHead = "head"
)

// lfsMode: a mode given on the command line wins over the saved one.
func lfsMode(cfg, saved string) string {
	for _, m := range []string{cfg, saved} {
		if m == LFSAll || m == LFSHead {
			return m
		}
	}
	return LFSAll
}

func (m *Manager) assetDir(name string) string { return filepath.Join(m.root, name) }

func assetMember(name string) string {
	if name == "lfs" {
		return backup.LFSMember
	}
	return backup.UploadsMember
}

// Asset returns the state of "lfs" or "uploads".
func (m *Manager) Asset(name string) AssetState {
	m.assets.mu.Lock()
	defer m.assets.mu.Unlock()
	if st := m.assets.state[name]; st != nil {
		return *st
	}
	return AssetState{Name: name}
}

func (m *Manager) setAsset(name string, f func(*AssetState)) {
	m.assets.mu.Lock()
	defer m.assets.mu.Unlock()
	if m.assets.state == nil {
		m.assets.state = map[string]*AssetState{}
	}
	st := m.assets.state[name]
	if st == nil {
		st = &AssetState{Name: name}
		m.assets.state[name] = st
	}
	f(st)
}

// initAssets records what the backup holds and what is already extracted.
func (m *Manager) initAssets(src backup.Source) {
	exp := m.Expected()
	for _, name := range []string{"lfs", "uploads"} {
		member := assetMember(name)
		var size int64
		present := false
		for _, e := range src.Entries() {
			if e.Name == member {
				present, size = true, e.Size
			}
		}
		saved := m.loadSettings()
		enabled := (name == "lfs" && (m.cfg.LFS || saved.LFS)) || (name == "uploads" && (m.cfg.Uploads || saved.Uploads))
		var mk assetMarker
		extracted := false
		if b, err := os.ReadFile(filepath.Join(m.assetDir(name), ".gbb-extracted.json")); err == nil && json.Unmarshal(b, &mk) == nil {
			extracted = true
		}
		mode := ""
		if name == "lfs" {
			mode = lfsMode(m.cfg.LFSMode, saved.LFSMode)
			if mk.Mode == "" {
				mk.Mode = LFSAll
			}
			// A partial (head) extraction does not satisfy "all"; the
			// reverse is fine.
			if extracted && mode == LFSAll && mk.Mode == LFSHead {
				extracted = false
			}
			if extracted {
				mode = mk.Mode
			}
		}
		m.setAsset(name, func(st *AssetState) {
			st.Present, st.Size, st.Enabled, st.Extracted = present, size, enabled, extracted
			// The database knows the real sizes; compressed size is only a floor.
			st.Disk = size
			if exp != nil && name == "lfs" && exp.LFSBytes() > size {
				st.Disk = exp.LFSBytes()
			} else if exp != nil && name == "uploads" && exp.UploadsBytes > size {
				st.Disk = exp.UploadsBytes
			}
			st.Files, st.Bytes, st.Mode = mk.Files, mk.Bytes, mode
		})
	}
}

// extractAssets runs the enabled extractions that are not done yet.
func (m *Manager) extractAssets(src backup.Source) {
	for _, name := range []string{"uploads", "lfs"} { // uploads first: small and visible in issues
		m.extractOne(src, name)
	}
}

type settings struct {
	LFS     bool   `json:"lfs,omitempty"`
	LFSMode string `json:"lfs_mode,omitempty"`
	Uploads bool   `json:"uploads,omitempty"`
}

func (m *Manager) loadSettings() settings {
	var s settings
	if b, err := os.ReadFile(filepath.Join(m.root, "settings.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// EnableAsset turns on "lfs" or "uploads" for this backup (remembered in
// its data directory) and starts the extraction in the background. mode
// is LFSAll or LFSHead for "lfs" (ignored otherwise); asking for all the
// objects after a head extraction extracts again.
func (m *Manager) EnableAsset(name, mode string) error {
	if name != "lfs" && name != "uploads" {
		return fmt.Errorf("unknown asset %q", name)
	}
	if name == "lfs" && mode != LFSHead {
		mode = LFSAll
	}
	src := m.Source()
	if src == nil || m.Index() == nil {
		return fmt.Errorf("the backup is still being opened")
	}
	s := m.loadSettings()
	if name == "lfs" {
		s.LFS, s.LFSMode = true, mode
	} else {
		s.Uploads = true
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(m.root, "settings.json"), b, 0o644); err != nil {
		return err
	}
	m.setAsset(name, func(st *AssetState) {
		st.Enabled = true
		if name == "lfs" && !st.Running && (!st.Extracted || mode == LFSAll && st.Mode == LFSHead) {
			st.Extracted, st.Mode = false, mode
		}
	})
	go func() {
		m.extractOne(src, name)
		m.setPhase("ready", 0, 0)
	}()
	return nil
}

// extractOne extracts one archive if enabled, present and not done (or
// already running).
func (m *Manager) extractOne(src backup.Source, name string) {
	start := false
	m.assets.mu.Lock()
	if st := m.assets.state[name]; st != nil && st.Enabled && st.Present && !st.Extracted && !st.Running {
		st.Running, start = true, true
	}
	m.assets.mu.Unlock()
	if !start {
		return
	}
	defer m.setAsset(name, func(s *AssetState) { s.Running = false })
	{
		st := m.Asset(name)
		dir := m.assetDir(name)
		keep := backup.KeepUpload
		need := st.Disk
		if name == "lfs" {
			keep = backup.KeepLFS
			if st.Mode == LFSHead {
				oids, size, err := m.lfsHeadObjects()
				if err != nil {
					m.cfg.Logf("LFS objects on default branches: %v; extracting all of them", err)
					m.setAsset(name, func(s *AssetState) { s.Mode = LFSAll })
				} else {
					m.cfg.Logf("LFS: %d objects (%s) on default branches", len(oids), human.Bytes(size))
					need = size
					keep = func(n string) (string, bool) {
						rel, ok := backup.KeepLFS(n)
						return rel, ok && oids[backup.LFSOIDOf(rel)]
					}
				}
			}
		}
		if free, ok := diskFree(m.root); ok && free < uint64(need)+(512<<20) {
			msg := fmt.Sprintf("not enough free space to extract %s (%s free, about %s needed)",
				assetMember(name), human.Bytes(int64(free)), human.Bytes(need))
			m.cfg.Logf("%s", msg)
			m.setAsset(name, func(s *AssetState) { s.Error = msg })
			return
		}
		m.cfg.Logf("extracting %s (%s)", assetMember(name), human.Bytes(st.Size))
		m.setAsset(name, func(s *AssetState) { s.Error = "" })
		start := time.Now()
		mode := m.Asset(name).Mode
		files, bytes, err := backup.ExtractAssets(src, assetMember(name), dir, keep, func(done, total int64) {
			m.setAsset(name, func(s *AssetState) { s.Done = done })
			m.setPhase("extracting "+assetMember(name), done, total)
		}, m.throttle)
		if err == nil {
			b, _ := json.Marshal(assetMarker{files, bytes, mode})
			err = os.WriteFile(filepath.Join(dir, ".gbb-extracted.json"), b, 0o644)
		}
		m.setAsset(name, func(s *AssetState) {
			s.Running = false
			if err != nil {
				s.Error = err.Error()
			} else {
				s.Extracted, s.Files, s.Bytes, s.Done = true, files, bytes, s.Size
			}
		})
		if err != nil {
			m.cfg.Logf("extracting %s: %v", assetMember(name), err)
		} else {
			m.cfg.Logf("extracted %s: %d files, %s in %s", assetMember(name), files, human.Bytes(bytes), time.Since(start).Round(time.Second))
		}
	}
}

// LFSObject returns the path of an extracted LFS object.
func (m *Manager) LFSObject(oid string) (string, bool) {
	if st := m.Asset("lfs"); !st.Extracted && !st.Running {
		return "", false
	}
	p := backup.LFSPath(m.assetDir("lfs"), oid)
	if p == "" {
		return "", false
	}
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, true
	}
	return "", false
}

// Upload returns the path of an extracted attachment of a project, looked
// up under the project's storage path (hashed) and its full path (legacy).
func (m *Manager) Upload(diskPath, fullPath, secret, file string) (string, bool) {
	if !m.Asset("uploads").Extracted || !safeKey(secret) || !safeKey(file) {
		return "", false
	}
	for _, base := range []string{diskPath, fullPath} {
		if base == "" || !safeKey(base) {
			continue
		}
		p := filepath.Join(m.assetDir("uploads"), filepath.FromSlash(base), secret, file)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// lfsHeadObjects lists the LFS objects referenced by the default branch of
// every project the database links to LFS objects, with their total size.
// Those repositories are unbundled on the way (they would be to browse
// them anyway); only pointer-sized blobs are read.
func (m *Manager) lfsHeadObjects() (map[string]bool, int64, error) {
	ix := m.Index()
	if ix == nil || !ix.LFSKnown {
		return nil, 0, errors.New("the database does not say which projects use LFS")
	}
	var keys []string
	for _, p := range ix.Projects {
		if p.HasLFS && p.Repo != "" {
			keys = append(keys, p.Repo)
		}
	}
	oids := map[string]bool{}
	var total int64
	for i, key := range keys {
		m.setPhase("finding LFS files on default branches", int64(i), int64(len(keys)))
		m.throttle()
		if err := m.Ensure(key); err != nil {
			m.cfg.Logf("LFS scan: %s: %v", key, err)
			continue
		}
		repo, release, err := m.Repo(key)
		if err != nil {
			m.cfg.Logf("LFS scan: %s: %v", key, err)
			continue
		}
		err = lfsInTree(repo, func(oid string, size int64) {
			if !oids[oid] {
				oids[oid] = true
				total += size
			}
		})
		release()
		if err != nil {
			m.cfg.Logf("LFS scan: %s: %v", key, err)
		}
	}
	return oids, total, nil
}

// lfsInTree calls fn for every LFS pointer in the tree of HEAD.
func lfsInTree(repo *gitstore.Repo, fn func(oid string, size int64)) error {
	h, _, err := repo.Resolve("HEAD")
	if err != nil {
		return err
	}
	c, err := repo.Commit(h)
	if err != nil {
		return err
	}
	seen := map[gitstore.Hash]bool{} // identical subtrees are walked once
	var walk func(t gitstore.Hash) error
	walk = func(t gitstore.Hash) error {
		if seen[t] {
			return nil
		}
		seen[t] = true
		es, err := repo.Tree(t)
		if err != nil {
			return err
		}
		for _, e := range es {
			switch {
			case e.IsDir():
				if err := walk(e.Hash); err != nil {
					return err
				}
			case e.IsSubmodule(), e.IsSymlink():
			default:
				// Pointers are ~130 bytes: skip real files without inflating them.
				if _, size, err := repo.Stat(e.Hash); err != nil || size > 1024 {
					continue
				}
				if _, data, err := repo.Object(e.Hash); err == nil {
					if oid, size, ok := backup.LFSPointer(data); ok {
						fn(oid, size)
					}
				}
			}
		}
		return nil
	}
	return walk(c.Tree)
}
