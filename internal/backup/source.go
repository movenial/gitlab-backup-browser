// Package backup reads GitLab backup archives (an uncompressed
// *_gitlab_backup.tar, a gzipped tar, or an already extracted directory)
// without restoring them.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is a regular file inside the backup.
type Entry struct {
	Name string // slash-separated, relative to the backup root
	Size int64
}

// Source gives random access to the files of a backup.
type Source interface {
	Entries() []Entry
	Open(name string) (io.ReadCloser, error)
	Has(name string) bool
	Describe() string
	Close() error
}

// cleanName normalises a tar member name; ok is false for names that are
// unsafe (absolute or escaping the root). Backslashes are refused too: on
// Windows they are separators, so "a\..\..\x" would escape the
// extraction directory, and GitLab never writes them.
func cleanName(n string) (string, bool) {
	n = strings.TrimPrefix(n, "./")
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsAny(n, "\\\x00") {
		return "", false
	}
	c := path.Clean(n)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || !filepath.IsLocal(filepath.FromSlash(c)) {
		return "", false
	}
	return c, true
}

// interesting reports whether a backup member is needed by this tool.
func interesting(name string) bool {
	return strings.HasPrefix(name, "repositories/") ||
		strings.HasPrefix(name, "db/") ||
		name == "backup_information.yml" ||
		name == LFSMember || name == UploadsMember
}

// Archives inside a backup holding Git LFS objects and issue/MR attachments.
const (
	LFSMember     = "lfs.tar.gz"
	UploadsMember = "uploads.tar.gz"
)

// Kind classifies a backup path.
type Kind int

const (
	KindTar Kind = iota
	KindTarGz
	KindDir
)

// Detect inspects p and returns its kind.
func Detect(p string) (Kind, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if st.IsDir() {
		return KindDir, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return 0, fmt.Errorf("%s: %w", p, err)
	}
	if magic[0] == 0x1f && magic[1] == 0x8b {
		return KindTarGz, nil
	}
	return KindTar, nil
}

// OpenSource opens an uncompressed tar or a directory. Gzipped tars must
// first be extracted with ExtractTarGz (they cannot be read randomly).
func OpenSource(p string) (Source, error) {
	k, err := Detect(p)
	if err != nil {
		return nil, err
	}
	switch k {
	case KindDir:
		return openDir(p)
	case KindTar:
		return openTar(p)
	default:
		return nil, errors.New("backup: gzipped archive must be extracted first")
	}
}

// --- directory source ---

type dirSource struct {
	root    string
	entries []Entry
	set     map[string]bool
}

func openDir(root string) (*dirSource, error) {
	d := &dirSource{root: root, set: map[string]bool{}}
	err := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if de.IsDir() {
			if rel != "." && !strings.HasPrefix(rel, "repositories") && !strings.HasPrefix(rel, "db") {
				return fs.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() || !interesting(rel) {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		d.entries = append(d.entries, Entry{Name: rel, Size: info.Size()})
		d.set[rel] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (d *dirSource) Entries() []Entry  { return d.entries }
func (d *dirSource) Has(n string) bool { return d.set[n] }
func (d *dirSource) Describe() string  { return "directory " + d.root }
func (d *dirSource) Close() error      { return nil }
func (d *dirSource) Open(n string) (io.ReadCloser, error) {
	if !d.set[n] {
		return nil, fs.ErrNotExist
	}
	return os.Open(filepath.Join(d.root, filepath.FromSlash(n)))
}

// --- uncompressed tar source: members are read in place, never copied ---

type tarMember struct {
	off, size int64
}

type tarSource struct {
	path    string
	f       *os.File
	entries []Entry
	members map[string]tarMember
}

// seekCounter lets archive/tar skip file bodies by seeking while we keep
// track of the absolute position.
type seekCounter struct {
	f   *os.File
	pos int64
}

func (s *seekCounter) Read(p []byte) (int, error) {
	n, err := s.f.Read(p)
	s.pos += int64(n)
	return n, err
}

func (s *seekCounter) Seek(off int64, whence int) (int64, error) {
	n, err := s.f.Seek(off, whence)
	if err == nil {
		s.pos = n
	}
	return n, err
}

func openTar(p string) (*tarSource, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	t := &tarSource{path: p, f: f, members: map[string]tarMember{}}
	sc := &seekCounter{f: f}
	tr := tar.NewReader(sc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("backup: reading %s: %w", p, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := cleanName(h.Name)
		if !ok || !interesting(name) {
			continue
		}
		// archive/tar reads headers exactly, so the body starts here.
		t.members[name] = tarMember{off: sc.pos, size: h.Size}
		t.entries = append(t.entries, Entry{Name: name, Size: h.Size})
	}
	sort.Slice(t.entries, func(i, j int) bool { return t.entries[i].Name < t.entries[j].Name })
	return t, nil
}

func (t *tarSource) Entries() []Entry  { return t.entries }
func (t *tarSource) Has(n string) bool { _, ok := t.members[n]; return ok }
func (t *tarSource) Describe() string  { return "archive " + t.path }
func (t *tarSource) Close() error      { return t.f.Close() }
func (t *tarSource) Open(n string) (io.ReadCloser, error) {
	m, ok := t.members[n]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(io.NewSectionReader(t.f, m.off, m.size)), nil
}

// ExtractTarGz extracts the members this tool needs from a gzipped tar
// into dst. progress, if set, receives compressed bytes read so far.
func ExtractTarGz(src, dst string, progress func(done int64)) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	cr := &countingReader{r: f, cb: progress}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	tmp := dst + ".partial"
	os.RemoveAll(tmp)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := cleanName(h.Name)
		if !ok || !interesting(name) {
			continue
		}
		out := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		w, err := os.Create(out)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, tr)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	os.RemoveAll(dst)
	return os.Rename(tmp, dst)
}

type countingReader struct {
	r    io.Reader
	n    int64
	last int64
	cb   func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.cb != nil && (c.n-c.last > 1<<20 || err != nil) {
		c.last = c.n
		c.cb(c.n)
	}
	return n, err
}
