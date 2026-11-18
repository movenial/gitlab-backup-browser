package tracker

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Interleaved records across many projects, with a tiny budget forcing
// many spills: every file must still hold its records in order.
func TestWriterSpillsKeepOrder(t *testing.T) { testWriter(t, false) }
func TestWriterAsync(t *testing.T)           { testWriter(t, true) }

func testWriter(t *testing.T, async bool) {
	dir := filepath.Join(t.TempDir(), "tr")
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	if async {
		w.Async()
	}
	w.budget = 4 << 10
	const projects, perProject = 30, 200
	for i := 0; i < projects*perProject; i++ {
		pid := int64(i%projects + 1)
		w.AddNote(pid, &Note{ID: int64(i), Type: "Issue", Target: 7, Body: fmt.Sprintf("<b>note %d</b> & more", i)})
		if i%projects == 0 {
			w.AddIssue(pid, &Issue{ID: int64(i), IID: int64(i/projects + 1), Title: "t", State: "opened"})
		}
	}
	w.AddMRDiff(1, 99, "0123456789012345678901234567890123456789", "abcdefabcdefabcdefabcdefabcdefabcdefabcd")
	w.AddMR(3, &MergeRequest{ID: 99, IID: 1, Title: "mr", State: "merged"})
	counts, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if w.Flushes < 50 {
		t.Fatalf("expected many spills with a tiny budget, got %d", w.Flushes)
	}
	if counts[1].Issues != perProject || counts[1].OpenIssues != perProject || counts[3].MRs != 1 {
		t.Fatalf("counts %+v", counts[1])
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for pid := int64(1); pid <= projects; pid++ {
		notes, _, err := s.Notes(pid, "Issue", 7, 10000)
		if err != nil || len(notes) != perProject {
			t.Fatalf("project %d: %d notes, %v", pid, len(notes), err)
		}
		for j := 1; j < len(notes); j++ {
			if notes[j].ID <= notes[j-1].ID {
				t.Fatalf("project %d: notes out of order", pid)
			}
		}
	}
	if n, _, _ := s.Notes(1, "Issue", 7, 1); n[0].Body != "<b>note 0</b> & more" {
		t.Fatalf("body %q", n[0].Body)
	}
	mr, err := s.MergeRequest(3, 1)
	if err != nil || mr.BaseSHA == "" || mr.HeadSHA[:6] != "abcdef" {
		t.Fatalf("mr %+v %v", mr, err)
	}
	// Issues come back newest first, paged.
	page, total, _ := s.Issues(1, Query{Limit: 10})
	if total != perProject || len(page) != 10 || page[0].IID != perProject {
		t.Fatalf("issues page: total %d len %d first %d", total, len(page), page[0].IID)
	}
}
