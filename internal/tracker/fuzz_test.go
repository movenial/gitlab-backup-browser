package tracker

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzStore reads tracker files and pages through them with arbitrary
// queries (page numbers come from URLs).
func FuzzStore(f *testing.F) {
	f.Add([]byte(`{"1":{"username":"ada","name":"Ada"}}`),
		[]byte(`{"id":1,"iid":1,"title":"t","state":"opened"}`+"\n"+`{"id":2,"iid":2,"title":"u","state":"closed"}`+"\n"),
		[]byte(`{"id":9,"iid":1,"title":"mr","state":"merged","source":"a","target":"b"}`+"\n"),
		[]byte(`{"id":1,"type":"Issue","target":1,"body":"hi"}`+"\n"),
		[]byte(`{"1":["0123456789012345678901234567890123456789","abcdefabcdefabcdefabcdefabcdefabcdefabcd"]}`),
		"t", 0, 50)
	f.Add([]byte(`null`), []byte("\n\n{}\n"), []byte(`{"iid":`), []byte{}, []byte(`[]`), "", -9223372036854775808, 9223372036854775807)
	f.Fuzz(func(t *testing.T, users, issues, mrs, notes, diffs []byte, search string, offset, limit int) {
		dir := t.TempDir()
		pd := filepath.Join(dir, "1")
		os.MkdirAll(pd, 0o755)
		os.WriteFile(filepath.Join(dir, "users.json"), users, 0o644)
		os.WriteFile(filepath.Join(pd, "issues.jsonl"), issues, 0o644)
		os.WriteFile(filepath.Join(pd, "merge_requests.jsonl"), mrs, 0o644)
		os.WriteFile(filepath.Join(pd, "notes.jsonl"), notes, 0o644)
		os.WriteFile(filepath.Join(pd, "mr_diffs.json"), diffs, 0o644)
		s, err := Open(dir)
		if err != nil {
			return
		}
		s.UserName(1)
		s.UserName(0)
		q := Query{State: "all", Search: search, Offset: offset, Limit: limit}
		// A damaged file may repeat an iid, so a page can hold more
		// records than were counted: only crashes are checked here.
		s.Issues(1, q)
		s.MergeRequests(1, q)
		s.Issue(1, 1)
		s.MergeRequest(1, 1)
		s.Notes(1, "Issue", 1, 10, false)
	})
}
