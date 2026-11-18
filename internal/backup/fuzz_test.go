package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func tarBytes(files map[string]string, extra ...*tar.Header) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	for _, name := range []string{"backup_information.yml", "db/database.sql.gz", "repositories/@hashed/aa/bb/x.bundle", "../evil", "repositories/a\\..\\..\\evil"} {
		if body, ok := files[name]; ok {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
	}
	tw.Close()
	return b.Bytes()
}

func seedTar() []byte {
	return tarBytes(map[string]string{
		"backup_information.yml":              ":gitlab_version: 17.5.1\n",
		"db/database.sql.gz":                  "x",
		"repositories/@hashed/aa/bb/x.bundle": "# v2 git bundle\n\n",
		"../evil":                             "no",
		"repositories/a\\..\\..\\evil":        "no",
	}, &tar.Header{Name: "repositories/", Typeflag: tar.TypeDir, Mode: 0o755})
}

func checkName(t *testing.T, n string) {
	t.Helper()
	if !filepath.IsLocal(filepath.FromSlash(n)) || strings.ContainsRune(n, '\\') || path.Clean(n) != n {
		t.Fatalf("unsafe member name %q accepted", n)
	}
}

// FuzzOpenTar reads tar archives in place: every member found must be
// readable in full, at the size its header declared.
func FuzzOpenTar(f *testing.F) {
	f.Add(seedTar())
	f.Add(seedTar()[:700])
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "b.tar")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		src, err := openTar(p)
		if err != nil {
			return
		}
		defer src.Close()
		for _, e := range src.Entries() {
			checkName(t, e.Name)
			if !src.Has(e.Name) {
				t.Fatalf("entry %q not found", e.Name)
			}
			rc, err := src.Open(e.Name)
			if err != nil {
				t.Fatal(err)
			}
			n, err := io.Copy(io.Discard, rc)
			rc.Close()
			if err != nil || n != e.Size {
				t.Fatalf("%s: read %d of %d bytes: %v", e.Name, n, e.Size, err)
			}
		}
		Layout(src.Entries())
	})
}

// FuzzExtractTarGz checks extraction never writes outside its target.
func FuzzExtractTarGz(f *testing.F) {
	f.Add(seedTar())
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write(data)
		zw.Close()
		p := filepath.Join(dir, "b.tgz")
		os.WriteFile(p, gz.Bytes(), 0o644)
		dst := filepath.Join(dir, "out", "x")
		ExtractTarGz(p, dst, nil)
		filepath.WalkDir(dir, func(q string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || q == p {
				return nil
			}
			rel, _ := filepath.Rel(filepath.Join(dir, "out"), q)
			if !filepath.IsLocal(rel) {
				t.Fatalf("extraction wrote %s", q)
			}
			return nil
		})
	})
}

func FuzzCleanName(f *testing.F) {
	for _, s := range []string{"./a/b", "/abs", "../x", "a/../../x", "a\\..\\..\\x", "a/./b//c", "C:/x", "a\x00b", "."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, n string) {
		if c, ok := cleanName(n); ok {
			checkName(t, c)
			if k, ok := KeepLFS(c); ok {
				checkName(t, k)
			}
		}
	})
}

func FuzzBackupInfo(f *testing.F) {
	f.Add([]byte(":backup_created_at: !ruby/object:ActiveSupport::TimeWithZone\n  utc: 2026-11-03 02:00:30.1 Z\n  zone: x\n:gitlab_version: 19.5.3\n"))
	f.Add([]byte("---\n:a: 'b'\n- x\n# c\n:d:\n  time: \"t Z\"\n  utc:\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		BackupInfo(bytes.NewReader(data))
	})
}

func FuzzLayout(f *testing.F) {
	f.Add("repositories/default/@hashed/aa/bb/h.git/2_b/002.bundle\nrepositories/default/@hashed/aa/bb/h.git/2_b/002.refs\nrepositories/@hashed/cc/dd/x.wiki.bundle\nrepositories/group/project.bundle\nrepositories/.git/x/1.bundle")
	f.Fuzz(func(t *testing.T, names string) {
		var es []Entry
		for i, n := range strings.Split(names, "\n") {
			es = append(es, Entry{Name: n, Size: int64(i)})
		}
		for key, rb := range Layout(es) {
			if len(rb.Steps) == 0 {
				t.Fatalf("%q has no steps", key)
			}
			rb.Kind()
		}
	})
}

// memSource is a Source over in-memory members.
type memSource map[string][]byte

func (m memSource) Entries() []Entry {
	var es []Entry
	for n, b := range m {
		es = append(es, Entry{Name: n, Size: int64(len(b))})
	}
	return es
}
func (m memSource) Has(n string) bool { _, ok := m[n]; return ok }
func (m memSource) Describe() string  { return "memory" }
func (m memSource) Close() error      { return nil }
func (m memSource) Open(n string) (io.ReadCloser, error) {
	b, ok := m[n]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// FuzzChainStart picks the start of an incremental chain from bundle
// headers and .refs files.
func FuzzChainStart(f *testing.F) {
	h := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	f.Add([]byte("# v2 git bundle\n"+h+" refs/heads/main\n\n"), []byte("# v2 git bundle\n-"+h+"\n"+h+" HEAD\n\n"), []byte(h+" refs/heads/main\nref: refs/heads/main HEAD\n"))
	f.Fuzz(func(t *testing.T, b1, b2, refs []byte) {
		const dir = "repositories/@hashed/aa/bb/h.git/1_a/"
		src := memSource{dir + "001.bundle": b1, dir + "002.bundle": b2, dir + "002.refs": refs}
		rb := Layout(src.Entries())["@hashed/aa/bb/h"]
		if rb == nil || len(rb.Steps) != 2 {
			t.Fatalf("layout %+v", rb)
		}
		if start, err := ChainStart(src, rb); err == nil && (start < 0 || start >= len(rb.Steps)) {
			t.Fatalf("start %d", start)
		}
		Refs(src, rb)
	})
}
