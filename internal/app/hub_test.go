package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/fixture"
)

// Switching backups while one is still importing must stop the old import
// cleanly (no cached half index) and leave the new one usable.
func TestHubRapidSwitch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "backups")
	os.MkdirAll(dir, 0o755)
	var paths []string
	for i, build := range []func(string, fixture.Layout, fixture.Format) (*fixture.Result, error){fixture.Build, fixture.BuildNext} {
		res, err := build(filepath.Join(tmp, "b", string(rune('a'+i))), fixture.Gitaly, fixture.Tar)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, filepath.Base(res.Path))
		os.Rename(res.Path, p)
		paths = append(paths, p)
	}
	h := NewHub(Config{DataDir: filepath.Join(tmp, "data"), Logf: t.Logf}, []string{dir})
	if len(h.Scan()) != 2 {
		t.Fatalf("scan: %+v", h.Scan())
	}
	for i := 0; i < 10; i++ {
		if err := h.Open(paths[i%2]); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Open("/etc/passwd"); err != ErrNotAllowed {
		t.Fatalf("outside path: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !h.Active().Status().Ready {
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %+v", h.Active().Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.Active().BackupPath() != paths[1] || h.Active().Index().DBStatus != "ok" {
		t.Fatalf("active %s, db %s", h.Active().BackupPath(), h.Active().Index().DBStatus)
	}
}

// The cache holds the content of private repositories: other users of the
// machine must not be able to enter it, even when it was created open.
func TestCacheIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	res, err := fixture.Build(filepath.Join(tmp, "b"), fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(tmp, "data")
	root, err := CacheRoot(data, res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewHub(Config{DataDir: data, Logf: t.Logf}, []string{filepath.Dir(res.Path)})
	if err := h.Open(res.Path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !h.Active().Status().Ready {
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %+v", h.Active().Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.Active().Root() != root {
		t.Fatalf("cache in %s, expected %s", h.Active().Root(), root)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory is %o, want 700", fi.Mode().Perm())
	}
}
