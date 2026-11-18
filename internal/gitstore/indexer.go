package gitstore

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// ObjectSource provides objects that a thin pack may reference but does
// not contain (typically objects from earlier packs of the same repo).
type ObjectSource interface {
	Object(h Hash) (ObjType, []byte, error)
}

type idxEntry struct {
	off      int64
	size     int64
	baseOfs  int64
	hash     Hash
	crc      uint32
	typ      ObjType // resolved type
	rawType  ObjType // type as stored (may be a delta)
	hdrLen   uint8
	resolved bool
}

// IndexPack indexes the packfile at tmpPack and moves it into packDir as
// pack-<checksum>.pack with a matching .idx (v2). Bases missing from a
// thin pack are looked up in ext and appended to the pack, like
// `git index-pack --fix-thin`. It returns the pack checksum and the
// number of objects.
func IndexPack(tmpPack, packDir string, ext ObjectSource) (Hash, int, error) {
	return IndexPackProgress(tmpPack, packDir, ext, nil)
}

// minEntryLen is the size of the smallest possible pack entry: a one-byte
// header and an empty zlib stream (2 bytes of header, 2 of deflate, 4 of
// checksum).
const minEntryLen = 1 + 8

// progressEvery limits how often progress callbacks fire.
const progressEvery = 2000

// IndexPackProgress is IndexPack with a progress callback, called with
// stage "reading objects" then "resolving deltas".
func IndexPackProgress(tmpPack, packDir string, ext ObjectSource, prog func(stage string, done, total int)) (Hash, int, error) {
	if prog == nil {
		prog = func(string, int, int) {}
	}
	f, err := os.OpenFile(tmpPack, os.O_RDWR, 0)
	if err != nil {
		return ZeroHash, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ZeroHash, 0, err
	}
	if st.Size() < packSigLen+20 {
		return ZeroHash, 0, fmt.Errorf("gitstore: pack too small (%d bytes)", st.Size())
	}

	// Pass 1: walk entries sequentially, hash full objects.
	bc := &byteCounter{r: bufio.NewReaderSize(f, 256*1024)}
	count, err := readPackHeader(bc)
	if err != nil {
		return ZeroHash, 0, err
	}
	// The count comes from the pack: check it against the pack size, and
	// let the table grow with the entries actually read.
	if int64(count) > (st.Size()-packSigLen-20)/minEntryLen || count > math.MaxInt32 {
		return ZeroHash, 0, fmt.Errorf("gitstore: pack claims %d objects but has only %d bytes", count, st.Size())
	}
	ents := make([]idxEntry, 0, min(int(count), 1<<16))
	refBases := map[int]Hash{}
	for i := 0; i < int(count); i++ {
		if i%progressEvery == 0 {
			prog("reading objects", i, int(count))
		}
		off := bc.n
		h, err := readEntryHeader(bc, off)
		if err != nil {
			return ZeroHash, 0, fmt.Errorf("gitstore: entry %d at %d: %w", i, off, err)
		}
		ents = append(ents, idxEntry{})
		e := &ents[i]
		e.off, e.size, e.rawType, e.hdrLen = off, h.size, h.typ, uint8(h.hdrLen)
		switch h.typ {
		case ObjOfsDelta:
			e.baseOfs = h.baseOfs
			err = inflateTo(io.Discard, bc, h.size)
		case ObjRefDelta:
			refBases[i] = h.baseHash
			err = inflateTo(io.Discard, bc, h.size)
		default:
			sh := sha1.New()
			sh.Write(objHeader(h.typ, h.size))
			err = inflateTo(sh, bc, h.size)
			sh.Sum(e.hash[:0])
			e.typ, e.resolved = h.typ, true
		}
		if err != nil {
			return ZeroHash, 0, fmt.Errorf("gitstore: entry %d at %d: %w", i, off, err)
		}
	}
	dataEnd := bc.n
	if dataEnd != st.Size()-20 {
		return ZeroHash, 0, fmt.Errorf("gitstore: pack has %d trailing bytes", st.Size()-20-dataEnd)
	}
	sum, err := shaOfRange(f, dataEnd)
	if err != nil {
		return ZeroHash, 0, err
	}
	var trailer Hash
	if _, err := f.ReadAt(trailer[:], dataEnd); err != nil {
		return ZeroHash, 0, err
	}
	if sum != trailer {
		return ZeroHash, 0, fmt.Errorf("gitstore: pack checksum mismatch")
	}

	// Pass 2: resolve deltas from each base outwards. Children of offset
	// deltas are found by binary search in a sorted slice rather than a
	// map: a few bytes per object instead of ~100.
	var ofsKids []ofsKid
	refKids := map[Hash][]int{}
	for i := range ents {
		switch ents[i].rawType {
		case ObjOfsDelta:
			ofsKids = append(ofsKids, ofsKid{base: ents[i].baseOfs, kid: int32(i)})
		case ObjRefDelta:
			refKids[refBases[i]] = append(refKids[refBases[i]], i)
		}
	}
	sort.Slice(ofsKids, func(a, b int) bool { return ofsKids[a].base < ofsKids[b].base })
	for _, k := range ofsKids {
		// Entries are in offset order, so a binary search validates bases.
		j := sort.Search(len(ents), func(n int) bool { return ents[n].off >= k.base })
		if j >= len(ents) || ents[j].off != k.base {
			return ZeroHash, 0, fmt.Errorf("gitstore: delta base offset %d is not an object", k.base)
		}
	}
	nDeltas := len(ofsKids)
	for _, ks := range refKids {
		nDeltas += len(ks)
	}
	r := &resolver{f: f, ents: ents, ofsKids: ofsKids, refKids: refKids, refBase: refBases,
		prog: prog, deltas: nDeltas}
	prog("resolving deltas", 0, nDeltas)
	for i := range ents {
		e := &ents[i]
		if e.rawType == ObjOfsDelta || e.rawType == ObjRefDelta {
			continue
		}
		if !r.hasOfsKids(e.off) && len(refKids[e.hash]) == 0 {
			continue
		}
		data, err := r.raw(i)
		if err != nil {
			return ZeroHash, 0, err
		}
		if err := r.resolveKids(i, e.typ, data, 0); err != nil {
			return ZeroHash, 0, err
		}
	}

	// Thin pack: append missing ref-delta bases.
	var missing []Hash
	for h, kids := range refKids {
		if !ents[kids[0]].resolved {
			missing = append(missing, h)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return bytes.Compare(missing[i][:], missing[j][:]) < 0 })
	if len(missing) > 0 {
		if ext == nil {
			return ZeroHash, 0, fmt.Errorf("gitstore: thin pack needs %d external objects (e.g. %s) but no source", len(missing), missing[0])
		}
		if _, err := f.Seek(dataEnd, io.SeekStart); err != nil {
			return ZeroHash, 0, err
		}
		if err := f.Truncate(dataEnd); err != nil {
			return ZeroHash, 0, err
		}
		w := bufio.NewWriter(f)
		pos := dataEnd
		for _, h := range missing {
			t, data, err := ext.Object(h)
			if err != nil {
				return ZeroHash, 0, fmt.Errorf("gitstore: missing delta base %s: %w", h, err)
			}
			hdr := encodeEntryHeader(t, int64(len(data)))
			var zb bytes.Buffer
			zw, _ := zlib.NewWriterLevel(&zb, zlib.BestSpeed)
			zw.Write(data)
			zw.Close()
			w.Write(hdr)
			w.Write(zb.Bytes())
			r.ents = append(r.ents, idxEntry{off: pos, size: int64(len(data)), hash: h, typ: t, rawType: t, hdrLen: uint8(len(hdr)), resolved: true})
			pos += int64(len(hdr) + zb.Len())
			if err := r.resolveKids(len(r.ents)-1, t, data, 0); err != nil {
				return ZeroHash, 0, err
			}
		}
		if err := w.Flush(); err != nil {
			return ZeroHash, 0, err
		}
		dataEnd = pos
		var cnt [4]byte
		binary.BigEndian.PutUint32(cnt[:], uint32(len(r.ents)))
		if _, err := f.WriteAt(cnt[:], 8); err != nil {
			return ZeroHash, 0, err
		}
	}
	ents = r.ents
	for i := range ents {
		if !ents[i].resolved {
			return ZeroHash, 0, fmt.Errorf("gitstore: unresolved delta at offset %d", ents[i].off)
		}
	}

	// Pass 3: CRCs and (possibly new) checksum.
	if sum, err = crcAndSum(f, ents, dataEnd); err != nil {
		return ZeroHash, 0, err
	}
	if len(missing) > 0 {
		if _, err := f.WriteAt(sum[:], dataEnd); err != nil {
			return ZeroHash, 0, err
		}
		if err := f.Truncate(dataEnd + 20); err != nil {
			return ZeroHash, 0, err
		}
	}
	if err := f.Sync(); err != nil {
		return ZeroHash, 0, err
	}

	base := filepath.Join(packDir, "pack-"+sum.String())
	if err := writeIdx(base+".idx.tmp", ents, sum); err != nil {
		return ZeroHash, 0, err
	}
	if err := os.Rename(tmpPack, base+".pack"); err != nil {
		return ZeroHash, 0, err
	}
	if err := os.Rename(base+".idx.tmp", base+".idx"); err != nil {
		return ZeroHash, 0, err
	}
	return sum, len(ents), nil
}

type ofsKid struct {
	base int64
	kid  int32
}

type resolver struct {
	f       *os.File
	ents    []idxEntry
	ofsKids []ofsKid // sorted by base
	refKids map[Hash][]int
	refBase map[int]Hash // base id of each ref delta
	prog    func(stage string, done, total int)
	deltas  int // total deltas, for progress
	done    int // deltas resolved so far
}

func (r *resolver) tick() {
	r.done++
	if r.done%progressEvery == 0 {
		r.prog("resolving deltas", r.done, r.deltas)
	}
}

func (r *resolver) ofsRange(off int64) []ofsKid {
	lo := sort.Search(len(r.ofsKids), func(n int) bool { return r.ofsKids[n].base >= off })
	hi := lo
	for hi < len(r.ofsKids) && r.ofsKids[hi].base == off {
		hi++
	}
	return r.ofsKids[lo:hi]
}

func (r *resolver) hasOfsKids(off int64) bool { return len(r.ofsRange(off)) > 0 }

// raw returns the inflated payload of entry i (delta data for deltas).
func (r *resolver) raw(i int) ([]byte, error) {
	e := &r.ents[i]
	sr := io.NewSectionReader(r.f, e.off+int64(e.hdrLen), 1<<62)
	return inflate(bufio.NewReaderSize(sr, 32*1024), e.size)
}

// bigBase is the size above which a delta base is dropped and rebuilt from
// the pack later rather than kept in memory while other subtrees resolve.
var bigBase = 8 << 20 // a var so tests can force the rebuild path

// resolveKids resolves the deltas based on object i, whose content is
// data. It keeps memory proportional to a few objects, not to the length
// of delta chains: the base is released before descending into its last
// child, and a large base is re-read instead of being held while other
// children's subtrees are resolved (like git's delta base cache limit).
// Callers must not keep their own reference to data. depth is the length
// of the delta chain leading to i: it recurses once per link, so a
// hostile pack with one long chain must not exhaust the stack.
func (r *resolver) resolveKids(i int, t ObjType, data []byte, depth int) error {
	if depth > maxDeltaDepth {
		return fmt.Errorf("gitstore: delta chain too deep at offset %d", r.ents[i].off)
	}
	var leaves, inner []int
	add := func(k int) {
		if r.hasOfsKids(r.ents[k].off) {
			inner = append(inner, k)
		} else {
			leaves = append(leaves, k)
		}
	}
	for _, k := range r.ofsRange(r.ents[i].off) {
		add(int(k.kid))
	}
	for _, k := range r.refKids[r.ents[i].hash] {
		add(k)
	}
	kids := append(leaves, inner...) // leaves first: they never need data again
	for n, k := range kids {
		if r.ents[k].resolved {
			continue
		}
		if data == nil { // dropped earlier: rebuild it from the pack
			var err error
			if _, data, err = r.materialize(i, 0); err != nil {
				return err
			}
		}
		delta, err := r.raw(k)
		if err != nil {
			return err
		}
		e := &r.ents[k]
		if !r.hasOfsKids(e.off) {
			// Leaf: only its id is needed. Hash the delta's output as it is
			// produced instead of building a copy of the whole object.
			h, err := hashDelta(t, data, delta)
			if err != nil {
				return fmt.Errorf("%w at offset %d", err, e.off)
			}
			e.typ, e.hash, e.resolved = t, h, true
			r.tick()
			if len(r.refKids[h]) == 0 { // nobody refers to it by id either
				if n == len(kids)-1 {
					size := len(data)
					data = nil
					collectIfBig(size)
				}
				continue
			}
			e.resolved = false // a ref delta needs its content after all
		}
		out, err := applyDelta(data, delta)
		delta = nil
		if err != nil {
			return fmt.Errorf("%w at offset %d", err, e.off)
		}
		e.typ, e.hash, e.resolved = t, hashObject(t, out), true
		r.tick()
		if n == len(kids)-1 || (len(data) > bigBase && r.hasOfsKids(e.off)) {
			size := len(data) // last use, or too big to keep across a subtree
			data = nil
			collectIfBig(size)
		}
		if err := r.resolveKids(k, t, out, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// collectIfBig runs the garbage collector right after a large base was
// dropped (the caller must have cleared its reference first): otherwise
// several released versions of a big file pile up before the collector
// runs, and that is what sets the peak.
func collectIfBig(size int) {
	if size > bigBase {
		runtime.GC()
	}
}

// materialize rebuilds the content of entry i by walking its delta chain
// in the pack. Only used for large bases that were released.
func (r *resolver) materialize(i, depth int) (ObjType, []byte, error) {
	if depth > maxDeltaDepth {
		return 0, nil, errors.New("gitstore: delta chain too deep")
	}
	e := &r.ents[i]
	raw, err := r.raw(i)
	if err != nil {
		return 0, nil, err
	}
	var bi int
	switch e.rawType {
	case ObjOfsDelta:
		bi = sort.Search(len(r.ents), func(n int) bool { return r.ents[n].off >= e.baseOfs })
	case ObjRefDelta:
		h, ok := r.refBase[i]
		if !ok {
			return 0, nil, fmt.Errorf("gitstore: unknown base for entry at %d", e.off)
		}
		bi = -1
		for j := range r.ents {
			if r.ents[j].resolved && r.ents[j].hash == h {
				bi = j
				break
			}
		}
		if bi < 0 {
			return 0, nil, fmt.Errorf("gitstore: base %s not found", h)
		}
	default:
		return e.rawType, raw, nil
	}
	t, base, err := r.materialize(bi, depth+1)
	if err != nil {
		return 0, nil, err
	}
	out, err := applyDelta(base, raw)
	return t, out, err
}

func shaOfRange(f *os.File, n int64) (Hash, error) {
	h := sha1.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, n)); err != nil {
		return ZeroHash, err
	}
	var out Hash
	h.Sum(out[:0])
	return out, nil
}

// crcAndSum streams the pack once, computing each entry's CRC32 over its
// raw bytes and the SHA-1 of the whole data section.
func crcAndSum(f *os.File, ents []idxEntry, end int64) (Hash, error) {
	order := make([]int, len(ents))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return ents[order[a]].off < ents[order[b]].off })
	h := sha1.New()
	buf := make([]byte, 256*1024)
	var pos int64
	cur := -1 // index into order of the object containing pos
	for i := range ents {
		ents[i].crc = 0
	}
	for pos < end {
		n := int64(len(buf))
		if end-pos < n {
			n = end - pos
		}
		chunk := buf[:n]
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return ZeroHash, err
		}
		h.Write(chunk)
		for len(chunk) > 0 {
			for cur+1 < len(order) && ents[order[cur+1]].off <= pos {
				cur++
			}
			limit := end
			if cur+1 < len(order) {
				limit = ents[order[cur+1]].off
			}
			take := int64(len(chunk))
			if limit-pos < take {
				take = limit - pos
			}
			if cur >= 0 {
				e := &ents[order[cur]]
				e.crc = crc32.Update(e.crc, crc32.IEEETable, chunk[:take])
			}
			chunk = chunk[take:]
			pos += take
		}
	}
	var out Hash
	h.Sum(out[:0])
	return out, nil
}

func writeIdx(path string, ents []idxEntry, packSum Hash) error {
	order := make([]int, len(ents))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return bytes.Compare(ents[order[a]].hash[:], ents[order[b]].hash[:]) < 0
	})
	for i := 1; i < len(order); i++ {
		if ents[order[i]].hash == ents[order[i-1]].hash {
			return fmt.Errorf("gitstore: duplicate object %s in pack", ents[order[i]].hash)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sh := sha1.New()
	w := bufio.NewWriter(io.MultiWriter(f, sh))
	w.Write([]byte{0xff, 't', 'O', 'c', 0, 0, 0, 2})
	var fan [256]uint32
	for _, i := range order {
		fan[ents[i].hash[0]]++
	}
	var acc uint32
	var b4 [4]byte
	for i := 0; i < 256; i++ {
		acc += fan[i]
		binary.BigEndian.PutUint32(b4[:], acc)
		w.Write(b4[:])
	}
	for _, i := range order {
		w.Write(ents[i].hash[:])
	}
	for _, i := range order {
		binary.BigEndian.PutUint32(b4[:], ents[i].crc)
		w.Write(b4[:])
	}
	var large []int64
	for _, i := range order {
		off := ents[i].off
		if off < 1<<31 {
			binary.BigEndian.PutUint32(b4[:], uint32(off))
		} else {
			binary.BigEndian.PutUint32(b4[:], 0x80000000|uint32(len(large)))
			large = append(large, off)
		}
		w.Write(b4[:])
	}
	var b8 [8]byte
	for _, off := range large {
		binary.BigEndian.PutUint64(b8[:], uint64(off))
		w.Write(b8[:])
	}
	w.Write(packSum[:])
	if err := w.Flush(); err != nil {
		return err
	}
	if _, err := f.Write(sh.Sum(nil)); err != nil {
		return err
	}
	return f.Sync()
}
