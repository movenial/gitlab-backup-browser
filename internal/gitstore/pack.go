package gitstore

import (
	"bufio"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const packSigLen = 12 // "PACK" + version + count

// entryHeader describes one object entry inside a packfile.
type entryHeader struct {
	typ      ObjType
	size     int64 // inflated size (of the delta itself for deltas)
	baseOfs  int64 // absolute offset of the base, for ObjOfsDelta
	baseHash Hash  // base id, for ObjRefDelta
	hdrLen   int   // bytes from entry start to zlib stream
}

// byteCounter wraps a bufio.Reader and counts consumed bytes. It satisfies
// flate.Reader so zlib never reads past the end of a stream.
type byteCounter struct {
	r *bufio.Reader
	n int64
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *byteCounter) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func readEntryHeader(r io.ByteReader, off int64) (entryHeader, error) {
	var h entryHeader
	c, err := r.ReadByte()
	if err != nil {
		return h, err
	}
	n := 1
	h.typ = ObjType((c >> 4) & 7)
	size := int64(c & 0x0f)
	shift := uint(4)
	for c&0x80 != 0 {
		if shift > 63-7 { // the next 7 bits would not fit in an int64
			return h, errors.New("gitstore: object size overflow")
		}
		if c, err = r.ReadByte(); err != nil {
			return h, err
		}
		n++
		size |= int64(c&0x7f) << shift
		shift += 7
	}
	h.size = size
	switch h.typ {
	case ObjCommit, ObjTree, ObjBlob, ObjTag:
	case ObjOfsDelta:
		c, err = r.ReadByte()
		if err != nil {
			return h, err
		}
		n++
		rel := int64(c & 0x7f)
		for c&0x80 != 0 {
			if rel > off { // only grows from here: bad, and avoids overflow
				return h, fmt.Errorf("gitstore: bad delta base offset at %d", off)
			}
			if c, err = r.ReadByte(); err != nil {
				return h, err
			}
			n++
			rel = ((rel + 1) << 7) | int64(c&0x7f)
		}
		if rel <= 0 || rel > off {
			return h, fmt.Errorf("gitstore: bad delta base offset at %d", off)
		}
		h.baseOfs = off - rel
	case ObjRefDelta:
		for i := 0; i < 20; i++ {
			if h.baseHash[i], err = r.ReadByte(); err != nil {
				return h, err
			}
		}
		n += 20
	default:
		return h, fmt.Errorf("gitstore: bad object type %d at offset %d", h.typ, off)
	}
	h.hdrLen = n
	return h, nil
}

var zlibPool sync.Pool

// inflateTo decompresses exactly size bytes from r into w and checks the
// zlib stream ends there.
func inflateTo(w io.Writer, r io.Reader, size int64) error {
	var zr io.ReadCloser
	if v := zlibPool.Get(); v != nil {
		zr = v.(io.ReadCloser)
		if err := zr.(zlib.Resetter).Reset(r, nil); err != nil {
			return err
		}
	} else {
		var err error
		if zr, err = zlib.NewReader(r); err != nil {
			return err
		}
	}
	defer zlibPool.Put(zr)
	n, err := io.CopyN(w, zr, size)
	if err != nil {
		return fmt.Errorf("gitstore: inflate: %w (got %d of %d bytes)", err, n, size)
	}
	// Consume the end of the stream (and the adler32 trailer).
	var one [1]byte
	if m, err := zr.Read(one[:]); m != 0 || (err != nil && err != io.EOF) {
		if err == nil {
			err = errors.New("object larger than declared")
		}
		return fmt.Errorf("gitstore: inflate: %w", err)
	}
	return nil
}

// plausibleSize reports whether avail compressed bytes can inflate to
// size bytes: deflate expands at most about 1032:1. It is checked before
// allocating a buffer for a size read from a pack.
func plausibleSize(size, avail int64) bool {
	return size >= 0 && avail > 0 && size/1032 <= avail
}

// inflate decompresses exactly size bytes into a buffer of exactly that
// size (a bytes.Buffer would grow to about twice the object).
func inflate(r io.Reader, size int64) ([]byte, error) {
	buf := sliceWriter{b: make([]byte, 0, size)}
	if err := inflateTo(&buf, r, size); err != nil {
		return nil, err
	}
	return buf.b, nil
}

type sliceWriter struct{ b []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	if len(w.b)+len(p) > cap(w.b) {
		return 0, errors.New("gitstore: object larger than declared")
	}
	w.b = append(w.b, p...)
	return len(p), nil
}

// encodeEntryHeader returns the pack entry header for a non-delta object.
func encodeEntryHeader(t ObjType, size int64) []byte {
	b := make([]byte, 0, 10)
	c := byte(t)<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		b = append(b, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	return append(b, c)
}

func readPackHeader(r io.Reader) (uint32, error) {
	var hdr [packSigLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, fmt.Errorf("gitstore: reading pack header: %w", err)
	}
	if string(hdr[:4]) != "PACK" {
		return 0, errors.New("gitstore: not a packfile")
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return 0, fmt.Errorf("gitstore: unsupported pack version %d", v)
	}
	return binary.BigEndian.Uint32(hdr[8:12]), nil
}
