package app

import (
	"os"
	"os/exec"
	"path/filepath"
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
