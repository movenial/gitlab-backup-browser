package app

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hub lets one server switch between the backups found in a few allowed
// directories. Only one backup is open at a time.
type Hub struct {
	base Config   // template for each backup's manager (Backup is set per backup)
	dirs []string // directories scanned for backups

	mu         sync.Mutex
	active     *Manager
	activePath string
}

// BackupFile is a backup found on disk.
type BackupFile struct {
	Path    string
	Name    string
	Size    int64
	ModTime time.Time
	Created time.Time // from the GitLab archive name, zero if unknown
	Version string    // GitLab version from the archive name
	IsDir   bool
	Indexed bool // an index is cached: opening it is instant
	Active  bool
}

// NewHub creates a hub. If base.Backup is set, that backup is opened.
func NewHub(base Config, dirs []string) *Hub {
	h := &Hub{base: base}
	for _, d := range dirs {
		if abs, err := filepath.Abs(d); err == nil {
			h.dirs = append(h.dirs, abs)
		}
	}
	if base.Backup != "" {
		if abs, err := filepath.Abs(base.Backup); err == nil {
			h.dirs = append(h.dirs, filepath.Dir(abs))
			h.openLocked(abs)
		}
	}
	h.dirs = dedupe(h.dirs)
	return h
}

// HubWith wraps an already started manager (tests, single-backup use).
func HubWith(m *Manager) *Hub {
	return &Hub{active: m, activePath: m.cfg.Backup, base: m.cfg}
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Dirs returns the scanned directories.
func (h *Hub) Dirs() []string { return h.dirs }

// Active returns the open backup's manager, or nil.
func (h *Hub) Active() *Manager {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

// gitlab archive names: <unix>_<yyyy>_<mm>_<dd>_<version>[-ee]_gitlab_backup.tar
var archiveName = regexp.MustCompile(`^(\d{9,11})_\d{4}_\d{2}_\d{2}_([0-9][0-9a-z.\-]*?)_gitlab_backup\.tar(\.gz)?$`)

func isBackupName(name string) bool {
	return strings.HasSuffix(name, "_gitlab_backup.tar") || strings.HasSuffix(name, "_gitlab_backup.tar.gz")
}

// Scan lists the backups in the allowed directories, newest first: GitLab
// archives (*_gitlab_backup.tar[.gz]) and extracted backup directories
// (holding backup_information.yml).
func (h *Hub) Scan() []BackupFile {
	h.mu.Lock()
	activePath := h.activePath
	h.mu.Unlock()
	var out []BackupFile
	seen := map[string]bool{}
	for _, d := range h.dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(d, e.Name())
			if seen[p] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			bf := BackupFile{Path: p, Name: e.Name(), ModTime: info.ModTime()}
			switch {
			case info.Mode().IsRegular() && isBackupName(e.Name()):
				bf.Size = info.Size()
			case info.IsDir():
				if _, err := os.Stat(filepath.Join(p, "backup_information.yml")); err != nil {
					continue
				}
				bf.IsDir = true
				// Extracted directories often share a name ("backup", "stage"):
				// show the parent too.
				bf.Name = filepath.Base(d) + "/" + e.Name()
			default:
				continue
			}
			if m := archiveName.FindStringSubmatch(e.Name()); m != nil {
				if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					bf.Created = time.Unix(sec, 0).UTC()
				}
				bf.Version = m[2]
			}
			if root, err := CacheRoot(h.base.DataDir, p); err == nil {
				if _, err := os.Stat(filepath.Join(root, "index.json")); err == nil {
					bf.Indexed = true
				}
			}
			bf.Active = p == activePath
			seen[p] = true
			out = append(out, bf)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ta, tb := a.Created, b.Created
		if ta.IsZero() {
			ta = a.ModTime
		}
		if tb.IsZero() {
			tb = b.ModTime
		}
		return ta.After(tb)
	})
	return out
}

// ErrNotAllowed is returned for paths outside the scanned backups.
var ErrNotAllowed = errors.New("not a backup in the allowed directories")

// Open switches to the backup at path, which must be one Scan lists.
func (h *Hub) Open(path string) error {
	found := false
	for _, b := range h.Scan() {
		if b.Path == path {
			found = true
			break
		}
	}
	if !found {
		return ErrNotAllowed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activePath == path && h.active != nil {
		return nil
	}
	h.openLocked(path)
	return nil
}

func (h *Hub) openLocked(path string) {
	old := h.active
	cfg := h.base
	cfg.Backup = path
	m := New(cfg)
	m.Start()
	h.active, h.activePath = m, path
	if old != nil {
		old.Close()
	}
}
