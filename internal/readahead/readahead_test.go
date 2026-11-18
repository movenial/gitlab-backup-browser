package readahead

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func TestCopiesExactly(t *testing.T) {
	data := make([]byte, 1<<20+123)
	rand.New(rand.NewSource(1)).Read(data)
	for _, cs := range []int{1, 7, 4096, 1 << 20, 4 << 20} {
		ra := New(bytes.NewReader(data), cs, 3)
		got, err := io.ReadAll(ra)
		ra.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("chunk %d: %v, %d bytes", cs, err, len(got))
		}
	}
}

type failing struct{ n int }

func (f *failing) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("boom")
	}
	f.n--
	p[0] = 'x'
	return 1, nil
}

func TestErrorAndEarlyClose(t *testing.T) {
	ra := New(&failing{n: 5}, 2, 2)
	got, err := io.ReadAll(ra)
	if err == nil || err.Error() != "boom" || string(got) != "xxxxx" {
		t.Fatalf("got %q, %v", got, err)
	}
	ra.Close()
	// Closing without reading everything must not leak or block.
	ra = New(bytes.NewReader(make([]byte, 10<<20)), 4096, 2)
	buf := make([]byte, 10)
	ra.Read(buf)
	ra.Close()
}
