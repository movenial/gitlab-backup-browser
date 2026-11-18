package index

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/fixture"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

// The same backup, as three GitLab generations would write it, must give
// the same projects, states and links; schema differences are noted, never
// as warnings when a fallback exists.
func TestGitLabEras(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, tc := range []struct {
		name   string
		era    fixture.Era
		layout fixture.Layout
		notes  []string // expected notes ("table.column", info only)
	}{
		{"gitlab12", fixture.Era12, fixture.Classic, []string{"namespace_details", "project_repositories", "topics", "project_topics", "snippet_repositories",
			"issues.state_id", "merge_requests.state_id", "merge_requests.draft", "notes.internal", "users.user_type"}},
		{"gitlab17", fixture.Era17, fixture.Gitaly, nil},
		{"gitlab19", fixture.Era19, fixture.Gitaly, []string{"namespaces.description"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			res, err := fixture.BuildEra(tmp, tc.layout, fixture.Dir, tc.era)
			if err != nil {
				t.Fatal(err)
			}
			src, err := backup.OpenSource(res.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			trDir := filepath.Join(tmp, "tracker")
			exp := filepath.Join(tmp, "expected.json")
			ix, err := Build(src, "fp", Options{TrackerDir: trDir, ExpectedFile: exp}, nil, nil)
			if err != nil || ix.DBStatus != "ok" {
				t.Fatalf("%v %s", err, ix.DBStatus)
			}
			// Projects found with their repositories, whatever the storage.
			for _, p := range []string{"acme/website", "acme/tools/cli", "zoe/dotfiles"} {
				if pr := ix.Project(p); pr == nil || pr.Repo == "" {
					t.Errorf("%s: %+v", p, pr)
				}
			}
			if p := ix.Project("acme/website"); p.Wiki == "" || p.OpenIssues != 2 || p.OpenMRs != 1 {
				t.Errorf("website: wiki %q, %d open issues, %d open MRs", p.Wiki, p.OpenIssues, p.OpenMRs)
			}
			if ns := ix.Namespace("acme"); ns == nil || ns.Description != "ACME Corp" {
				t.Errorf("group description: %+v", ns)
			}
			if p := ix.Project("renovate-bot/renovate-config"); p == nil || p.Kind != "bot" {
				t.Errorf("bot not recognised: %+v", p)
			}
			ts, err := tracker.Open(trDir)
			if err != nil {
				t.Fatal(err)
			}
			if is, _ := ts.Issue(1, 2); is == nil || is.State != "closed" {
				t.Errorf("issue #2: %+v", is)
			}
			mrs, _, _ := ts.MergeRequests(1, tracker.Query{State: "all"})
			states := map[int64]string{}
			drafts := map[int64]bool{}
			for _, mr := range mrs {
				states[mr.IID], drafts[mr.IID] = mr.State, mr.Draft
			}
			if states[3] != "merged" || states[2] != "closed" || !drafts[2] || drafts[1] {
				t.Errorf("MR states %v drafts %v", states, drafts)
			}
			// Attachments checked against the right tables.
			e, err := ReadExpected(exp)
			if err != nil || len(e.Uploads) != 2 {
				t.Errorf("expected uploads: %v %+v", err, e)
			}
			// Schema notes: exactly the expected fallbacks, no warning.
			got := map[string]bool{}
			for _, n := range ix.Schema {
				if n.Severity == "warning" {
					t.Errorf("warning: %s", n)
				}
				k := n.Table
				if n.Column != "" {
					k += "." + n.Column
				}
				got[k] = true
			}
			for _, k := range tc.notes {
				if !got[k] {
					t.Errorf("missing note %s (got %v)", k, ix.Schema)
				}
			}
			if len(got) != len(tc.notes) {
				t.Errorf("notes: got %v, want %v", ix.Schema, tc.notes)
			}
		})
	}
}
