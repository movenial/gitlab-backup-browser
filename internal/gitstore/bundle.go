package gitstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Ref is a named reference.
type Ref struct {
	Name string
	Hash Hash
}

// BundleHeader is the parsed text header of a git bundle (v2 or v3).
type BundleHeader struct {
	Version      int
	ObjectFormat string
	Prereqs      []Hash
	Refs         []Ref
}

// maxBundleLine bounds a bundle header line ("<oid> <refname>"): without
// it a damaged bundle with no newline would be read whole into memory.
const maxBundleLine = 64 << 10

// readHeaderLine reads one newline-terminated line of at most
// maxBundleLine bytes.
func readHeaderLine(br *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > maxBundleLine {
			return "", fmt.Errorf("line longer than %d bytes", maxBundleLine)
		}
		line = append(line, chunk...)
		if err != bufio.ErrBufferFull {
			return string(line), err
		}
	}
}

// ReadBundleHeader consumes the bundle header from br, leaving br
// positioned at the start of the packfile.
func ReadBundleHeader(br *bufio.Reader) (*BundleHeader, error) {
	first, err := readHeaderLine(br)
	if err != nil {
		return nil, fmt.Errorf("gitstore: reading bundle signature: %w", err)
	}
	h := &BundleHeader{ObjectFormat: "sha1"}
	switch first {
	case "# v2 git bundle\n":
		h.Version = 2
	case "# v3 git bundle\n":
		h.Version = 3
	default:
		return nil, errors.New("gitstore: not a git bundle")
	}
	for {
		line, err := readHeaderLine(br)
		if err != nil {
			return nil, fmt.Errorf("gitstore: truncated bundle header: %w", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return h, nil
		}
		switch {
		case strings.HasPrefix(line, "@"):
			k, v, _ := strings.Cut(line[1:], "=")
			if k == "object-format" {
				h.ObjectFormat = v
			}
		case strings.HasPrefix(line, "-"):
			id, _, _ := strings.Cut(line[1:], " ")
			p, err := ParseHash(id)
			if err != nil {
				return nil, err
			}
			h.Prereqs = append(h.Prereqs, p)
		default:
			id, name, ok := strings.Cut(line, " ")
			if !ok {
				return nil, fmt.Errorf("gitstore: bad bundle ref line %q", line)
			}
			hh, err := ParseHash(id)
			if err != nil {
				return nil, err
			}
			h.Refs = append(h.Refs, Ref{Name: name, Hash: hh})
		}
	}
}

// BundleInput is one bundle of a backup chain, opened on demand.
type BundleInput struct {
	Name string
	Open func() (io.ReadCloser, error)
}

// RefSet is an authoritative list of refs (e.g. from a GitLab .refs
// file) that replaces the refs accumulated from bundle headers.
type RefSet struct {
	Refs    []Ref
	HeadRef string // symbolic HEAD target if known, e.g. refs/heads/main
}

// ParseRefsFile parses a Gitaly backup ".refs" file: "<oid> <ref>" lines,
// and optionally "ref: <target> HEAD" for a symbolic HEAD.
func ParseRefsFile(r io.Reader) (*RefSet, error) {
	rs := &RefSet{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ref: ") {
			f := strings.Fields(line[5:])
			if len(f) >= 1 && (len(f) == 1 || f[1] == "HEAD") {
				rs.HeadRef = f[0]
			}
			continue
		}
		id, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		h, err := ParseHash(id)
		if err != nil {
			continue
		}
		rs.Refs = append(rs.Refs, Ref{Name: strings.TrimSpace(name), Hash: h})
	}
	return rs, sc.Err()
}

// BuildStats summarises an unbundle.
type BuildStats struct {
	Objects     int
	Packs       int
	Refs        int
	DroppedRefs int // refs whose objects were missing
}

// BuildFromBundles applies bundles in order into a new bare repository at
// dst. The repository is assembled in a temporary directory and renamed
// into place only when complete.
func BuildFromBundles(dst string, bundles []BundleInput, final *RefSet) (*BuildStats, error) {
	return BuildFromBundlesProgress(dst, bundles, final, nil)
}

// Progress describes an unbundle in progress.
type Progress struct {
	Bundle, Bundles int    // 1-based bundle being applied, out of Bundles
	Stage           string // "reading objects", "resolving deltas", "writing refs"
	Done, Total     int
}

// Percent returns the progress of the current stage, or -1.
func (p Progress) Percent() int {
	if p.Total <= 0 {
		return -1
	}
	return p.Done * 100 / p.Total
}

// BuildFromBundlesProgress is BuildFromBundles with a progress callback.
func BuildFromBundlesProgress(dst string, bundles []BundleInput, final *RefSet, prog func(Progress)) (*BuildStats, error) {
	if prog == nil {
		prog = func(Progress) {}
	}
	tmp := dst + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	packDir := filepath.Join(tmp, "objects", "pack")
	for _, d := range []string{packDir, filepath.Join(tmp, "objects", "info"), filepath.Join(tmp, "refs", "heads"), filepath.Join(tmp, "refs", "tags"), filepath.Join(tmp, "info")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	repo, err := Open(tmp)
	if err != nil {
		return nil, err
	}
	defer repo.Close()

	st := &BuildStats{}
	var headers [][]Ref
	for i, b := range bundles {
		bi := i
		n, hdr, err := applyBundle(repo, packDir, b, i, func(stage string, done, total int) {
			prog(Progress{Bundle: bi + 1, Bundles: len(bundles), Stage: stage, Done: done, Total: total})
		})
		if err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("%s: %w", b.Name, err)
		}
		st.Objects += n
		if n > 0 {
			st.Packs++
		}
		headers = append(headers, hdr.Refs)
	}
	planned, headRef := PlanRefs(headers, final)
	var list []Ref
	for _, r := range planned {
		if !repo.Has(r.Hash) {
			st.DroppedRefs++
			continue
		}
		list = append(list, r)
	}
	if err := writeRefs(repo, tmp, list, headRef); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	st.Refs = len(list)
	repo.Close()
	if err := os.RemoveAll(dst); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	return st, nil
}

func applyBundle(repo *Repo, packDir string, b BundleInput, seq int, prog func(string, int, int)) (int, *BundleHeader, error) {
	rc, err := b.Open()
	if err != nil {
		return 0, nil, err
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 256*1024)
	hdr, err := ReadBundleHeader(br)
	if err != nil {
		return 0, nil, err
	}
	if hdr.ObjectFormat != "sha1" {
		return 0, nil, fmt.Errorf("object format %q is not supported (only sha1)", hdr.ObjectFormat)
	}
	for _, p := range hdr.Prereqs {
		if !repo.Has(p) {
			return 0, nil, fmt.Errorf("prerequisite commit %s is missing (incomplete incremental chain?)", p)
		}
	}
	tmpPack := filepath.Join(packDir, fmt.Sprintf("tmp-%d.pack", seq))
	f, err := os.Create(tmpPack)
	if err != nil {
		return 0, nil, err
	}
	n, err := io.Copy(f, br)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpPack)
		return 0, nil, err
	}
	if n == 0 {
		os.Remove(tmpPack)
		return 0, hdr, nil
	}
	pf, err := os.Open(tmpPack)
	if err != nil {
		return 0, nil, err
	}
	count, err := readPackHeader(pf)
	pf.Close()
	if err != nil {
		os.Remove(tmpPack)
		return 0, nil, err
	}
	if count == 0 {
		os.Remove(tmpPack)
		return 0, hdr, nil
	}
	_, objs, err := IndexPackProgress(tmpPack, packDir, repo, prog)
	if err != nil {
		os.Remove(tmpPack)
		return 0, nil, err
	}
	if err := repo.Reload(); err != nil {
		return 0, nil, err
	}
	return objs, hdr, nil
}
