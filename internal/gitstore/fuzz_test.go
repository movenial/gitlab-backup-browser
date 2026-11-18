package gitstore

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fuzzAllocLimit is the most a single fuzz input may make the parsers
// allocate. Inputs are at most a few hundred KiB, so anything near this
// means a size read from the input was trusted.
const fuzzAllocLimit = 256 << 20

func checkAlloc(t *testing.T, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > fuzzAllocLimit {
		t.Fatalf("allocated %d MiB", n>>20)
	}
}

// --- a tiny pack writer for seeds ---

type packObj struct {
	typ  ObjType
	data []byte // content, or delta data for deltas
	base int    // index of the base entry, for deltas
}

var seedZW = zlib.NewWriter(nil)

func zlibBytes(b []byte) []byte {
	var zb bytes.Buffer
	seedZW.Reset(&zb)
	seedZW.Write(b)
	seedZW.Close()
	return zb.Bytes()
}

// buildPack returns a pack (without its trailer) holding objs.
func buildPack(objs []packObj) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	binary.Write(&b, binary.BigEndian, uint32(2))
	binary.Write(&b, binary.BigEndian, uint32(len(objs)))
	offs := make([]int64, len(objs))
	content := make([][]byte, len(objs))
	types := make([]ObjType, len(objs))
	for i, o := range objs {
		offs[i] = int64(b.Len())
		hdr := encodeEntryHeader(o.typ, int64(len(o.data)))
		b.Write(hdr)
		switch o.typ {
		case ObjOfsDelta:
			rel := offs[i] - offs[o.base]
			var enc []byte
			enc = append(enc, byte(rel&0x7f))
			for rel >>= 7; rel > 0; rel >>= 7 {
				rel--
				enc = append([]byte{0x80 | byte(rel&0x7f)}, enc...)
			}
			b.Write(enc)
		case ObjRefDelta:
			h := hashObject(types[o.base], content[o.base])
			b.Write(h[:])
		}
		b.Write(zlibBytes(o.data))
		if o.typ == ObjOfsDelta || o.typ == ObjRefDelta {
			content[i], _ = applyDelta(content[o.base], o.data)
			types[i] = types[o.base]
		} else {
			content[i], types[i] = o.data, o.typ
		}
	}
	return b.Bytes()
}

func varint(n int) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// makeDelta turns base into target with one copy and one insert.
func makeDelta(base []byte, keep int, insert string) []byte {
	d := append(varint(len(base)), varint(keep+len(insert))...)
	d = append(d, 0x80|0x10|0x20, byte(keep), byte(keep>>8))
	return append(append(d, byte(len(insert))), insert...)
}

var (
	seedBlob   = []byte(strings.Repeat("some file content\n", 20))
	seedTree   = append([]byte("100644 a.txt\x00"), bytes.Repeat([]byte{0xab}, 20)...)
	seedCommit = []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nparent 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author A U Thor <a@example.com> 1700000000 +0200\ncommitter C <c@example.com> 1700000000 -0130\n" +
		"gpgsig -----BEGIN-----\n xyz\n -----END-----\n\nsubject\n\nbody\n")
	seedTag = []byte("object 4b825dc642cb6eb9a060e54bf8d69288fbee4904\ntype commit\ntag v1\ntagger T <t@x> 1 +0000\n\nmsg\n")
)

func seedPacks() [][]byte {
	blob := packObj{typ: ObjBlob, data: seedBlob}
	return [][]byte{
		buildPack([]packObj{blob}),
		buildPack([]packObj{
			blob,
			{typ: ObjTree, data: seedTree},
			{typ: ObjCommit, data: seedCommit},
			{typ: ObjTag, data: seedTag},
			{typ: ObjOfsDelta, base: 0, data: makeDelta(seedBlob, 100, "tail")},
			{typ: ObjOfsDelta, base: 4, data: makeDelta(append(seedBlob[:100:100], "tail"...), 50, "x")},
			{typ: ObjRefDelta, base: 0, data: makeDelta(seedBlob, 10, "ref")},
		}),
	}
}

// extSource is the "earlier packs" of a thin pack.
type extSource map[Hash]packObj

func (e extSource) Object(h Hash) (ObjType, []byte, error) {
	if o, ok := e[h]; ok {
		return o.typ, o.data, nil
	}
	return 0, nil, fmt.Errorf("%w: %s", ErrNotFound, h)
}

func thinSeed() ([]byte, extSource) {
	// A ref delta whose base lives outside the pack.
	ext := extSource{hashObject(ObjBlob, seedBlob): {typ: ObjBlob, data: seedBlob}}
	h := hashObject(ObjBlob, seedBlob)
	d := makeDelta(seedBlob, 30, "thin")
	var b bytes.Buffer
	b.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
	b.Write(encodeEntryHeader(ObjRefDelta, int64(len(d))))
	b.Write(h[:])
	b.Write(zlibBytes(d))
	return b.Bytes(), ext
}

// FuzzIndexPack feeds packs (the trailer is computed by the harness so
// that mutations reach delta resolution) to the indexer and reads every
// indexed object back.
func FuzzIndexPack(f *testing.F) {
	for _, p := range seedPacks() {
		f.Add(p)
	}
	thin, ext := thinSeed()
	f.Add(thin)
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		packDir := filepath.Join(dir, "objects", "pack")
		os.MkdirAll(packDir, 0o755)
		sum := sha1.Sum(data)
		tmp := filepath.Join(packDir, "tmp.pack")
		if err := os.WriteFile(tmp, append(data[:len(data):len(data)], sum[:]...), 0o644); err != nil {
			t.Fatal(err)
		}
		var n int
		var err error
		checkAlloc(t, func() { _, n, err = IndexPack(tmp, packDir, ext) })
		if err != nil {
			return
		}
		repo, err := Open(dir)
		if err != nil {
			t.Fatalf("indexed pack does not open: %v", err)
		}
		defer repo.Close()
		p := repo.packs[0]
		if p.n != n {
			t.Fatalf("idx has %d objects, indexer said %d", p.n, n)
		}
		for i := 0; i < p.n; i++ {
			var h Hash
			copy(h[:], p.hashAt(i))
			typ, obj, err := repo.Object(h)
			if err != nil {
				t.Fatalf("object %s: %v", h, err)
			}
			if got := hashObject(typ, obj); got != h {
				t.Fatalf("object %s reads back as %s", h, got)
			}
			if st, size, err := repo.Stat(h); err != nil || st != typ || size != int64(len(obj)) {
				t.Fatalf("Stat %s: %v %d %v", h, st, size, err)
			}
		}
	})
}

// FuzzReadPack reads objects from a pack and idx given as is, the way a
// built repository is read (no indexing, no checksum checks).
func FuzzReadPack(f *testing.F) {
	for _, p := range seedPacks() {
		dir := f.TempDir()
		packDir := filepath.Join(dir, "objects", "pack")
		os.MkdirAll(packDir, 0o755)
		sum := sha1.Sum(p)
		tmp := filepath.Join(packDir, "tmp.pack")
		os.WriteFile(tmp, append(p, sum[:]...), 0o644)
		psum, _, err := IndexPack(tmp, packDir, nil)
		if err != nil {
			f.Fatal(err)
		}
		base := filepath.Join(packDir, "pack-"+psum.String())
		idx, _ := os.ReadFile(base + ".idx")
		pack, _ := os.ReadFile(base + ".pack")
		f.Add(idx, pack)
	}
	f.Fuzz(func(t *testing.T, idx, pack []byte) {
		dir := t.TempDir()
		packDir := filepath.Join(dir, "objects", "pack")
		os.MkdirAll(packDir, 0o755)
		os.WriteFile(filepath.Join(packDir, "pack-x.idx"), idx, 0o644)
		os.WriteFile(filepath.Join(packDir, "pack-x.pack"), pack, 0o644)
		repo, err := Open(dir)
		if err != nil {
			return
		}
		defer repo.Close()
		hashes := []Hash{ZeroHash, {0xff}}
		for i := 0; i < repo.packs[0].n && i < 64; i++ {
			var h Hash
			copy(h[:], repo.packs[0].hashAt(i))
			hashes = append(hashes, h)
		}
		checkAlloc(t, func() {
			for _, h := range hashes {
				repo.Has(h)
				repo.Object(h)
				repo.Stat(h)
				repo.Commit(h)
				repo.Tree(h)
			}
		})
	})
}

// FuzzDelta checks applyDelta and hashDelta agree and never trust the
// sizes a delta declares.
func FuzzDelta(f *testing.F) {
	f.Add(seedBlob, makeDelta(seedBlob, 100, "tail"))
	f.Add([]byte("hello world"), []byte{11, 3, 0x91, 0, 3})
	f.Add([]byte{}, []byte{0, 0})
	f.Add([]byte("x"), []byte{1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x80})
	f.Fuzz(func(t *testing.T, base, delta []byte) {
		var out []byte
		var err error
		checkAlloc(t, func() { out, err = applyDelta(base, delta) })
		for _, typ := range []ObjType{ObjBlob} {
			h, herr := hashDelta(typ, base, delta)
			if (err == nil) != (herr == nil) {
				t.Fatalf("applyDelta err %v, hashDelta err %v", err, herr)
			}
			if err == nil && h != hashObject(typ, out) {
				t.Fatal("hashDelta disagrees with applyDelta")
			}
		}
	})
}

func FuzzBundleHeader(f *testing.F) {
	h := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	f.Add([]byte("# v2 git bundle\n-" + h + " prereq\n" + h + " refs/heads/main\n" + h + " HEAD\n\nPACK"))
	f.Add([]byte("# v3 git bundle\n@object-format=sha1\n@filter=blob:none\n" + h + " refs/tags/v1\n\n"))
	f.Add([]byte("# v3 git bundle\n@object-format=sha256\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkAlloc(t, func() {
			hdr, err := ReadBundleHeader(bufio.NewReaderSize(bytes.NewReader(data), 16))
			if err == nil && len(hdr.Refs)+len(hdr.Prereqs) > len(data)/41 {
				t.Fatalf("%d refs from %d bytes", len(hdr.Refs), len(data))
			}
			rs, err := ParseRefsFile(bytes.NewReader(data))
			if err == nil && len(rs.Refs) > len(data)/41 {
				t.Fatalf("%d refs from %d bytes", len(rs.Refs), len(data))
			}
			guessHead(rs.Refs)
		})
	})
}

func FuzzParseObjects(f *testing.F) {
	f.Add(seedCommit)
	f.Add(seedTag)
	f.Add(seedTree)
	f.Add([]byte("author x <y> 99999999999999999999 +9999\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if c, err := ParseCommit(ZeroHash, data); err == nil {
			c.Subject()
			c.Body()
		}
		ParseTag(ZeroHash, data)
		if es, err := ParseTree(data); err == nil {
			SortTreeForDisplay(es)
			for _, e := range es {
				e.IsDir()
			}
		}
	})
}

func indexBytes(t *testing.T, pack []byte) error {
	t.Helper()
	dir := t.TempDir()
	sum := sha1.Sum(pack)
	tmp := filepath.Join(dir, "tmp.pack")
	if err := os.WriteFile(tmp, append(pack, sum[:]...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := IndexPack(tmp, dir, nil)
	return err
}

func TestPackClaimsTooManyObjects(t *testing.T) {
	err := indexBytes(t, []byte("PACK\x00\x00\x00\x02\xff\xff\xff\xff"))
	if err == nil || !strings.Contains(err.Error(), "claims") {
		t.Fatalf("got %v", err)
	}
}

func TestDeltaChainTooDeep(t *testing.T) {
	// A chain of offset deltas, each rewriting the previous object.
	objs := []packObj{{typ: ObjBlob, data: []byte("0")}}
	prev := []byte("0")
	for i := 1; i <= maxDeltaDepth+2; i++ {
		next := []byte(fmt.Sprint(i))
		d := append(append(varint(len(prev)), varint(len(next))...), byte(len(next)))
		objs = append(objs, packObj{typ: ObjOfsDelta, base: i - 1, data: append(d, next...)})
		prev = next
	}
	err := indexBytes(t, buildPack(objs))
	if err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("got %v", err)
	}
}

func TestDeltaResultSizeNotTrusted(t *testing.T) {
	// A one-byte delta result declared as 2^62 bytes.
	d := append(varint(1), 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x40, 1, 'x')
	if _, err := applyDelta([]byte("a"), d); err == nil {
		t.Fatal("expected error")
	}
}

// FuzzIndexPackEntries builds a pack from raw entry headers and
// uncompressed payloads, which the harness compresses: mutations then
// reach object headers, delta bases and delta ops instead of breaking
// zlib streams. Input: repeated [header length][header][payload length,
// 2 bytes][payload].
func FuzzIndexPackEntries(f *testing.F) {
	enc := func(objs []packObj) []byte {
		var out []byte
		offs := []int{}
		pos := packSigLen
		for _, o := range objs {
			offs = append(offs, pos)
			hdr := encodeEntryHeader(o.typ, int64(len(o.data)))
			switch o.typ {
			case ObjOfsDelta:
				rel := int64(pos - offs[o.base])
				e := []byte{byte(rel & 0x7f)}
				for rel >>= 7; rel > 0; rel >>= 7 {
					rel--
					e = append([]byte{0x80 | byte(rel&0x7f)}, e...)
				}
				hdr = append(hdr, e...)
			case ObjRefDelta:
				h := hashObject(ObjBlob, objs[o.base].data)
				hdr = append(hdr, h[:]...)
			}
			out = append(out, byte(len(hdr)))
			out = append(out, hdr...)
			out = append(out, byte(len(o.data)>>8), byte(len(o.data)))
			out = append(out, o.data...)
			pos += len(hdr) + len(zlibBytes(o.data))
		}
		return out
	}
	blob := packObj{typ: ObjBlob, data: seedBlob}
	f.Add(enc([]packObj{
		blob,
		{typ: ObjOfsDelta, base: 0, data: makeDelta(seedBlob, 100, "tail")},
		{typ: ObjRefDelta, base: 0, data: makeDelta(seedBlob, 10, "ref")},
		{typ: ObjTree, data: seedTree},
	}))
	_, ext := thinSeed()
	f.Fuzz(func(t *testing.T, data []byte) {
		var b bytes.Buffer
		b.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x00")
		n := 0
		for len(data) > 0 {
			hl := int(data[0])
			data = data[1:]
			if hl == 0 || len(data) < hl+2 {
				break
			}
			b.Write(data[:hl])
			pl := int(data[hl])<<8 | int(data[hl+1])
			data = data[hl+2:]
			pl = min(pl, len(data))
			b.Write(zlibBytes(data[:pl]))
			data = data[pl:]
			n++
		}
		pk := b.Bytes()
		binary.BigEndian.PutUint32(pk[8:], uint32(n))
		dir := t.TempDir()
		sum := sha1.Sum(pk)
		tmp := filepath.Join(dir, "tmp.pack")
		os.WriteFile(tmp, append(pk, sum[:]...), 0o644)
		var err error
		checkAlloc(t, func() { _, _, err = IndexPack(tmp, dir, ext) })
		if err == nil {
			repo := &Repo{Dir: dir, cache: NewCache(1 << 20)}
			ps, _ := filepath.Glob(filepath.Join(dir, "pack-*.idx"))
			p, err := openPack(strings.TrimSuffix(ps[0], ".idx"))
			if err != nil {
				t.Fatal(err)
			}
			repo.packs = []*pack{p}
			defer repo.Close()
			for i := 0; i < p.n; i++ {
				var h Hash
				copy(h[:], p.hashAt(i))
				typ, obj, err := repo.Object(h)
				if err != nil || hashObject(typ, obj) != h {
					t.Fatalf("object %s: %v", h, err)
				}
			}
		}
	})
}

func TestEntryHeaderOverflow(t *testing.T) {
	// A blob whose size varint runs to 67 bits used to wrap negative.
	b := []byte{0xbf, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}
	if h, err := readEntryHeader(bufio.NewReader(bytes.NewReader(b)), 0); err == nil {
		t.Fatalf("accepted size %d", h.size)
	}
	// An offset delta whose base distance never ends.
	b = append([]byte{0x60}, bytes.Repeat([]byte{0xff}, 64)...)
	if _, err := readEntryHeader(bufio.NewReader(bytes.NewReader(b)), 1000); err == nil {
		t.Fatal("accepted endless base offset")
	}
}
