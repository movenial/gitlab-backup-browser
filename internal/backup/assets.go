package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// AssetProgress reports compressed bytes read out of the member's size.
type AssetProgress func(done, total int64)

// ExtractAssets unpacks the gzipped tar member of the backup into dst,
// keeping only files accepted by keep (which may rename them). Nothing is
// visible in dst until the extraction completed: it is built in
// dst+".partial" and renamed. throttle may block (battery).
func ExtractAssets(src Source, member, dst string, keep func(name string) (string, bool), progress AssetProgress, throttle func()) (files int, bytes int64, err error) {
	var size int64
	for _, e := range src.Entries() {
		if e.Name == member {
			size = e.Size
		}
	}
	rc, err := src.Open(member)
	if err != nil {
		return 0, 0, err
	}
	defer rc.Close()
	cr := &countingReader{r: rc, cb: func(n int64) {
		if progress != nil {
			progress(n, size)
		}
		if throttle != nil {
			throttle()
		}
	}}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", member, err)
	}
	defer gz.Close()
	tmp := dst + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return 0, 0, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(tmp)
			return files, bytes, fmt.Errorf("%s: %w", member, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := cleanName(h.Name)
		if !ok {
			continue
		}
		rel, ok := keep(name)
		if !ok {
			continue
		}
		out := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			os.RemoveAll(tmp)
			return files, bytes, err
		}
		f, err := os.Create(out)
		if err != nil {
			os.RemoveAll(tmp)
			return files, bytes, err
		}
		n, err := io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.RemoveAll(tmp)
			return files, bytes, err
		}
		files++
		bytes += n
	}
	if err := os.RemoveAll(dst); err != nil {
		return files, bytes, err
	}
	return files, bytes, os.Rename(tmp, dst)
}

// KeepLFS accepts GitLab LFS storage paths (…/ab/cd/<60 hex>) and stores
// them as ab/cd/<rest>, the layout LFSPath looks up.
func KeepLFS(name string) (string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) < 3 {
		return "", false
	}
	a, b, rest := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
	oid := a + b + rest
	if len(a) != 2 || len(b) != 2 || len(oid) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(oid); err != nil || strings.ToLower(oid) != oid {
		return "", false
	}
	return path.Join(a, b, rest), true
}

// KeepUpload accepts every attachment path as is.
func KeepUpload(name string) (string, bool) { return name, true }

// LFSPath is where an extracted LFS object lives under dir.
func LFSPath(dir, oid string) string {
	if len(oid) != 64 {
		return ""
	}
	return filepath.Join(dir, oid[:2], oid[2:4], oid[4:])
}
