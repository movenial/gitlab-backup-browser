// Package app ties the pieces together: it imports a backup in the
// background, keeps the index, and materialises repositories on demand.
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/power"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

// Config controls a Manager.
type Config struct {
	Backup         string // path to the backup tar, tar.gz or directory
	DataDir        string // where the index, extracted files and repos live
	PauseOnBattery bool   // hold background work while unplugged
	Prefetch       bool   // unbundle every repository after indexing
	NoIssues       bool   // skip importing issues, merge requests and comments
	LFS            bool   // extract lfs.tar.gz to serve Git LFS content
	LFSMode        string // "all" (default) or "head": only objects on default branches
	Uploads        bool   // extract uploads.tar.gz to serve issue/MR attachments
	Logf           func(format string, args ...any)

	// Memory bounds; zero values pick phone-friendly defaults.
	MaxOpenRepos int           // repositories kept open at once (default 6)
	CacheBytes   int           // shared object cache budget (default 24 MiB)
	IdleClose    time.Duration // close repositories unused for this long (default 3m)
}

// Status is the import state shown to users.
type Status struct {
	Phase   string    `json:"phase"`
	Done    int64     `json:"done"`
	Total   int64     `json:"total"`
	Ready   bool      `json:"ready"` // full index available
	Paused  bool      `json:"paused"`
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"started"`
	Power   string    `json:"power"`
	// Prefetch progress (repos unbundled / total), when enabled.
	Built int `json:"built"`
	Repos int `json:"repos"`
}

// Percent returns progress in percent, or -1 when unknown.
func (s Status) Percent() int {
	if s.Total <= 0 {
		return -1
	}
	return int(s.Done * 100 / s.Total)
}

// Manager owns the backup source, the index and the repository cache.
type Manager struct {
	cfg  Config
	root string // data dir for this backup fingerprint

	mu     sync.RWMutex
	status Status
	ix     *index.Index
	src    backup.Source

	buildMu  sync.Mutex
	building map[string]*buildCall
	failed   map[string]string

	pool    *pool
	tracker *tracker.Store
	assets  assets

	closed    atomic.Bool
	closeOnce sync.Once
	busy      atomic.Bool // a background action (prefetch) is running

	lastPower     time.Time
	powerBusy     atomic.Bool
	lastPowerSeen power.State
}

type buildCall struct {
	done     chan struct{}
	err      error
	progress atomic.Pointer[gitstore.Progress]
}

// New creates a manager; call Start to begin importing.
func New(cfg Config) *Manager {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.MaxOpenRepos <= 0 {
		cfg.MaxOpenRepos = 6
	}
	if cfg.CacheBytes <= 0 {
		cfg.CacheBytes = 24 << 20
	}
	if cfg.IdleClose <= 0 {
		cfg.IdleClose = 3 * time.Minute
	}
	return &Manager{
		cfg:      cfg,
		status:   Status{Phase: "starting", Started: time.Now()},
		building: map[string]*buildCall{},
		failed:   map[string]string{},
		pool:     newPool(cfg.MaxOpenRepos, cfg.IdleClose, cfg.CacheBytes),
	}
}

// Status returns a snapshot of the import status.
func (m *Manager) Status() Status {
	m.mu.RLock()
	s := m.status
	stale := time.Since(m.lastPower) > 20*time.Second
	m.mu.RUnlock()
	// The power state is otherwise only read when there is work to pause:
	// refresh it in the background (reading it may run termux-battery-status).
	if stale && m.powerBusy.CompareAndSwap(false, true) {
		go func() {
			defer m.powerBusy.Store(false)
			m.powerState()
		}()
	}
	return s
}

// Index returns the current index (partial during import), or nil.
func (m *Manager) Index() *index.Index {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ix
}

func (m *Manager) setPhase(phase string, done, total int64) {
	m.mu.Lock()
	m.status.Phase, m.status.Done, m.status.Total = phase, done, total
	m.mu.Unlock()
}

func (m *Manager) fail(err error) {
	m.cfg.Logf("import failed: %v", err)
	m.mu.Lock()
	m.status.Error = err.Error()
	m.status.Phase = "failed"
	m.mu.Unlock()
}

// Start runs the import in the background.
func (m *Manager) Start() { go m.run() }

// rootLocks serialises imports into the same cache directory.
var rootLocks sync.Map

// Run runs the import synchronously (used by the CLI commands).
func (m *Manager) Run() error {
	m.run()
	if s := m.Status(); s.Error != "" {
		return errors.New(s.Error)
	}
	return nil
}

func (m *Manager) run() {
	fp, err := index.Fingerprint(m.cfg.Backup)
	if err != nil {
		m.fail(err)
		return
	}
	sum := sha256.Sum256([]byte(fp))
	m.root = filepath.Join(m.cfg.DataDir, "backup-"+hex.EncodeToString(sum[:6]))
	// A manager closed a moment ago for the same backup may still be
	// importing into this directory: wait for it to stop first.
	lk, _ := rootLocks.LoadOrStore(m.root, new(sync.Mutex))
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	if m.closed.Load() {
		return
	}
	// Everything read from the backup (private repositories, issues,
	// attachments) is written below this directory, so closing it to other
	// users closes all of it. MkdirAll leaves an existing directory as it is.
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		m.fail(err)
		return
	}
	if err := os.Chmod(m.root, 0o700); err != nil {
		m.fail(err)
		return
	}
	os.WriteFile(filepath.Join(m.root, "SOURCE"), []byte(fp+"\n"), 0o644)
	m.cleanupStale()

	kind, err := backup.Detect(m.cfg.Backup)
	if err != nil {
		m.fail(err)
		return
	}
	srcPath := m.cfg.Backup
	if kind == backup.KindTarGz {
		srcPath = filepath.Join(m.root, "extracted")
		if _, err := os.Stat(srcPath); err != nil {
			st, _ := os.Stat(m.cfg.Backup)
			total := st.Size()
			m.setPhase("extracting compressed archive", 0, total)
			m.cfg.Logf("extracting %s (needed once for .tar.gz backups)", m.cfg.Backup)
			err := backup.ExtractTarGz(m.cfg.Backup, srcPath, func(done int64) {
				m.setPhase("extracting compressed archive", done, total)
				m.throttle()
			})
			if err != nil {
				m.fail(fmt.Errorf("extracting: %w", err))
				return
			}
		}
	}
	m.setPhase("scanning archive", 0, 0)
	src, err := backup.OpenSource(srcPath)
	if err != nil {
		m.fail(err)
		return
	}
	m.mu.Lock()
	m.src = src
	m.mu.Unlock()

	ixPath := filepath.Join(m.root, "index.json")
	if ix, err := index.Load(ixPath, fp); err == nil && ix != nil {
		m.publish(ix, true)
		m.cfg.Logf("loaded cached index: %d projects", len(ix.Projects))
	} else {
		// Repositories become browsable (by hash) right away.
		m.publish(index.Partial(src, fp), false)
		opts := index.Options{ExpectedFile: filepath.Join(m.root, "expected.json")}
		if !m.cfg.NoIssues {
			opts.TrackerDir = filepath.Join(m.root, "tracker")
		}
		ix, err := index.Build(src, fp, opts, func(phase string, done, total int64) {
			m.setPhase(phase, done, total)
		}, m.throttle)
		if m.closed.Load() {
			return // abandoned: never cache an index cut short
		}
		if err != nil {
			m.fail(err)
			return
		}
		if err := ix.Save(ixPath); err != nil {
			m.cfg.Logf("saving index: %v", err)
		}
		m.publish(ix, true)
		st := ix.Stats()
		m.cfg.Logf("index ready: %d projects (%d with repository), %d unmatched bundles, database: %s",
			st.Projects, st.WithRepo, st.Unmatched, ix.DBStatus)
	}
	m.initAssets(src)
	m.extractAssets(src)
	if m.cfg.Prefetch && !m.closed.Load() {
		m.prefetch(0)
	}
	m.setPhase("ready", 0, 0)
}

func (m *Manager) publish(ix *index.Index, ready bool) {
	var ts *tracker.Store
	if ix.Tracker {
		var err error
		if ts, err = tracker.Open(filepath.Join(m.root, "tracker")); err != nil {
			m.cfg.Logf("issues unavailable: %v", err)
			ts = nil
		}
	}
	m.mu.Lock()
	m.ix = ix
	m.tracker = ts
	m.status.Ready = ready
	if ready {
		m.status.Phase = "ready"
	}
	m.mu.Unlock()
}

// cleanupStale removes cache directories of previous backups.
//
// A cache is kept while its backup still exists unchanged, so yesterday's
// backup stays indexed for comparisons until it is rotated away.
func (m *Manager) cleanupStale() {
	dirs, _ := filepath.Glob(filepath.Join(m.cfg.DataDir, "backup-*"))
	for _, d := range dirs {
		if d == m.root {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, "SOURCE"))
		if err != nil {
			continue // not ours
		}
		fp := strings.TrimSpace(string(b))
		path, _, _ := strings.Cut(fp, "|")
		if cur, err := index.Fingerprint(path); err == nil && cur == fp {
			continue // that backup is still there, unchanged
		}
		m.cfg.Logf("removing cache of a backup that no longer exists: %s", d)
		os.RemoveAll(d)
	}
}

// throttle blocks while the device is on battery and pausing is enabled.
// It also ends the calling background goroutine once the manager is
// closed (another backup was opened), so its import stops using CPU.
func (m *Manager) throttle() {
	if m.closed.Load() {
		runtime.Goexit()
	}
	if !m.cfg.PauseOnBattery {
		return
	}
	for {
		st := m.powerState()
		if st != power.OnBattery {
			m.mu.Lock()
			m.status.Paused = false
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		if !m.status.Paused {
			m.cfg.Logf("on battery: pausing background work until a charger is connected")
		}
		m.status.Paused = true
		m.mu.Unlock()
		time.Sleep(30 * time.Second)
	}
}

func (m *Manager) powerState() power.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.lastPower) > 20*time.Second {
		m.lastPowerSeen = power.Check()
		m.lastPower = time.Now()
		m.status.Power = m.lastPowerSeen.String()
	}
	return m.lastPowerSeen
}

// prefetch unbundles repositories in the background, most recently active
// projects first; n limits how many (0 = all).
func (m *Manager) prefetch(n int) {
	ix := m.Index()
	type item struct {
		key, activity string
	}
	seen := map[string]bool{}
	var items []item
	for _, p := range ix.Projects {
		for _, k := range []string{p.Repo, p.Wiki} {
			if k != "" && !seen[k] && ix.Repos[k] != nil {
				seen[k] = true
				items = append(items, item{k, p.LastActivity})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].activity > items[j].activity })
	if n > 0 && len(items) > n {
		items = items[:n]
	}
	m.mu.Lock()
	m.status.Repos, m.status.Built = len(items), 0
	m.mu.Unlock()
	for i, it := range items {
		m.setPhase("unbundling repositories", int64(i), int64(len(items)))
		m.throttle()
		// Only build: opening every repository would defeat the pool.
		if err := m.ensureBuilt(it.key); err != nil {
			m.cfg.Logf("unbundle %s: %v", it.key, err)
		}
		m.mu.Lock()
		m.status.Built = i + 1
		m.mu.Unlock()
	}
}

// PrefetchActive unbundles the n most recently active projects in the
// background. It returns false if a background action is already running.
func (m *Manager) PrefetchActive(n int) bool {
	if m.Index() == nil || !m.busy.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer m.busy.Store(false)
		m.prefetch(n)
		m.setPhase("ready", 0, 0)
	}()
	return true
}

// Busy reports whether a background action is running.
func (m *Manager) Busy() bool { return m.busy.Load() }

// Close stops this manager's background work and releases its files. It
// is used when another backup is opened; requests still being served keep
// working until they finish.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		close(m.pool.stop)
		m.pool.closeIdle(0)
		go func() {
			time.Sleep(time.Minute) // let in-flight requests and builds finish
			if src := m.Source(); src != nil {
				src.Close()
			}
		}()
	})
}

// BackupPath is the backup this manager serves.
func (m *Manager) BackupPath() string { return m.cfg.Backup }

// Root is this backup's directory inside the data directory ("" before
// the import started).
func (m *Manager) Root() string { return m.root }

// CacheRoot is the data sub-directory used for a backup file, without
// opening it.
func CacheRoot(dataDir, backupPath string) (string, error) {
	fp, err := index.Fingerprint(backupPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(fp))
	return filepath.Join(dataDir, "backup-"+hex.EncodeToString(sum[:6])), nil
}

// safeKey validates a repository key before it is used as a path.
func safeKey(k string) bool {
	if k == "" || strings.HasPrefix(k, "/") || strings.Contains(k, "\\") {
		return false
	}
	return path.Clean(k) == k && !strings.HasPrefix(k, "../") && k != ".."
}

// RepoDir returns where repository key is (or will be) materialised.
func (m *Manager) RepoDir(key string) string {
	return filepath.Join(m.root, "repos", filepath.FromSlash(key)+".git")
}

// Built reports whether a repository is already unbundled.
func (m *Manager) Built(key string) bool {
	if m.root == "" || !safeKey(key) {
		return false
	}
	_, err := os.Stat(filepath.Join(m.RepoDir(key), "HEAD"))
	return err == nil
}

// BuildError returns the last unbundle error of key, if any.
func (m *Manager) BuildError(key string) string {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	return m.failed[key]
}

// Building reports whether key is being unbundled right now.
func (m *Manager) Building(key string) bool {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	_, ok := m.building[key]
	return ok
}

// BuildProgress reports how far the unbundling of key is, if running.
func (m *Manager) BuildProgress(key string) (gitstore.Progress, bool) {
	m.buildMu.Lock()
	c, ok := m.building[key]
	m.buildMu.Unlock()
	if !ok {
		return gitstore.Progress{}, false
	}
	if p := c.progress.Load(); p != nil {
		return *p, true
	}
	return gitstore.Progress{Stage: "starting"}, true
}

// BuildAsync starts unbundling key in the background if needed.
func (m *Manager) BuildAsync(key string) {
	if m.Built(key) || m.Building(key) {
		return
	}
	go func() {
		if err := m.ensureBuilt(key); err != nil {
			m.cfg.Logf("unbundle %s: %v", key, err)
		}
	}()
}

// ErrNoBundle is returned for repositories without a bundle.
var ErrNoBundle = errors.New("no bundle for this repository in the backup")

// Repo returns the opened repository for key, unbundling it on first use.
// The caller must call release once it no longer reads from the repo.
func (m *Manager) Repo(key string) (repo *gitstore.Repo, release func(), err error) {
	if !safeKey(key) {
		return nil, nil, fmt.Errorf("invalid repository key %q", key)
	}
	if err := m.ensureBuilt(key); err != nil {
		return nil, nil, err
	}
	return m.pool.acquire(key, func(c *gitstore.Cache) (*gitstore.Repo, error) {
		return gitstore.OpenWithCache(m.RepoDir(key), c)
	})
}

// Ensure unbundles key if needed, without opening it.
func (m *Manager) Ensure(key string) error {
	if !safeKey(key) {
		return fmt.Errorf("invalid repository key %q", key)
	}
	return m.ensureBuilt(key)
}

// MemStats reports memory use and the repository pool state.
func (m *Manager) MemStats() MemStats { return m.pool.stats(m.cfg.CacheBytes) }

func (m *Manager) ensureBuilt(key string) error {
	if m.Built(key) {
		return nil
	}
	m.mu.RLock()
	ix, src := m.ix, m.src
	m.mu.RUnlock()
	if ix == nil || src == nil {
		return errors.New("the backup is still being scanned")
	}
	rb := ix.Repos[key]
	if rb == nil {
		return ErrNoBundle
	}
	m.buildMu.Lock()
	if c, ok := m.building[key]; ok {
		m.buildMu.Unlock()
		<-c.done
		return c.err
	}
	c := &buildCall{done: make(chan struct{})}
	m.building[key] = c
	m.buildMu.Unlock()

	start := time.Now()
	st, err := backup.BuildRepoProgress(src, rb, m.RepoDir(key), func(p gitstore.Progress) {
		c.progress.Store(&p)
	})
	m.pool.freeMemory() // indexing a pack allocates a lot, briefly
	if err == nil {
		m.cfg.Logf("unbundled %s: %d objects, %d refs in %s", key, st.Objects, st.Refs, time.Since(start).Round(time.Millisecond))
	}
	c.err = err
	m.buildMu.Lock()
	delete(m.building, key)
	if err != nil {
		m.failed[key] = err.Error()
	} else {
		delete(m.failed, key)
	}
	m.buildMu.Unlock()
	close(c.done)
	return err
}

// Tracker returns the issues/MR store, or nil when none was imported.
func (m *Manager) Tracker() *tracker.Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tracker
}

// Source returns the opened backup source (nil until scanned).
func (m *Manager) Source() backup.Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.src
}

// PausesOnBattery reports whether background work pauses on battery.
func (m *Manager) PausesOnBattery() bool { return m.cfg.PauseOnBattery }

// Expected returns what the database lists of LFS objects and attachments
// (nil when unknown, e.g. an index built by an older version).
func (m *Manager) Expected() *index.Expected {
	e, err := index.ReadExpected(filepath.Join(m.root, "expected.json"))
	if err != nil {
		return nil
	}
	return e
}

// PlannedRefs returns the refs and HEAD branch key will have once
// unbundled, from the bundle headers only.
func (m *Manager) PlannedRefs(key string) ([]gitstore.Ref, string, error) {
	m.mu.RLock()
	ix, src := m.ix, m.src
	m.mu.RUnlock()
	if ix == nil || src == nil {
		return nil, "", errors.New("the backup is still being scanned")
	}
	rb := ix.Repos[key]
	if rb == nil {
		return nil, "", ErrNoBundle
	}
	return backup.PlannedRefs(src, rb)
}
