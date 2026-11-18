package gitstore

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"container/list"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound is returned when an object is not in the store.
var ErrNotFound = errors.New("gitstore: object not found")

// pack is an opened packfile with its index memory-mapped.
type pack struct {
	name  string
	f     *os.File
	idx   []byte
	unmap func() error
	n     int
	size  int64 // of the .pack file
}

func openPack(base string) (*pack, error) {
	idx, unmap, err := mapFile(base + ".idx")
	if err != nil {
		return nil, err
	}
	if len(idx) < 8+1024+40 || !bytes.Equal(idx[:8], []byte{0xff, 't', 'O', 'c', 0, 0, 0, 2}) {
		unmap()
		return nil, fmt.Errorf("gitstore: %s.idx: unsupported index format", base)
	}
	n := int64(binary.BigEndian.Uint32(idx[8+255*4:]))
	if int64(len(idx)) < 8+1024+n*28+40 {
		unmap()
		return nil, fmt.Errorf("gitstore: %s.idx: truncated", base)
	}
	// find trusts the fan-out table to bound its searches.
	for i, prev := 0, uint32(0); i < 256; i++ {
		v := binary.BigEndian.Uint32(idx[8+i*4:])
		if v < prev {
			unmap()
			return nil, fmt.Errorf("gitstore: %s.idx: corrupt fan-out table", base)
		}
		prev = v
	}
	f, err := os.Open(base + ".pack")
	if err != nil {
		unmap()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		unmap()
		return nil, err
	}
	return &pack{name: filepath.Base(base), f: f, idx: idx, unmap: unmap, n: int(n), size: st.Size()}, nil
}

func (p *pack) close() {
	p.f.Close()
	if p.unmap != nil {
		p.unmap()
	}
	p.idx = nil
}

func (p *pack) hashAt(i int) []byte {
	o := 8 + 1024 + i*20
	return p.idx[o : o+20]
}

func (p *pack) find(h Hash) (int64, bool) {
	lo := 0
	if h[0] > 0 {
		lo = int(binary.BigEndian.Uint32(p.idx[8+int(h[0]-1)*4:]))
	}
	hi := int(binary.BigEndian.Uint32(p.idx[8+int(h[0])*4:]))
	i := lo + sort.Search(hi-lo, func(k int) bool { return bytes.Compare(p.hashAt(lo+k), h[:]) >= 0 })
	if i >= hi || !bytes.Equal(p.hashAt(i), h[:]) {
		return 0, false
	}
	offTab := 8 + 1024 + p.n*24
	v := binary.BigEndian.Uint32(p.idx[offTab+i*4:])
	if v&0x80000000 == 0 {
		return int64(v), true
	}
	large := offTab + p.n*4 + int(v&0x7fffffff)*8
	if large+8 > len(p.idx)-40 {
		return 0, false // corrupt index: no such large offset
	}
	return int64(binary.BigEndian.Uint64(p.idx[large:])), true
}

// Repo is a read-only view over the packs of a bare repository directory.
// A Repo must not be used after Close.
type Repo struct {
	Dir   string
	packs []*pack
	cache *Cache
}

// Open opens the bare repository at dir with a small private cache. A
// directory without packs yields an empty repository.
func Open(dir string) (*Repo, error) {
	return OpenWithCache(dir, NewCache(4<<20))
}

// OpenWithCache opens dir using a cache shared with other repositories,
// so the total memory spent on caching stays bounded.
func OpenWithCache(dir string, c *Cache) (*Repo, error) {
	r := &Repo{Dir: dir, cache: c}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload rescans objects/pack for new packs.
func (r *Repo) Reload() error {
	matches, err := filepath.Glob(filepath.Join(r.Dir, "objects", "pack", "pack-*.idx"))
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, p := range r.packs {
		have[p.name] = true
	}
	for _, m := range matches {
		base := strings.TrimSuffix(m, ".idx")
		if have[filepath.Base(base)] {
			continue
		}
		p, err := openPack(base)
		if err != nil {
			return err
		}
		r.packs = append(r.packs, p)
	}
	return nil
}

// Close releases file handles, index mappings and cached objects.
func (r *Repo) Close() error {
	for _, p := range r.packs {
		r.cache.dropPack(p)
		p.close()
	}
	r.packs = nil
	return nil
}

// Packs returns the pack base names (pack-<sum>).
func (r *Repo) Packs() []string {
	var out []string
	for _, p := range r.packs {
		out = append(out, p.name)
	}
	return out
}

// Has reports whether the object exists.
func (r *Repo) Has(h Hash) bool {
	for _, p := range r.packs {
		if _, ok := p.find(h); ok {
			return true
		}
	}
	return false
}

// Object returns the type and content of object h.
func (r *Repo) Object(h Hash) (ObjType, []byte, error) {
	return r.object(h, 0, nil)
}

// object is Object for a delta base at depth in a delta chain (see readAt
// for seen).
func (r *Repo) object(h Hash, depth int, seen map[cacheKey]bool) (ObjType, []byte, error) {
	for _, p := range r.packs {
		if off, ok := p.find(h); ok {
			return r.readAt(p, off, depth, seen)
		}
	}
	return 0, nil, fmt.Errorf("%w: %s", ErrNotFound, h)
}

const maxDeltaDepth = 10000

var errDeltaCycle = errors.New("gitstore: delta chain loops back on itself")

// readAt reads the object at off, resolving its delta chain. Offset
// deltas only point backwards, but a ref delta names its base by id, so in
// a corrupt pack it can lead back to an entry of its own chain (to itself,
// even). seen records the entries of the chain from its first ref delta
// on, so a cycle stops within two laps; it stays nil until then, and a
// chain of offset deltas allocates nothing for it. Without it, a cycle
// runs to maxDeltaDepth, reading and inflating one entry per step.
func (r *Repo) readAt(p *pack, off int64, depth int, seen map[cacheKey]bool) (ObjType, []byte, error) {
	if depth > maxDeltaDepth {
		return 0, nil, errors.New("gitstore: delta chain too deep")
	}
	k := cacheKey{p, off}
	if seen != nil {
		if seen[k] {
			return 0, nil, fmt.Errorf("%w (%s at %d)", errDeltaCycle, p.name, off)
		}
		seen[k] = true
	}
	if t, d, ok := r.cache.get(p, off); ok {
		return t, d, nil
	}
	br := bufio.NewReaderSize(io.NewSectionReader(p.f, off, 1<<62), 16*1024)
	h, err := readEntryHeader(br, off)
	if err != nil {
		return 0, nil, err
	}
	if !plausibleSize(h.size, p.size-off) {
		return 0, nil, fmt.Errorf("gitstore: %s: object at %d claims %d bytes", p.name, off, h.size)
	}
	data, err := inflate(br, h.size)
	if err != nil {
		return 0, nil, err
	}
	var bt ObjType
	var base []byte
	switch h.typ {
	case ObjOfsDelta:
		bt, base, err = r.readAt(p, h.baseOfs, depth+1, seen)
	case ObjRefDelta:
		if seen == nil {
			seen = map[cacheKey]bool{k: true}
		}
		bt, base, err = r.object(h.baseHash, depth+1, seen)
	default:
		if depth > 0 { // used as a delta base: worth caching
			r.cache.put(p, off, h.typ, data)
		}
		return h.typ, data, nil
	}
	if err != nil {
		return 0, nil, err
	}
	out, err := applyDelta(base, data)
	if err != nil {
		return 0, nil, err
	}
	r.cache.put(p, off, bt, out)
	return bt, out, nil
}

// Cache is an LRU of resolved delta bases, keyed by pack offset, with a
// byte budget. One Cache can be shared by many repositories.
type Cache struct {
	mu    sync.Mutex
	max   int
	size  int
	ll    *list.List
	items map[cacheKey]*list.Element
}

type cacheKey struct {
	p   *pack
	off int64
}

type cacheVal struct {
	k cacheKey
	t ObjType
	d []byte
}

// NewCache returns a cache holding at most max bytes of object data.
func NewCache(max int) *Cache {
	return &Cache{max: max, ll: list.New(), items: map[cacheKey]*list.Element{}}
}

// Size returns the bytes currently cached.
func (c *Cache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// dropPack forgets every entry of p (called when its repo closes).
func (c *Cache) dropPack(p *pack) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.ll.Front(); e != nil; {
		next := e.Next()
		if v := e.Value.(*cacheVal); v.k.p == p {
			c.ll.Remove(e)
			delete(c.items, v.k)
			c.size -= len(v.d)
		}
		e = next
	}
}

func (c *Cache) get(p *pack, off int64) (ObjType, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[cacheKey{p, off}]; ok {
		c.ll.MoveToFront(e)
		v := e.Value.(*cacheVal)
		return v.t, v.d, true
	}
	return 0, nil, false
}

func (c *Cache) put(p *pack, off int64, t ObjType, d []byte) {
	if len(d) > c.max/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey{p, off}
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = c.ll.PushFront(&cacheVal{k, t, d})
	c.size += len(d)
	for c.size > c.max {
		e := c.ll.Back()
		v := e.Value.(*cacheVal)
		c.ll.Remove(e)
		delete(c.items, v.k)
		c.size -= len(v.d)
	}
}

// Stat returns an object's type and size without inflating it: only entry
// headers are read, plus the first bytes of a delta (which encode the
// result size). It lets callers skip large blobs cheaply.
func (r *Repo) Stat(h Hash) (ObjType, int64, error) {
	for _, p := range r.packs {
		if off, ok := p.find(h); ok {
			return r.statAt(p, off)
		}
	}
	return 0, 0, fmt.Errorf("%w: %s", ErrNotFound, h)
}

func (r *Repo) statAt(p *pack, off int64) (ObjType, int64, error) {
	size := int64(-1)
	var seen map[cacheKey]bool // as in readAt
	for depth := 0; depth <= maxDeltaDepth; depth++ {
		k := cacheKey{p, off}
		if seen != nil {
			if seen[k] {
				return 0, 0, fmt.Errorf("%w (%s at %d)", errDeltaCycle, p.name, off)
			}
			seen[k] = true
		}
		br := bufio.NewReaderSize(io.NewSectionReader(p.f, off, 1<<62), 512)
		hd, err := readEntryHeader(br, off)
		if err != nil {
			return 0, 0, err
		}
		switch hd.typ {
		case ObjOfsDelta, ObjRefDelta:
			if size < 0 {
				if size, err = deltaResultSize(br); err != nil {
					return 0, 0, err
				}
			}
			if hd.typ == ObjOfsDelta {
				off = hd.baseOfs
				continue
			}
			// Ref delta: find its base, possibly in another pack.
			if seen == nil {
				seen = map[cacheKey]bool{k: true}
			}
			found := false
			for _, q := range r.packs {
				if o, ok := q.find(hd.baseHash); ok {
					p, off, found = q, o, true
					break
				}
			}
			if !found {
				return 0, 0, fmt.Errorf("%w: delta base %s", ErrNotFound, hd.baseHash)
			}
		default:
			if size < 0 {
				size = hd.size
			}
			return hd.typ, size, nil
		}
	}
	return 0, 0, errors.New("gitstore: delta chain too deep")
}

// deltaResultSize inflates just enough of a delta to read its header.
func deltaResultSize(r io.Reader) (int64, error) {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	var buf [20]byte
	n, err := io.ReadAtLeast(zr, buf[:], 2)
	if err != nil && n < 2 {
		return 0, err
	}
	b := buf[:n]
	_, k := deltaVarint(b) // source size
	if k <= 0 {
		return 0, errBadDelta
	}
	dst, k2 := deltaVarint(b[k:])
	if k2 <= 0 || int64(dst) < 0 {
		return 0, errBadDelta
	}
	return int64(dst), nil
}

// WriteObject writes the content of object h to w. Objects stored whole
// in the pack are inflated straight into w, in constant memory; deltas
// have to be rebuilt in memory first (materialized is then true, so the
// caller may give that memory back to the OS).
func (r *Repo) WriteObject(w io.Writer, h Hash) (t ObjType, size int64, materialized bool, err error) {
	for _, p := range r.packs {
		off, ok := p.find(h)
		if !ok {
			continue
		}
		if t, d, ok := r.cache.get(p, off); ok {
			_, err := w.Write(d)
			return t, int64(len(d)), false, err
		}
		br := bufio.NewReaderSize(io.NewSectionReader(p.f, off, 1<<62), 16*1024)
		eh, err := readEntryHeader(br, off)
		if err != nil {
			return 0, 0, false, err
		}
		if eh.typ == ObjOfsDelta || eh.typ == ObjRefDelta {
			t, d, err := r.readAt(p, off, 0, nil)
			if err != nil {
				return 0, 0, false, err
			}
			_, err = w.Write(d)
			return t, int64(len(d)), true, err
		}
		if !plausibleSize(eh.size, p.size-off) {
			return 0, 0, false, fmt.Errorf("gitstore: %s: object at %d claims %d bytes", p.name, off, eh.size)
		}
		return eh.typ, eh.size, false, inflateTo(w, br, eh.size)
	}
	return 0, 0, false, fmt.Errorf("%w: %s", ErrNotFound, h)
}

// ErrAmbiguous: a short object id matches several objects.
var ErrAmbiguous = errors.New("gitstore: ambiguous short id")

// ResolvePrefix finds the object whose id starts with the hex prefix (4
// to 40 digits, as in "e6f7e7da").
func (r *Repo) ResolvePrefix(prefix string) (Hash, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) < 4 || len(prefix) > 40 {
		return ZeroHash, fmt.Errorf("%w: %q", ErrNotFound, prefix)
	}
	// Lowest id with this prefix: pad with zeros.
	low, err := hex.DecodeString((prefix + strings.Repeat("0", 40))[:40])
	if err != nil {
		return ZeroHash, fmt.Errorf("%w: %q", ErrNotFound, prefix)
	}
	var found Hash
	n := 0
	for _, p := range r.packs {
		lo := 0
		if low[0] > 0 {
			lo = int(binary.BigEndian.Uint32(p.idx[8+int(low[0]-1)*4:]))
		}
		hi := int(binary.BigEndian.Uint32(p.idx[8+int(low[0])*4:]))
		for i := lo + sort.Search(hi-lo, func(k int) bool { return bytes.Compare(p.hashAt(lo+k), low) >= 0 }); i < hi; i++ {
			var h Hash
			copy(h[:], p.hashAt(i))
			if !strings.HasPrefix(h.String(), prefix) {
				break
			}
			if n == 0 || h != found { // the same object may be in several packs
				found = h
				n++
			}
			if n > 1 {
				return ZeroHash, fmt.Errorf("%w: %q", ErrAmbiguous, prefix)
			}
		}
	}
	if n == 0 {
		return ZeroHash, fmt.Errorf("%w: %q", ErrNotFound, prefix)
	}
	return found, nil
}
