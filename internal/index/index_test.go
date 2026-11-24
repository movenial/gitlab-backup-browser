package index

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/fixture"
	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cases := []struct {
		name   string
		layout fixture.Layout
		format fixture.Format
	}{
		{"gitaly-tar", fixture.Gitaly, fixture.Tar},
		{"gitaly-dir", fixture.Gitaly, fixture.Dir},
		{"gitaly-tgz", fixture.Gitaly, fixture.TarGz},
		{"classic-tar", fixture.Classic, fixture.Tar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			res, err := fixture.Build(tmp, tc.layout, tc.format)
			if err != nil {
				t.Fatal(err)
			}
			path := res.Path
			if tc.format == fixture.TarGz {
				path = filepath.Join(tmp, "extracted")
				if err := backup.ExtractTarGz(res.Path, path, nil); err != nil {
					t.Fatal(err)
				}
			}
			src, err := backup.OpenSource(path)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			ix, err := Build(src, "fp", Options{TrackerDir: filepath.Join(tmp, "tracker")}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if ix.DBStatus != "ok" {
				t.Fatalf("db: %s", ix.DBStatus)
			}
			if ix.Info["gitlab_version"] != "17.5.1" {
				t.Fatalf("info %v", ix.Info)
			}
			web := ix.Project("acme/website")
			if web == nil || web.Repo == "" || web.Wiki == "" || web.Description != "Company website\twith tab" || web.VisibilityName() != "public" {
				t.Fatalf("website %+v", web)
			}
			if p := ix.Project("ACME/Tools/CLI"); p == nil || p.Repo == "" {
				t.Fatalf("cli %+v", p)
			}
			if p := ix.Project("zoe/dotfiles"); p == nil || !p.Archived {
				t.Fatalf("dotfiles %+v", p)
			}
			if p := ix.Project("acme/empty"); p == nil || p.Repo != "" {
				t.Fatalf("empty %+v", p)
			}
			st := ix.Stats()
			if st.Unmatched != 1 || st.WithRepo != 3 || st.WithoutRepo != 2 || st.Wikis != 1 {
				t.Fatalf("stats %+v", st)
			}
			ns, ps := ix.Children("")
			if len(ns) != 4 || len(ps) != 0 { // acme, zoe, renovate-bot, _unmatched
				t.Fatalf("top level ns=%d ps=%d", len(ns), len(ps))
			}
			kinds := map[string]string{}
			for _, n := range ns {
				kinds[n.Path] = n.Kind
			}
			if kinds["acme"] != "group" || kinds["zoe"] != "user" || kinds["renovate-bot"] != "bot" {
				t.Fatalf("namespace kinds %v", kinds)
			}
			if p := ix.Project("renovate-bot/renovate-config"); p == nil || p.Kind != "bot" {
				t.Fatalf("bot project %+v", p)
			}
			if p := ix.Project("zoe/dotfiles"); p.Kind != "personal" || ix.Project("acme/website").Kind != "group" {
				t.Fatal("project kinds")
			}
			if w := ix.Project("acme/website"); len(w.Topics) != 2 || w.Topics[0] != "Infrastructure" || w.Size == 0 {
				t.Fatalf("topics/size %+v %d", w.Topics, w.Size)
			}
			if tc := ix.Topics(); len(tc) != 2 || tc[0].Name != "Infrastructure" || tc[0].Count != 2 {
				t.Fatalf("topic counts %+v", tc)
			}
			ns, ps = ix.Children("acme")
			if len(ns) != 1 || len(ps) != 2 {
				t.Fatalf("acme ns=%d ps=%d", len(ns), len(ps))
			}

			// Issues, merge requests, comments and users.
			if !ix.Tracker || web.Issues != 3 || web.OpenIssues != 2 || web.MRs != 3 || web.OpenMRs != 1 {
				t.Fatalf("tracker counts %+v (tracker=%v)", web, ix.Tracker)
			}
			ts, err := tracker.Open(filepath.Join(tmp, "tracker"))
			if err != nil {
				t.Fatal(err)
			}
			if ts.UserName(1) != "Zoé M." || ts.UserName(2) != "Ada Lovelace" {
				t.Fatalf("users %v", ts.Users)
			}
			if raw, _ := os.ReadFile(filepath.Join(tmp, "tracker", "users.json")); strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "example.com") {
				t.Fatal("credentials or e-mails leaked into users.json")
			}
			open, total, err := ts.Issues(fixture.WebsiteID, tracker.Query{State: "opened"})
			if err != nil || total != 2 || len(open) != 2 || open[0].IID != 3 {
				t.Fatalf("open issues %v %d %v", open, total, err)
			}
			if found, n, _ := ts.Issues(fixture.WebsiteID, tracker.Query{State: "all", Search: "copyrigth"}); n != 1 || found[0].IID != 2 {
				t.Fatalf("issue search: %d", n)
			}
			is, err := ts.Issue(fixture.WebsiteID, 1)
			if err != nil || !strings.Contains(is.Description, "**logo**") {
				t.Fatalf("issue 1: %+v %v", is, err)
			}
			notes, _, err := ts.Notes(fixture.WebsiteID, "Issue", is.ID, 100, true)
			if err != nil || len(notes) != 3 || !notes[1].System || !notes[2].Internal || notes[2].Body == "" {
				t.Fatalf("notes %+v %v", notes, err)
			}
			if notes, _, _ := ts.Notes(fixture.WebsiteID, "Issue", is.ID, 100, false); len(notes) != 3 || !notes[2].Redacted || notes[2].Body != "" || notes[2].Author != 0 || notes[2].Created == "" {
				t.Fatalf("redacted internal note %+v", notes[2])
			}
			hidden, n, _ := ts.Issues(fixture.WebsiteID, tracker.Query{State: "opened", HideConfidential: true})
			if n != 2 || hidden[0].IID != 3 || !hidden[0].Redacted || hidden[0].Title != "" || hidden[0].Author != 0 || hidden[0].State != "opened" {
				t.Fatalf("confidential placeholder %d %+v", n, hidden)
			}
			if _, n, _ := ts.Issues(fixture.WebsiteID, tracker.Query{State: "all", Search: "contact", HideConfidential: true}); n != 0 {
				t.Fatalf("confidential issue found by search: %d", n)
			}
			if w := ix.Project("acme/website"); w.ConfidentialIssues != 1 || w.OpenIssues != 2 {
				t.Fatalf("confidential counts %+v", w)
			}
			mr, err := ts.MergeRequest(fixture.WebsiteID, 1)
			if err != nil || mr.HeadSHA != res.MRHead || mr.BaseSHA != res.MRBase || mr.Source != "feature/dark-mode" {
				t.Fatalf("mr %+v %v", mr, err)
			}
			if merged, n, _ := ts.MergeRequests(fixture.WebsiteID, tracker.Query{State: "closed"}); n != 1 || !merged[0].Draft {
				t.Fatalf("closed MRs: %d", n)
			}

			// Materialise the incremental CLI repo and check its HEAD.
			cli := ix.Project("acme/tools/cli")
			dst := filepath.Join(tmp, "repos", "cli.git")
			bs, err := backup.BuildRepo(src, ix.Repos[cli.Repo], dst)
			if err != nil {
				t.Fatal(err)
			}
			if tc.layout == fixture.Gitaly && bs.Packs != 2 {
				t.Fatalf("expected 2 packs from incremental chain, got %+v", bs)
			}
			repo, err := gitstore.Open(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			h, ref, err := repo.Resolve("")
			if err != nil || h.String() != res.CLIHead || ref != "refs/heads/main" {
				t.Fatalf("cli head %s %s %v want %s", h, ref, err, res.CLIHead)
			}
			out, err := exec.Command("git", "--git-dir", dst, "fsck", "--full").CombinedOutput()
			if err != nil {
				t.Fatalf("fsck: %v %s", err, out)
			}

			// Save / load round trip.
			ip := filepath.Join(tmp, "index.json")
			if err := ix.Save(ip); err != nil {
				t.Fatal(err)
			}
			back, err := Load(ip, "fp")
			if err != nil || back == nil || back.Project("acme/website") == nil {
				t.Fatalf("load: %v", err)
			}
			if again, _ := Load(ip, "other"); again != nil {
				t.Fatal("fingerprint mismatch should invalidate")
			}
		})
	}
}

func TestPartialListsBundles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	res, err := fixture.Build(tmp, fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	src, err := backup.OpenSource(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ix := Partial(src, "fp")
	n := 0
	for _, p := range ix.Projects {
		if !p.Unmatched || !strings.HasPrefix(p.FullPath, "_unmatched/") {
			t.Fatalf("partial project %+v", p)
		}
		n++
	}
	if n != 7 { // 4 code repos + 1 wiki + 2 snippets
		t.Fatalf("partial projects: %d", n)
	}
}
