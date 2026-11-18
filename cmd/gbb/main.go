// Command gbb browses and serves a GitLab backup without restoring it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/app"
	"github.com/movenial/gitlab-backup-browser/internal/fixture"
	"github.com/movenial/gitlab-backup-browser/internal/human"
	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/report"
	"github.com/movenial/gitlab-backup-browser/internal/web"
)

var version = "dev"

// defaultMemLimitMiB is the soft memory limit: lower on Android, where the
// system kills apps that grow, even briefly.
var defaultMemLimitMiB = func() int {
	if runtime.GOOS == "android" {
		return 128
	}
	return 256
}()

const usage = `gbb — browse and clone a GitLab backup without restoring it

Usage:
  gbb serve  [flags] <backup>   start the web UI and git server
  gbb verify [flags] <backup>   check that a backup is complete and readable
  gbb list   [flags] <backup>   print the projects in a backup
  gbb demo   <dir>              create a small sample backup (needs git)
  gbb version

<backup> is a *_gitlab_backup.tar, a .tar.gz, or an extracted backup directory.

Run "gbb <command> -h" for the flags of a command.
`

func main() {
	returnMemoryPromptly()
	log.SetFlags(log.Ltime)
	// Phone-friendly default for every command; serve can change it.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(defaultMemLimitMiB) << 20)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "verify":
		// verify reports through its exit status: 0 ok, 1 warnings, 2 errors.
		// Any failure to run the check (missing file, bad arguments...) is
		// an error too, never a mere warning.
		code, verr := verify(args)
		if verr != nil {
			fmt.Fprintln(os.Stderr, "error:", verr)
			code = 2
		}
		os.Exit(code)
	case "list", "ls":
		err = list(args)
	case "demo":
		err = demo(args)
	case "version", "--version", "-v":
		fmt.Println("gbb", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "gitlab-backup-browser")
	}
	return ".gbb-data"
}

func commonFlags(fs *flag.FlagSet) *string {
	return fs.String("data", defaultDataDir(), "directory for the index and unbundled repositories")
}

func oneArg(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return "", errors.New("expected exactly one backup path")
	}
	return fs.Arg(0), nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := commonFlags(fs)
	listen := fs.String("listen", "127.0.0.1:8080", "address to listen on (use :8080 to allow other devices on your network)")
	pause := fs.Bool("pause-on-battery", false, "pause background import while the device is not charging (Termux, Linux laptops)")
	prefetch := fs.Bool("prefetch", false, "unbundle every repository in the background after indexing")
	noIssues := fs.Bool("no-issues", false, "do not import issues, merge requests and comments")
	lfs := &lfsFlag{}
	fs.Var(lfs, "lfs", "extract lfs.tar.gz once to show Git LFS files and serve them to git-lfs: --lfs for all objects (about the archive's size on disk), --lfs=head for the files of each project's default branch only")
	uploads := fs.Bool("uploads", false, "extract uploads.tar.gz once to show images and attachments of issues and merge requests")
	auth := fs.String("auth", "", `require a login, as "user:password" (or set GBB_AUTH)`)
	memLimit := fs.Int("memory-limit", defaultMemLimitMiB, "soft memory limit in MiB, 128 on Android, 256 elsewhere; the garbage collector works harder near it (0 = none)")
	maxOpen := fs.Int("max-open-repos", 6, "repositories kept open at once; others are closed and their memory released")
	cacheMB := fs.Int("cache", 24, "shared object cache in MiB, for all repositories together")
	var dirs []string
	fs.Func("backups", "directory holding GitLab backups to choose from in the browser (repeatable; default: the usual places)", func(v string) error {
		dirs = append(dirs, v)
		return nil
	})
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage: gbb serve [flags] [backup]

Without a backup, the browser lists the backups found in --backups
directories (default: /var/opt/gitlab/backups, the phone's Download folder,
the current directory) and opens the one you pick.`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return errors.New("expected at most one backup path")
	}
	path := fs.Arg(0)
	if len(dirs) == 0 {
		dirs = defaultBackupDirs()
	}
	if *memLimit > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(*memLimit) << 20)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	addr := ln.Addr().(*net.TCPAddr)
	if *auth == "" {
		*auth = os.Getenv("GBB_AUTH")
	}
	hub := app.NewHub(app.Config{
		Backup: path, DataDir: *data, PauseOnBattery: *pause, Prefetch: *prefetch,
		MaxOpenRepos: *maxOpen, CacheBytes: *cacheMB << 20, NoIssues: *noIssues, LFS: lfs.mode != "", LFSMode: lfs.mode, Uploads: *uploads,
	}, dirs)
	srv, err := web.NewHub(hub, version)
	if err != nil {
		return err
	}
	// Buttons that change things (open a backup, extract, delete caches)
	// need a login once the server is reachable from the network.
	srv.ActionsAllowed = *auth != "" || addr.IP.IsLoopback()
	var handler http.Handler = srv
	if *auth != "" {
		c, err := web.ParseCredentials(*auth)
		if err != nil {
			return err
		}
		handler = web.RequireAuth(srv, c)
	}
	if path != "" {
		log.Printf("gbb %s serving %s", version, path)
	} else {
		log.Printf("gbb %s: pick a backup in the browser (looking in %s)", version, strings.Join(hub.Dirs(), ", "))
	}
	for _, u := range urls(addr) {
		log.Printf("  open %s", u)
	}
	if *auth == "" && !addr.IP.IsLoopback() {
		log.Printf("WARNING: reachable from the network without a login; every repository is readable and action buttons are disabled. Use --auth user:password.")
	}
	hs := &http.Server{Handler: logRequests(handler), ReadHeaderTimeout: 10 * time.Second}
	return hs.Serve(ln)
}

// defaultBackupDirs are the usual places for GitLab backups that exist on
// this machine: GitLab's own directory, the phone's Download folder (Termux
// or adb shell) and the current directory.
func defaultBackupDirs() []string {
	cands := []string{"/var/opt/gitlab/backups"}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "storage", "shared", "Download"), filepath.Join(home, "Downloads"))
	}
	cands = append(cands, "/sdcard/Download", ".")
	var out []string
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

func urls(a *net.TCPAddr) []string {
	if !a.IP.IsUnspecified() {
		return []string{fmt.Sprintf("http://%s", a.String())}
	}
	out := []string{fmt.Sprintf("http://localhost:%d", a.Port)}
	ifaces, _ := net.InterfaceAddrs()
	for _, ia := range ifaces {
		if ipn, ok := ia.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, fmt.Sprintf("http://%s:%d", ipn.IP, a.Port))
		}
	}
	return out
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/-/static/") || r.URL.Path == "/-/status.json" {
			return
		}
		log.Printf("%d %s %s (%s)", sw.code, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func loadIndex(path, data string) (*app.Manager, *index.Index, error) {
	m := app.New(app.Config{Backup: path, DataDir: data, Logf: func(string, ...any) {}})
	done := make(chan error, 1)
	go func() { done <- m.Run() }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	for {
		select {
		case err := <-done:
			if last != "" {
				fmt.Fprintln(os.Stderr)
			}
			if err != nil {
				return nil, nil, err
			}
			return m, m.Index(), nil
		case <-tick.C:
			s := m.Status()
			line := s.Phase
			if p := s.Percent(); p >= 0 {
				line += fmt.Sprintf(" %d%%", p)
			}
			if line != last {
				fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
				last = line
			}
		}
	}
}

func list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	data := commonFlags(fs)
	path, err := oneArg(fs, args)
	if err != nil {
		return err
	}
	_, ix, err := loadIndex(path, *data)
	if err != nil {
		return err
	}
	for _, p := range ix.Projects {
		state := "code"
		switch {
		case p.Unmatched:
			state = "unmatched"
		case p.Repo == "":
			state = "no-repo"
		}
		extra := ""
		if p.Wiki != "" {
			extra = " +wiki"
		}
		if p.Archived {
			extra += " archived"
		}
		fmt.Printf("%-9s %s%s\n", state, p.FullPath, extra)
	}
	return nil
}

func verify(args []string) (int, error) {
	started := time.Now()
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	data := commonFlags(fs)
	deep := fs.Bool("deep", false, "fully unbundle and index every repository (slow, catches corrupt packs, detects rewound branches)")
	verbose := fs.Bool("v", false, "with --deep: print time, objects and peak memory of every repository")
	asJSON := fs.Bool("json", false, "print the report as JSON on stdout")
	prom := fs.String("prom", "", "write Prometheus metrics to this file (node_exporter textfile collector)")
	previous := fs.String("previous", "", "an earlier backup to compare with (projects, branches, issues that disappeared)")
	var ignoreRefs []string
	fs.Func("ignore-refs", `with --previous: do not warn about changes of these refs, e.g. 'renovate/*' or 'group/mirror:*' (repeatable)`, func(v string) error {
		ignoreRefs = append(ignoreRefs, v)
		return nil
	})
	maxAge := fs.Duration("max-age", 0, "error when the backup is older than this, e.g. 26h")
	jobs := fs.Int("jobs", 1, "with --deep: repositories restored in parallel (0 = one per CPU); memory grows with it")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage: gbb verify [flags] <backup>

Exit status: 0 ok, 1 warnings only, 2 errors (or bad arguments).`)
		fs.PrintDefaults()
	}
	path, err := oneArg(fs, args)
	if err != nil {
		return 2, err
	}
	var prevSnap *report.Snapshot
	if *previous != "" {
		pm, pix, err := loadIndex(*previous, *data)
		if err != nil {
			return 2, fmt.Errorf("previous backup: %w", err)
		}
		pl := &report.Loaded{Path: *previous, Index: pix, Src: pm.Source(), Tracker: pm.Tracker()}
		prevSnap = report.TakeSnapshot(pl, report.Summarize(pl, time.Now()))
	}
	m, ix, err := loadIndex(path, *data)
	if err != nil {
		return 2, err
	}
	// Restored repositories go under the data directory (not /tmp, which
	// may be RAM), are removed on exit and on Ctrl-C / SIGTERM, and leftovers
	// of a killed run are cleaned up next time.
	tmp, err := verifyTempDir(*data)
	if err != nil {
		return 2, err
	}
	defer os.RemoveAll(tmp)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		// A worker may still be writing a repository: remove, let in-flight
		// writes settle, remove again, then exit.
		os.RemoveAll(tmp)
		time.Sleep(200 * time.Millisecond)
		os.RemoveAll(tmp)
		fmt.Fprintln(os.Stderr, "\ninterrupted")
		os.Exit(2)
	}()
	defer signal.Stop(sigs)
	out := os.Stdout
	if *asJSON {
		out = os.Stderr // keep stdout for the JSON document
	}
	opts := report.Options{
		Started: started, Deep: *deep, Jobs: *jobs, MaxAge: *maxAge, TempDir: tmp, Previous: prevSnap, Sample: sampleHeap,
		IgnoreRefs: ignoreRefs,
		Progress: func(done, total int, name string) {
			if *deep && name != "" {
				fmt.Fprintf(os.Stderr, "\r\033[K[%d/%d] %s", done+1, total, name)
			} else if name == "" && *deep {
				fmt.Fprintf(os.Stderr, "\r\033[K")
			}
		},
	}
	if *verbose {
		opts.OnRepo = func(r report.RepoResult) {
			fmt.Fprintf(os.Stderr, "\r\033[K")
			status := "ok   "
			if r.Error != "" {
				status = "ERROR"
			}
			fmt.Fprintf(out, "%s %-50s %8d objects %6.1fs  peak heap %s\n", status, r.Project, r.Objects, r.Seconds, human.Bytes(int64(r.PeakHeap)))
		}
	}
	rep, _ := report.Run(&report.Loaded{Path: path, Index: ix, Src: m.Source(), Tracker: m.Tracker(), Expected: m.Expected()}, opts)
	if *prom != "" {
		if err := rep.WritePrometheusFile(*prom); err != nil {
			return 2, fmt.Errorf("writing metrics: %w", err)
		}
	}
	if *asJSON {
		rep.WriteJSON(os.Stdout)
	} else {
		rep.WriteText(os.Stdout)
	}
	return rep.ExitCode(), nil
}

// verifyTempDir creates a scratch directory under the data directory and
// removes scratch directories older than an hour (from killed runs).
func verifyTempDir(data string) (string, error) {
	base := filepath.Join(data, "tmp")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	if old, err := filepath.Glob(filepath.Join(base, "verify-*")); err == nil {
		for _, d := range old {
			if fi, err := os.Stat(d); err == nil && time.Since(fi.ModTime()) > time.Hour {
				os.RemoveAll(d)
			}
		}
	}
	return os.MkdirTemp(base, "verify-")
}

func demo(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: gbb demo <dir>")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("gbb demo needs the git command to create sample repositories")
	}
	dirs := []string{filepath.Join(args[0], "day1"), filepath.Join(args[0], "day2")}
	for _, d := range dirs {
		if _, err := os.Stat(d); err == nil {
			return fmt.Errorf("%s already exists", d)
		}
	}
	cleanup := func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}
	day1, err := fixture.Build(dirs[0], fixture.Gitaly, fixture.Tar)
	if err != nil {
		cleanup()
		return err
	}
	day2, err := fixture.BuildNext(dirs[1], fixture.Gitaly, fixture.Tar)
	if err != nil {
		cleanup()
		return err
	}
	fmt.Println("Sample backups written:")
	fmt.Println("  day 1:", day1.Path)
	fmt.Println("  day 2:", day2.Path, "(a project deleted, a branch rewound, a tag deleted, a force-push)")
	fmt.Println("\nTry:")
	fmt.Println("  gbb serve", day1.Path)
	fmt.Println("  gbb verify --deep --previous", day1.Path, day2.Path)
	return nil
}

// sampleHeap records the peak live heap until the returned function is
// called. runtime/metrics is cheap enough to read every 20 ms.
func sampleHeap() (stop func() uint64) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	var peak atomic.Uint64
	read := func() {
		metrics.Read(sample)
		if v := sample[0].Value.Uint64(); v > peak.Load() {
			peak.Store(v)
		}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			read()
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-finished
		read()
		return peak.Load()
	}
}

// lfsFlag is --lfs (all objects), --lfs=all, --lfs=head or --lfs=false.
type lfsFlag struct{ mode string }

func (f *lfsFlag) IsBoolFlag() bool { return true }
func (f *lfsFlag) String() string   { return f.mode }
func (f *lfsFlag) Set(v string) error {
	switch strings.ToLower(v) {
	case "true", "all", "1":
		f.mode = app.LFSAll
	case "head":
		f.mode = app.LFSHead
	case "false", "off", "0", "":
		f.mode = ""
	default:
		return fmt.Errorf("--lfs: want all or head, not %q", v)
	}
	return nil
}
