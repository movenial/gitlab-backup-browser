package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

func TestPoolBoundsAndRefcount(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o755)
	opened := 0
	p := newPool(2, time.Hour, 1<<20)
	defer close(p.stop)
	open := func(c *gitstore.Cache) (*gitstore.Repo, error) {
		opened++
		return gitstore.OpenWithCache(dir, c)
	}
	count := func() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.items) }

	_, relA, _ := p.acquire("a", open)
	_, relB, _ := p.acquire("b", open)
	relB()
	_, relC, _ := p.acquire("c", open) // over max: idle "b" is closed, busy "a" kept
	if count() != 2 {
		t.Fatalf("pool holds %d repos, want 2", count())
	}
	p.mu.Lock()
	_, hasA := p.items["a"]
	_, hasB := p.items["b"]
	p.mu.Unlock()
	if !hasA || hasB {
		t.Fatalf("expected busy a kept and idle b evicted (a=%v b=%v)", hasA, hasB)
	}
	// Re-acquiring a pooled repo does not reopen it.
	_, relA2, _ := p.acquire("a", open)
	if opened != 3 {
		t.Fatalf("opened %d times, want 3", opened)
	}
	relA()
	relA2()
	relA2() // double release is harmless
	relC()
	p.closeIdle(0)
	if count() != 0 {
		t.Fatalf("idle repos not closed: %d left", count())
	}
}
