// Package readahead runs a slow reader (typically gzip decompression) in
// its own goroutine, so the consumer can process one chunk while the next
// is being produced. Memory is bounded: depth+1 chunks of chunkSize bytes.
// On a single core it simply interleaves, at negligible cost.
package readahead

import (
	"io"
	"sync"
)

type chunk struct {
	b   []byte
	err error
}

// Reader reads ahead of its consumer.
type Reader struct {
	ready chan chunk
	free  chan []byte
	stop  chan struct{}
	cur   []byte
	buf   []byte // backing slice of cur, returned to free when consumed
	err   error
	once  sync.Once
	done  chan struct{}
}

// New starts reading r in the background.
func New(r io.Reader, chunkSize, depth int) *Reader {
	if depth < 1 {
		depth = 1
	}
	ra := &Reader{
		ready: make(chan chunk, depth),
		free:  make(chan []byte, depth+1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	for i := 0; i < depth+1; i++ {
		ra.free <- make([]byte, chunkSize)
	}
	go ra.run(r)
	return ra
}

func (ra *Reader) run(r io.Reader) {
	defer close(ra.done)
	defer close(ra.ready)
	for {
		var b []byte
		select {
		case b = <-ra.free:
		case <-ra.stop:
			return
		}
		n, err := io.ReadFull(r, b)
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		if n > 0 || err != nil {
			select {
			case ra.ready <- chunk{b[:n], err}:
			case <-ra.stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Read implements io.Reader.
func (ra *Reader) Read(p []byte) (int, error) {
	for len(ra.cur) == 0 {
		if ra.buf != nil {
			ra.free <- ra.buf[:cap(ra.buf)]
			ra.buf = nil
		}
		if ra.err != nil {
			return 0, ra.err
		}
		c, ok := <-ra.ready
		if !ok {
			ra.err = io.EOF
			return 0, io.EOF
		}
		ra.cur, ra.buf, ra.err = c.b, c.b, c.err
	}
	n := copy(p, ra.cur)
	ra.cur = ra.cur[n:]
	return n, nil
}

// Close stops the background reader and waits for it.
func (ra *Reader) Close() error {
	ra.once.Do(func() { close(ra.stop) })
	<-ra.done
	return nil
}
