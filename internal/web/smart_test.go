package web

import (
	"bytes"
	"strings"
	"testing"
)

func pkts(lines ...string) string {
	var b bytes.Buffer
	for _, l := range lines {
		if l == "" {
			pktFlush(&b)
		} else {
			pktLine(&b, l)
		}
	}
	return b.String()
}

const (
	h1 = "1111111111111111111111111111111111111111"
	h2 = "2222222222222222222222222222222222222222"
)

func TestReadUploadRequest(t *testing.T) {
	req, err := readUploadRequest(strings.NewReader(pkts("want "+h1+" multi_ack_detailed side-band-64k no-progress agent=git/2\n", "want "+h2+"\n", "", "have "+h2+"\n", "done\n")))
	if err != nil || len(req.wants) != 2 || len(req.haves) != 1 || !req.done || req.sideband != 65520 || !req.noProgress {
		t.Fatalf("%+v %v", req, err)
	}
	req, err = readUploadRequest(strings.NewReader(pkts("want "+h1+" side-band\n", "", "have "+h1+"\n", "")))
	if err != nil || req.done || req.sideband != 1000 {
		t.Fatalf("%+v %v", req, err)
	}
	for _, bad := range []string{"zzzz", "0003", pkts("want nope\n"), pkts("want "+h1+"\n", "deepen 1\n")} {
		if _, err := readUploadRequest(strings.NewReader(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Side-band packets stay within the limit.
	var b bytes.Buffer
	sideband{&b, 1000, 1}.Write(bytes.Repeat([]byte("x"), 2500))
	if b.Len() != 2500+3*5 || b.String()[:5] != "03e8\x01" {
		t.Fatalf("sideband framing: %d %q", b.Len(), b.String()[:5])
	}
}

func FuzzUploadRequest(f *testing.F) {
	f.Add([]byte(pkts("want "+h1+" side-band-64k\n", "", "have "+h2+"\n", "done\n")))
	f.Add([]byte("0000"))
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := readUploadRequest(bytes.NewReader(data))
		if err == nil && len(req.wants)+len(req.haves) > len(data)/45 {
			t.Fatalf("%d ids from %d bytes", len(req.wants)+len(req.haves), len(data))
		}
	})
}
