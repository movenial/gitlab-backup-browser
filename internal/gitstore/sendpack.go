package gitstore

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PackPaths returns the pack files of a built repository.
func (r *Repo) PackPaths() []string {
	var out []string
	for _, name := range r.Packs() {
		out = append(out, filepath.Join(r.Dir, "objects", "pack", name+".pack"))
	}
	return out
}

// WritePacks writes the given pack files to w as one pack, as the smart
// protocol sends it: a single pack is copied as is; several are joined
// under one header and checksum. Entries are copied byte for byte, so
// offset deltas stay valid and nothing is decompressed. An object stored
// in two packs is sent twice, which git accepts.
func WritePacks(w io.Writer, paths []string) (objects uint32, err error) {
	type part struct {
		f    *os.File
		size int64
	}
	var parts []part
	defer func() {
		for _, p := range parts {
			p.f.Close()
		}
	}()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		parts = append(parts, part{f, fi.Size()})
		var hdr [12]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		v := binary.BigEndian.Uint32(hdr[4:8])
		if string(hdr[:4]) != "PACK" || v != 2 && v != 3 || fi.Size() < 32 {
			return 0, fmt.Errorf("%s: not a pack file", path)
		}
		n := binary.BigEndian.Uint32(hdr[8:12])
		if objects+n < objects {
			return 0, errors.New("too many objects")
		}
		objects += n
	}
	if len(parts) == 0 {
		return 0, errors.New("no pack")
	}
	if len(parts) == 1 {
		if _, err := parts[0].f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		_, err := io.Copy(w, parts[0].f)
		return objects, err
	}
	sum := sha1.New()
	out := io.MultiWriter(w, sum)
	var hdr [12]byte
	copy(hdr[:], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], objects)
	if _, err := out.Write(hdr[:]); err != nil {
		return 0, err
	}
	for _, p := range parts {
		// Each pack without its header and checksum.
		if _, err := io.Copy(out, io.NewSectionReader(p.f, 12, p.size-12-20)); err != nil {
			return 0, err
		}
	}
	_, err = w.Write(sum.Sum(nil))
	return objects, err
}
