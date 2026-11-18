package web

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

// git's smart HTTP protocol (v0, stateless), the minimal part of it: refs
// are advertised and every fetch gets the repository's packs as they are,
// whatever the client already has. That keeps the server as cheap as the
// dumb protocol, lets git show progress while a repository is unbundled,
// and serves clients that only speak the smart protocol (libgit2, go-git).
// Shallow and partial clones are not offered.

func pktLine(w io.Writer, s string) error {
	_, err := fmt.Fprintf(w, "%04x%s", len(s)+4, s)
	return err
}

func pktFlush(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}

// advertise answers GET info/refs?service=git-upload-pack. The refs come
// from the bundle headers when the repository is not unbundled yet, so the
// client does not wait here: the wait happens in uploadPack, with progress.
func (s *Server) advertise(w http.ResponseWriter, t *target) {
	type adv struct {
		name         string
		hash, peeled gitstore.Hash
	}
	var refs []adv
	var head string
	if s.mgr().Built(t.Key) {
		repo, release, err := s.mgr().Repo(t.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		list, h, err := repo.Refs()
		release()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, r := range list {
			refs = append(refs, adv{r.Name, r.Hash, r.Peeled})
		}
		head = h
	} else {
		list, h, err := s.mgr().PlannedRefs(t.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var real []gitstore.Ref
		for _, r := range list {
			if strings.HasPrefix(r.Name, "refs/") {
				real = append(real, r)
				refs = append(refs, adv{name: r.Name, hash: r.Hash})
			}
		}
		head = gitstore.ChooseHead(real, h)
		s.mgr().BuildAsync(t.Key)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	var headHash gitstore.Hash
	shown := refs[:0]
	for _, r := range refs {
		if !gitstore.Advertised(r.name) {
			continue
		}
		shown = append(shown, r)
		if r.name == head {
			headHash = r.hash
		}
	}

	caps := "multi_ack_detailed side-band-64k side-band ofs-delta no-progress agent=gbb/" + s.Version
	if !headHash.IsZero() {
		caps = "symref=HEAD:" + head + " " + caps
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	bw := bufio.NewWriter(w)
	pktLine(bw, "# service=git-upload-pack\n")
	pktFlush(bw)
	first := true
	line := func(h gitstore.Hash, name string) {
		if first {
			pktLine(bw, h.String()+" "+name+"\x00"+caps+"\n")
			first = false
			return
		}
		pktLine(bw, h.String()+" "+name+"\n")
	}
	if !headHash.IsZero() {
		line(headHash, "HEAD")
	}
	for _, r := range shown {
		line(r.hash, r.name)
		if !r.peeled.IsZero() {
			line(r.peeled, r.name+"^{}")
		}
	}
	if first { // an empty repository
		line(gitstore.ZeroHash, "capabilities^{}")
	}
	pktFlush(bw)
	bw.Flush()
}

// uploadRequest is a parsed git-upload-pack request.
type uploadRequest struct {
	wants, haves []gitstore.Hash
	done         bool
	sideband     int // 0, or the largest packet: 1000 (side-band) or 65520
	noProgress   bool
}

func readUploadRequest(r io.Reader) (*uploadRequest, error) {
	req := &uploadRequest{}
	br := bufio.NewReader(r)
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				return req, nil
			}
			return nil, err
		}
		var n int
		if _, err := fmt.Sscanf(string(hdr[:]), "%04x", &n); err != nil {
			return nil, errors.New("bad pkt-line")
		}
		if n == 0 { // flush
			continue
		}
		if n < 4 || n > 65520 {
			return nil, errors.New("bad pkt-line length")
		}
		buf := make([]byte, n-4)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		line := strings.TrimSuffix(string(buf), "\n")
		cmd, arg, _ := strings.Cut(line, " ")
		switch cmd {
		case "want":
			id, caps, _ := strings.Cut(arg, " ")
			h, err := gitstore.ParseHash(id)
			if err != nil {
				return nil, fmt.Errorf("bad want %q", id)
			}
			if len(req.wants) == 0 {
				for _, c := range strings.Fields(caps) {
					switch c {
					case "side-band-64k":
						req.sideband = 65520
					case "side-band":
						if req.sideband == 0 {
							req.sideband = 1000
						}
					case "no-progress":
						req.noProgress = true
					}
				}
			}
			req.wants = append(req.wants, h)
		case "have":
			h, err := gitstore.ParseHash(arg)
			if err != nil {
				return nil, fmt.Errorf("bad have %q", arg)
			}
			req.haves = append(req.haves, h)
		case "done":
			req.done = true
			return req, nil
		case "shallow", "deepen", "deepen-since", "deepen-not", "filter":
			return nil, errors.New("shallow and partial clones are not supported by gbb; clone without --depth or --filter")
		}
	}
}

// sideband frames data on a side-band channel.
type sideband struct {
	w    io.Writer
	max  int // largest packet, header included
	band byte
}

func (s sideband) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), s.max-5)
		if _, err := fmt.Fprintf(s.w, "%04x%c", n+5, s.band); err != nil {
			return 0, err
		}
		if _, err := s.w.Write(p[:n]); err != nil {
			return 0, err
		}
		p = p[n:]
	}
	return total, nil
}

// uploadPack answers POST git-upload-pack.
func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, t *target) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// 16 MiB is ~300k object ids: far more than any clone sends.
	var body io.Reader = http.MaxBytesReader(w, r.Body, 16<<20)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		defer zr.Close()
		body = io.LimitReader(zr, 16<<20)
	}
	req, err := readUploadRequest(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if len(req.wants) == 0 {
		return
	}
	mgr := s.mgr()
	built := mgr.Built(t.Key)

	// Negotiation. Every fetch gets the full packs, so the only use of the
	// client's haves is to end the negotiation quickly: the first one we
	// have is acknowledged as common and "ready".
	var common gitstore.Hash
	if built && len(req.haves) > 0 {
		repo, release, err := mgr.Repo(t.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, h := range req.haves {
			if repo.Has(h) {
				common = h
				break
			}
		}
		release()
	} else if !built {
		mgr.BuildAsync(t.Key)
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	if !req.done {
		if !common.IsZero() {
			pktLine(bw, "ACK "+common.String()+" common\n")
			pktLine(bw, "ACK "+common.String()+" ready\n")
		}
		pktLine(bw, "NAK\n")
		return
	}
	if common.IsZero() {
		pktLine(bw, "NAK\n")
	} else {
		pktLine(bw, "ACK "+common.String()+"\n")
	}

	var data, progress io.Writer = bw, io.Discard
	fail := func(msg string) {
		if req.sideband > 0 {
			sideband{bw, req.sideband, 3}.Write([]byte("gbb: " + msg + "\n"))
		}
	}
	if req.sideband > 0 {
		data = sideband{bw, req.sideband, 1}
		if !req.noProgress {
			progress = sideband{bw, req.sideband, 2}
		}
	}
	if err := s.waitBuilt(w, r, bw, progress, t.Key); err != nil {
		fail(err.Error())
		return
	}
	repo, release, err := mgr.Repo(t.Key)
	if err != nil {
		fail(err.Error())
		return
	}
	for _, h := range req.wants {
		if !repo.Has(h) {
			release()
			fail("object " + h.String() + " is not in the backup (its ref was dropped while unbundling)")
			return
		}
	}
	packs := repo.PackPaths()
	release() // the pack files are read from disk, not through the repo
	if _, err := gitstore.WritePacks(data, packs); err != nil {
		fail(err.Error())
		return
	}
	if req.sideband > 0 {
		pktFlush(bw)
	}
}

// waitBuilt waits for the repository to be unbundled, reporting progress
// to the git client.
func (s *Server) waitBuilt(w http.ResponseWriter, r *http.Request, bw *bufio.Writer, progress io.Writer, key string) error {
	mgr := s.mgr()
	if mgr.Built(key) {
		return nil
	}
	errc := make(chan error, 1)
	go func() { errc <- mgr.Ensure(key) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	shown := false
	for {
		select {
		case err := <-errc:
			if shown && err == nil {
				fmt.Fprint(progress, "gbb: repository unbundled, sending it\n")
			}
			return err
		case <-r.Context().Done():
			return r.Context().Err()
		case <-tick.C:
			p, ok := mgr.BuildProgress(key)
			if !ok {
				continue
			}
			msg := "gbb: unbundling from the backup: " + p.Stage
			if p.Bundles > 1 {
				msg += fmt.Sprintf(" (bundle %d/%d)", p.Bundle, p.Bundles)
			}
			if pc := p.Percent(); pc >= 0 {
				msg += fmt.Sprintf(" %d%% (%d/%d)", pc, p.Done, p.Total)
			}
			fmt.Fprint(progress, msg+"\r")
			shown = true
			bw.Flush()
			http.NewResponseController(w).Flush()
		}
	}
}
