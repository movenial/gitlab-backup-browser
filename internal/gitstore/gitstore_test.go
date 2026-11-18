package gitstore

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2024-01-02T03:04:05+0200", "GIT_COMMITTER_DATE=2024-01-02T03:04:05+0200",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// makeHistory builds a repo with deltas, binaries, tags and branches.
func makeHistory(t *testing.T, dir string, commits int, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	var big strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&big, "line %d of a fairly large text file that will produce deltas\n", i)
	}
	text := big.String()
	for i := 0; i < commits; i++ {
		lines := strings.Split(text, "\n")
		for k := 0; k < 20; k++ {
			j := rng.Intn(len(lines))
			lines[j] = fmt.Sprintf("changed %d/%d %x", i, k, rng.Int63())
		}
		text = strings.Join(lines, "\n")
		os.MkdirAll(filepath.Join(dir, "src", "sub"), 0o755)
		os.WriteFile(filepath.Join(dir, "big.txt"), []byte(text), 0o644)
		os.WriteFile(filepath.Join(dir, "src", fmt.Sprintf("f%d.go", i%5)), []byte(fmt.Sprintf("package x // %d\n", i)), 0o644)
		bin := make([]byte, 3000)
		rng.Read(bin)
		os.WriteFile(filepath.Join(dir, "src", "sub", "blob.bin"), bin, 0o644)
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", fmt.Sprintf("commit %d\n\nbody of %d", i, i))
	}
}

func TestBuildFromBundlesMatchesGit(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	makeHistory(t, src, 30, 1)
	git(t, src, "tag", "-a", "v1.0", "-m", "release one")
	git(t, src, "branch", "feature")
	git(t, src, "bundle", "create", "-q", filepath.Join(tmp, "001.bundle"), "--all")
	base := strings.TrimSpace(git(t, src, "rev-parse", "main"))
	makeHistory(t, src, 10, 2)
	git(t, src, "tag", "light")
	git(t, src, "bundle", "create", "-q", filepath.Join(tmp, "002.bundle"), base+"..main", "light")

	open := func(p string) BundleInput {
		return BundleInput{Name: p, Open: func() (io.ReadCloser, error) { return os.Open(p) }}
	}
	dst := filepath.Join(tmp, "out.git")
	st, err := BuildFromBundles(dst, []BundleInput{open(filepath.Join(tmp, "001.bundle")), open(filepath.Join(tmp, "002.bundle"))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Packs != 2 || st.DroppedRefs != 0 {
		t.Fatalf("stats %+v", st)
	}

	// git agrees the repository is complete and consistent.
	git(t, dst, "fsck", "--full", "--strict")

	// Our idx for the (non-thin) first pack is byte-identical to git's.
	packs, _ := filepath.Glob(filepath.Join(dst, "objects", "pack", "*.pack"))
	for _, p := range packs {
		ours, _ := os.ReadFile(strings.TrimSuffix(p, ".pack") + ".idx")
		ref := filepath.Join(tmp, "check.idx")
		git(t, tmp, "index-pack", "-o", ref, p)
		theirs, _ := os.ReadFile(ref)
		if !bytes.Equal(ours, theirs) {
			t.Errorf("idx for %s differs from git index-pack", filepath.Base(p))
		}
		os.Remove(ref)
	}

	// Every object reads back with the right id.
	repo, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	all := git(t, dst, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	n := 0
	for _, id := range strings.Fields(all) {
		h, _ := ParseHash(id)
		typ, data, err := repo.Object(h)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got := hashObject(typ, data); got != h {
			t.Fatalf("object %s read back as %s", id, got)
		}
		if st, size, err := repo.Stat(h); err != nil || st != typ || size != int64(len(data)) {
			t.Fatalf("Stat %s: %v %d %v, want %v %d", id, st, size, err, typ, len(data))
		}
		var buf bytes.Buffer
		if wt, size, _, err := repo.WriteObject(&buf, h); err != nil || wt != typ || size != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("WriteObject %s: %v %v %d", id, err, wt, size)
		}
		n++
	}
	// Thin-pack bases appended to the second pack duplicate objects of the
	// first one (exactly like git index-pack --fix-thin), so st.Objects may
	// exceed the number of distinct objects.
	if n == 0 || st.Objects < n {
		t.Fatalf("objects: git sees %d distinct, we indexed %d", n, st.Objects)
	}

	// Refs, HEAD, peeling and tree lookups.
	refs, head, err := repo.Refs()
	if err != nil || head != "refs/heads/main" {
		t.Fatalf("head %q err %v", head, err)
	}
	names := map[string]RefInfo{}
	for _, r := range refs {
		names[r.Name] = r
	}
	if names["refs/tags/v1.0"].Peeled.IsZero() || names["refs/heads/feature"].Hash.IsZero() || names["refs/tags/light"].Hash.IsZero() {
		t.Fatalf("refs %+v", refs)
	}
	h, _, err := repo.Resolve("main")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Commit(h)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject() != "commit 9" || c.Body() != "body of 9" || c.Author.Name != "Tester" {
		t.Fatalf("commit %+v", c)
	}
	e, err := repo.Lookup(c.Tree, "src/sub/blob.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, data, _ := repo.Object(e.Hash)
	want, _ := os.ReadFile(filepath.Join(src, "src", "sub", "blob.bin"))
	if !bytes.Equal(data, want) {
		t.Fatal("blob content mismatch")
	}
	if _, err := repo.Lookup(c.Tree, "nope/x"); err == nil {
		t.Fatal("expected not found")
	}

	// A real git clone of the result works and matches.
	clone := filepath.Join(tmp, "clone")
	git(t, tmp, "clone", "-q", dst, clone)
	if got := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD")); got != h.String() {
		t.Fatalf("clone HEAD %s want %s", got, h)
	}
}

func TestMissingPrerequisite(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	makeHistory(t, src, 3, 3)
	base := strings.TrimSpace(git(t, src, "rev-parse", "main"))
	makeHistory(t, src, 2, 4)
	git(t, src, "bundle", "create", "-q", filepath.Join(tmp, "inc.bundle"), base+"..main")
	p := filepath.Join(tmp, "inc.bundle")
	_, err := BuildFromBundles(filepath.Join(tmp, "out.git"), []BundleInput{{Name: p, Open: func() (io.ReadCloser, error) { return os.Open(p) }}}, nil)
	if err == nil || !strings.Contains(err.Error(), "prerequisite") {
		t.Fatalf("expected prerequisite error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "out.git")); !os.IsNotExist(err) {
		t.Fatal("partial repo left behind")
	}
}

func TestDelta(t *testing.T) {
	base := []byte("hello world, this is the base")
	// copy 0..5, insert "XYZ", copy 6..
	d := []byte{byte(len(base)), 0}
	tgt := append(append([]byte("hello"), "XYZ"...), base[5:]...)
	d[1] = byte(len(tgt))
	d = append(d, 0x80|0x10, 5)
	d = append(d, 3, 'X', 'Y', 'Z')
	d = append(d, 0x80|0x01|0x10, 5, byte(len(base)-5))
	out, err := applyDelta(base, d)
	if err != nil || !bytes.Equal(out, tgt) {
		t.Fatalf("got %q %v", out, err)
	}
	if _, err := applyDelta(base[:3], d); err == nil {
		t.Fatal("expected size mismatch error")
	}
}

// With a zero threshold every released base is rebuilt from the pack:
// the result must still be byte-identical to git's.
func TestRebuildReleasedBases(t *testing.T) {
	old := bigBase
	bigBase = 0
	defer func() { bigBase = old }()
	TestBuildFromBundlesMatchesGit(t)
}

func TestBuildProgress(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	makeHistory(t, src, 40, 5)
	b := filepath.Join(tmp, "b.bundle")
	git(t, src, "bundle", "create", "-q", b, "--all")
	var events []Progress
	_, err := BuildFromBundlesProgress(filepath.Join(tmp, "out.git"),
		[]BundleInput{{Name: b, Open: func() (io.ReadCloser, error) { return os.Open(b) }}}, nil,
		func(p Progress) { events = append(events, p) })
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]bool{}
	for _, e := range events {
		stages[e.Stage] = true
		if e.Bundles != 1 || e.Bundle != 1 || e.Done > e.Total {
			t.Fatalf("bad event %+v", e)
		}
	}
	if !stages["reading objects"] || !stages["resolving deltas"] {
		t.Fatalf("stages %v", stages)
	}
}

func TestResolveAncestors(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	makeHistory(t, src, 6, 3)
	git(t, src, "checkout", "-q", "-b", "side", "HEAD~2")
	os.WriteFile(filepath.Join(src, "side.txt"), []byte("side\n"), 0o644)
	git(t, src, "add", "side.txt")
	git(t, src, "commit", "-qm", "side")
	git(t, src, "checkout", "-q", "main")
	git(t, src, "merge", "-q", "--no-ff", "-m", "merge", "side")
	git(t, src, "tag", "-a", "t1", "-m", "tag")
	git(t, src, "bundle", "create", "-q", filepath.Join(tmp, "a.bundle"), "--all")
	dst := filepath.Join(tmp, "out.git")
	if _, err := BuildFromBundles(dst, []BundleInput{{Name: "a", Open: func() (io.ReadCloser, error) { return os.Open(filepath.Join(tmp, "a.bundle")) }}}, nil); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, name := range []string{"main~3", "main^", "main^2", "main^2~1", "main~1^1", "t1~2", "main^0", "HEAD~1"} {
		want := strings.TrimSpace(git(t, src, "rev-parse", name+"^{commit}"))
		h, _, err := repo.Resolve(name)
		if err != nil || h.String() != want {
			t.Errorf("%s: %s %v, want %s", name, h, err, want)
		}
	}
	head := strings.TrimSpace(git(t, src, "rev-parse", "main"))
	for _, n := range []int{4, 7, 12, 39} {
		if h, err := repo.ResolvePrefix(head[:n]); err != nil || h.String() != head {
			t.Errorf("prefix %d: %v %s", n, err, h)
		}
		if h, _, err := repo.Resolve(strings.ToUpper(head[:n]) + "~0"); err != nil || h.String() != head {
			t.Errorf("Resolve short %d: %v", n, err)
		}
	}
	if _, err := repo.ResolvePrefix("abc"); err == nil {
		t.Error("3-digit prefix accepted")
	}
	for _, bad := range []string{"main^3", "main~99", "main~1234567", "nope~1", "main~1/README.md", "main~x", "main^{tree}"} {
		if _, _, err := repo.Resolve(bad); err == nil {
			t.Errorf("%s resolved", bad)
		}
	}
}
