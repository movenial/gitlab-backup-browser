package fixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A relative directory works, as with "gbb demo sample".
func TestBuildRelativeDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Chdir(t.TempDir())
	res, err := Build(filepath.Join("sample", "day1"), Gitaly, Tar)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(res.Path) {
		t.Fatalf("backup path %q is relative", res.Path)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatal(err)
	}
}
