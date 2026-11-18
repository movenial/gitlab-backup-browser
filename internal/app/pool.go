package app

import (
	"container/list"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

// pool keeps a few repositories open and closes the rest, so browsing
// many repositories in one session does not make memory grow. Every
// repository shares one bounded object cache. A repository is only closed
// once no request is using it (reference counted).
type pool struct {
	mu       sync.Mutex
	items    map[string]*pooled
	lru      *list.List // front = most recently used
	max      int
	idle     time.Duration
	cache    *gitstore.Cache
	lastFree time.Time
	stop     chan struct{}
}

type pooled struct {
	key     string
	repo    *gitstore.Repo
	refs    int
	last    time.Time
	elem    *list.Element
	evicted bool
}

func newPool(max int, idle time.Duration, cacheBytes int) *pool {
	p := &pool{
		items: map[string]*pooled{},
		lru:   list.New(),
		max:   max,
		idle:  idle,
		cache: gitstore.NewCache(cacheBytes),
		stop:  make(chan struct{}),
	}
	go p.janitor()
	return p
}

// acquire returns the open repository for key, opening it with open if
// needed. The caller must call release when done with it.
func (p *pool) acquire(key string, open func(*gitstore.Cache) (*gitstore.Repo, error)) (*gitstore.Repo, func(), error) {
	p.mu.Lock()
	if it, ok := p.items[key]; ok {
		it.refs++
		it.last = time.Now()
		p.lru.MoveToFront(it.elem)
		p.mu.Unlock()
		return it.repo, p.releaser(it), nil
	}
	p.mu.Unlock()

	r, err := open(p.cache)
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	if it, ok := p.items[key]; ok { // another request opened it meanwhile
		it.refs++
		it.last = time.Now()
		p.lru.MoveToFront(it.elem)
		p.mu.Unlock()
		r.Close()
		return it.repo, p.releaser(it), nil
	}
	it := &pooled{key: key, repo: r, refs: 1, last: time.Now()}
	it.elem = p.lru.PushFront(it)
	p.items[key] = it
	closing := p.evictLocked()
	p.mu.Unlock()
	p.closeAll(closing)
	return r, p.releaser(it), nil
}

func (p *pool) releaser(it *pooled) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			it.refs--
			it.last = time.Now()
			var closing []*pooled
			if it.evicted && it.refs == 0 {
				closing = append(closing, it)
			}
			p.mu.Unlock()
			p.closeAll(closing)
		})
	}
}

// evictLocked trims the pool to max entries. Idle entries close now;
// busy ones are detached and close on their last release.
func (p *pool) evictLocked() []*pooled {
	var closing []*pooled
	for p.lru.Len() > p.max {
		var victim *pooled
		for e := p.lru.Back(); e != nil; e = e.Prev() {
			if it := e.Value.(*pooled); it.refs == 0 {
				victim = it
				break
			}
		}
		if victim == nil {
			victim = p.lru.Back().Value.(*pooled)
		}
		p.detachLocked(victim)
		if victim.refs == 0 {
			closing = append(closing, victim)
		}
	}
	return closing
}

func (p *pool) detachLocked(it *pooled) {
	p.lru.Remove(it.elem)
	delete(p.items, it.key)
	it.evicted = true
}

func (p *pool) closeAll(items []*pooled) {
	if len(items) == 0 {
		return
	}
	for _, it := range items {
		it.repo.Close()
	}
	p.freeMemory()
}

// freeMemory returns released memory to the OS, at most every 10s.
func (p *pool) freeMemory() {
	p.mu.Lock()
	if time.Since(p.lastFree) < 10*time.Second {
		p.mu.Unlock()
		return
	}
	p.lastFree = time.Now()
	p.mu.Unlock()
	go debug.FreeOSMemory()
}

// janitor closes repositories nobody used for a while.
func (p *pool) janitor() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.closeIdle(p.idle)
		}
	}
}

func (p *pool) closeIdle(idle time.Duration) {
	p.mu.Lock()
	var closing []*pooled
	for e := p.lru.Back(); e != nil; {
		prev := e.Prev()
		it := e.Value.(*pooled)
		if it.refs == 0 && time.Since(it.last) >= idle {
			p.detachLocked(it)
			closing = append(closing, it)
		}
		e = prev
	}
	p.mu.Unlock()
	p.closeAll(closing)
}

// MemStats is shown on the About page.
type MemStats struct {
	OpenRepos  int
	MaxRepos   int
	CacheBytes int
	CacheMax   int
	HeapInUse  uint64
	FromOS     uint64
}

func (p *pool) stats(cacheMax int) MemStats {
	p.mu.Lock()
	n := len(p.items)
	p.mu.Unlock()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return MemStats{
		OpenRepos: n, MaxRepos: p.max, CacheBytes: p.cache.Size(), CacheMax: cacheMax,
		HeapInUse: ms.HeapInuse, FromOS: ms.Sys - ms.HeapReleased,
	}
}
