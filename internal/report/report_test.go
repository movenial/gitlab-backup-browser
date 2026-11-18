package report

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/fixture"
	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

func load(t *testing.T, path, trackerDir string) *Loaded {
	t.Helper()
	src, err := backup.OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	exp := filepath.Join(t.TempDir(), "expected.json")
	ix, err := index.Build(src, path, index.Options{TrackerDir: trackerDir, ExpectedFile: exp}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loaded{Path: path, Index: ix, Src: src}
	if l.Expected, err = index.ReadExpected(exp); err != nil {
		t.Fatal(err)
	}
	if trackerDir != "" {
		if l.Tracker, err = tracker.Open(trackerDir); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func needGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func refChange(c *Comparison, project, ref string) string {
	for _, r := range c.Refs {
		if r.Project == project && r.Ref == ref {
			return r.Change
		}
	}
	return ""
}

func hasProblem(r *Report, kind, project string) bool {
	for _, p := range r.Problems {
		if p.Kind == kind && (project == "" || p.Project == project) {
			return true
		}
	}
	return false
}

func TestCompareWithPrevious(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	day1, err := fixture.Build(filepath.Join(tmp, "d1"), fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	day2, err := fixture.BuildNext(filepath.Join(tmp, "d2"), fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	prev := load(t, day1.Path, filepath.Join(tmp, "t1"))
	cur := load(t, day2.Path, filepath.Join(tmp, "t2"))
	prevSnap := TakeSnapshot(prev, Summarize(prev, time.Now()))

	// Header-only comparison.
	r, _ := Run(cur, Options{Previous: prevSnap, TempDir: tmp})
	c := r.Comparison
	if c == nil || len(c.ProjectsRemoved) != 1 || c.ProjectsRemoved[0] != "zoe/dotfiles" {
		t.Fatalf("removed: %+v", c)
	}
	if got := refChange(c, "acme/website", "refs/tags/v1.0.0"); got != "deleted" {
		t.Fatalf("tag: %q", got)
	}
	if got := refChange(c, "acme/website", "refs/heads/fix/typo"); got != "deleted-merged" || hasProblem(r, "ref_deleted", "acme/website") && strings.Contains(fmt.Sprint(r.Problems), "fix/typo") {
		t.Fatalf("branch deleted after its MR was merged: %q, problems %+v", got, r.Problems)
	}
	if got := refChange(c, "acme/website", "refs/heads/main"); got != "moved" {
		t.Fatalf("main without --deep: %q", got)
	}
	if c.IssuesDelta != -1 || !hasProblem(r, "issues_decreased", "acme/website") || !hasProblem(r, "project_removed", "zoe/dotfiles") {
		t.Fatalf("issues: delta %d problems %+v", c.IssuesDelta, r.Problems)
	}
	if r.Status != StatusWarning || r.ExitCode() != 1 {
		t.Fatalf("status %s", r.Status)
	}

	// Restoring in parallel gives the same report.
	par, _ := Run(cur, Options{Previous: prevSnap, Deep: true, Jobs: 4, TempDir: tmp})
	seq, _ := Run(cur, Options{Previous: prevSnap, Deep: true, Jobs: 1, TempDir: tmp})
	if par.Jobs != 4 || len(par.Problems) != len(seq.Problems) || par.ReposChecked != seq.ReposChecked {
		t.Fatalf("jobs=4: %d problems, jobs=1: %d", len(par.Problems), len(seq.Problems))
	}
	for i := range seq.Problems {
		if par.Problems[i] != seq.Problems[i] {
			t.Fatalf("problem %d differs: %+v vs %+v", i, par.Problems[i], seq.Problems[i])
		}
	}

	// --deep tells normal pushes from rewinds and force-pushes.
	r, _ = Run(cur, Options{Previous: prevSnap, Deep: true, TempDir: tmp})
	c = r.Comparison
	want := map[string]string{
		"acme/website refs/heads/main":              "rewound",
		"acme/website refs/heads/feature/dark-mode": "diverged",
		"acme/tools/cli refs/heads/main":            "fast-forward",
	}
	for k, w := range want {
		p, ref, _ := strings.Cut(k, " ")
		if got := refChange(c, p, ref); got != w {
			t.Errorf("%s: %q, want %q", k, got, w)
		}
	}
	if !hasProblem(r, "ref_rewound", "acme/website") || !hasProblem(r, "ref_diverged", "acme/website") || hasProblem(r, "ref_rewound", "acme/tools/cli") {
		t.Fatalf("problems %+v", r.Problems)
	}

	// Text, JSON and Prometheus renderings.
	var txt, js, prom bytes.Buffer
	r.WriteText(&txt)
	for _, s := range []string{"Compared with the backup of 2024-10-27", "1 rewound", "1 diverged", "1 deleted-merged", "WARN  zoe/dotfiles", "WARNING:"} {
		if !strings.Contains(txt.String(), s) {
			t.Errorf("text report misses %q:\n%s", s, txt.String())
		}
	}
	if err := r.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || back.Status != StatusWarning || back.Comparison == nil {
		t.Fatalf("json: %v", err)
	}
	r.WritePrometheus(&prom)
	for _, s := range []string{
		"gbb_verify_status 1",
		`gbb_compare_refs{change="rewound"} 1`,
		`gbb_compare_refs{change="deleted-merged"} 1`,
		"gbb_compare_projects_removed 1",
		"gbb_backup_created_timestamp_seconds 1.7301096e+09",
		`gbb_backup_projects{state="with_repository"} 2`,
		`gbb_verify_problems{severity="warning",kind="project_removed"} 1`,
		"# TYPE gbb_backup_issues gauge",
	} {
		if !strings.Contains(prom.String(), s) {
			t.Errorf("metrics miss %q:\n%s", s, prom.String())
		}
	}
	file := filepath.Join(tmp, "gbb.prom")
	if err := r.WritePrometheusFile(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary metrics file left behind")
	}
}

func TestHealthyAndBroken(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	res, err := fixture.Build(tmp, fixture.Gitaly, fixture.Dir)
	if err != nil {
		t.Fatal(err)
	}
	l := load(t, res.Path, "")
	day := time.Date(2024, 10, 27, 12, 0, 0, 0, time.UTC)
	r, _ := Run(l, Options{Deep: true, TempDir: tmp, MaxAge: 26 * time.Hour, Now: func() time.Time { return day }})
	if r.Status != StatusOK || r.ReposChecked != 7 || r.Backup.AgeSeconds != 2*3600 {
		t.Fatalf("healthy: %s %d %v %+v", r.Status, r.ReposChecked, r.Backup.AgeSeconds, r.Problems)
	}

	// Too old.
	later := func() time.Time { return day.Add(72 * time.Hour) }
	if r, _ := Run(l, Options{MaxAge: 26 * time.Hour, Now: later}); r.Status != StatusError || !hasProblem(r, "backup_too_old", "") {
		t.Fatalf("max-age: %s", r.Status)
	}

	// A truncated bundle is an error with --deep.
	var victim string
	filepath.Walk(filepath.Join(res.Path, "repositories"), func(p string, fi os.FileInfo, err error) error {
		if strings.HasSuffix(p, ".bundle") && strings.Contains(p, "@hashed") && strings.Contains(p, fixtureHashPrefix()) {
			victim = p
		}
		return nil
	})
	if victim == "" {
		t.Fatal("no bundle to break")
	}
	b, _ := os.ReadFile(victim)
	os.WriteFile(victim, b[:len(b)-30], 0o644)
	l = load(t, res.Path, "")
	r, _ = Run(l, Options{Deep: true, TempDir: tmp})
	if r.Status != StatusError || r.ExitCode() != 2 || !hasProblem(r, "repository_corrupt", "acme/website") {
		t.Fatalf("broken: %s %+v", r.Status, r.Problems)
	}
}

// fixtureHashPrefix is the hashed path of acme/website (project id 1).
func fixtureHashPrefix() string {
	return "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b.git"
}

func TestAssetsCheck(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	res, err := fixture.Build(tmp, fixture.Gitaly, fixture.Dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Run(load(t, res.Path, ""), Options{Deep: true, TempDir: tmp})
	if r.Status != StatusOK || len(r.Assets) != 2 {
		t.Fatalf("complete: %s %+v %+v", r.Status, r.Problems, r.Assets)
	}
	lfs, up := r.Assets[0], r.Assets[1]
	if lfs.Expected != 2 || lfs.Found != 2 || lfs.Remote != 1 || up.Expected != 2 || up.Found != 2 || up.Remote != 1 || up.Unchecked != 1 {
		t.Fatalf("counts: %+v %+v", lfs, up)
	}
	var text, prom bytes.Buffer
	r.WriteText(&text)
	r.WritePrometheus(&prom)
	if !strings.Contains(text.String(), "lfs.tar.gz") || !strings.Contains(prom.String(), `gbb_backup_assets{archive="lfs",state="missing"} 0`) {
		t.Fatalf("render:\n%s\n%s", text.String(), prom.String())
	}

	// An LFS object missing from the archive, and no uploads archive.
	if err := writeTarGz(filepath.Join(res.Path, "lfs.tar.gz"), map[string][]byte{}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(res.Path, "uploads.tar.gz"))
	r, _ = Run(load(t, res.Path, ""), Options{Deep: true, TempDir: tmp})
	if r.Status != StatusError || !hasProblem(r, "lfs_missing", "") || !hasProblem(r, "uploads_archive_missing", "") || r.Assets[0].Missing != 2 {
		t.Fatalf("broken: %s %+v %+v", r.Status, r.Problems, r.Assets[0])
	}
	// Not checked without --deep.
	if r, _ := Run(load(t, res.Path, ""), Options{TempDir: tmp}); r.Assets != nil {
		t.Fatal("assets checked without --deep")
	}
}

func writeTarGz(p string, files map[string][]byte) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return os.WriteFile(p, buf.Bytes(), 0o644)
}

func TestAssetsUnchecked(t *testing.T) {
	needGit(t)
	tmp := t.TempDir()
	res, err := fixture.Build(tmp, fixture.Gitaly, fixture.Dir)
	if err != nil {
		t.Fatal(err)
	}
	l := load(t, res.Path, "")
	l.Expected.Uploads, l.Expected.UploadsRemote = nil, 0 // as if the schema were unknown
	r, _ := Run(l, Options{Deep: true, TempDir: tmp})
	if !hasProblem(r, "uploads_unchecked", "") || r.Status != StatusWarning {
		t.Fatalf("%s %+v", r.Status, r.Problems)
	}
}

// verify --deep passes on every GitLab generation the fixture imitates,
// with the schema differences reported as notes, not problems.
func TestVerifyEras(t *testing.T) {
	needGit(t)
	for _, era := range []fixture.Era{fixture.Era12, fixture.Era17, fixture.Era19} {
		tmp := t.TempDir()
		layout := fixture.Gitaly
		if era == fixture.Era12 {
			layout = fixture.Classic
		}
		res, err := fixture.BuildEra(tmp, layout, fixture.Dir, era)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := Run(load(t, res.Path, filepath.Join(tmp, "tr")), Options{Deep: true, TempDir: tmp})
		if r.Status != StatusOK || r.Assets[1].Found != 2 || r.Assets[1].Missing != 0 {
			t.Errorf("era %d: %s %+v %+v", era, r.Status, r.Problems, r.Assets[1])
		}
		if era == fixture.Era12 && len(r.Schema) == 0 {
			t.Error("era 12: no schema notes")
		}
	}
}

func TestIgnoreRefs(t *testing.T) {
	for _, c := range []struct {
		pats         []string
		project, ref string
		want         bool
	}{
		{[]string{"renovate/*"}, "a/b", "refs/heads/renovate/lodash-4.x", true},
		{[]string{"renovate/*"}, "a/b", "refs/heads/main", false},
		{[]string{"mirrors/upstream-project:*"}, "mirrors/upstream-project", "refs/heads/main", true},
		{[]string{"mirrors/upstream-project:*"}, "mirrors/other", "refs/heads/main", false},
		{[]string{"*/mirrors/*:main"}, "g/mirrors/x", "refs/heads/main", true},
		{[]string{"v*-rc*"}, "a/b", "refs/tags/v1.0-rc2", true},
		{[]string{"refs/heads/tmp-*"}, "a/b", "refs/heads/tmp-1", true},
	} {
		if got := refIgnored(c.pats, c.project, c.ref); got != c.want {
			t.Errorf("%v %s %s: %v", c.pats, c.project, c.ref, got)
		}
	}
	needGit(t)
	tmp := t.TempDir()
	prev, err := fixture.Build(filepath.Join(tmp, "a"), fixture.Gitaly, fixture.Dir)
	if err != nil {
		t.Fatal(err)
	}
	next, err := fixture.BuildNext(filepath.Join(tmp, "b"), fixture.Gitaly, fixture.Dir)
	if err != nil {
		t.Fatal(err)
	}
	pl := load(t, prev.Path, "")
	snap := TakeSnapshot(pl, Summarize(pl, time.Now()))
	r, _ := Run(load(t, next.Path, ""), Options{Previous: snap, Deep: true, TempDir: tmp,
		IgnoreRefs: []string{"feature/*", "acme/website:v1.*"}})
	if hasProblem(r, "ref_diverged", "acme/website") || hasProblem(r, "ref_deleted", "acme/website") && strings.Contains(fmt.Sprint(r.Problems), "v1.0.0") {
		t.Errorf("ignored refs reported: %+v", r.Problems)
	}
	if !hasProblem(r, "ref_rewound", "acme/website") { // main is not ignored
		t.Errorf("main not reported: %+v", r.Problems)
	}
	n := 0
	for _, rc := range r.Comparison.Refs {
		if rc.Ignored {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d ignored, want 2: %+v", n, r.Comparison.Refs)
	}
	var prom strings.Builder
	r.WritePrometheus(&prom)
	for _, s := range []string{`gbb_compare_refs{change="diverged"} 0`, `gbb_compare_refs_ignored{change="diverged"} 1`, `gbb_compare_refs{change="rewound"} 1`} {
		if !strings.Contains(prom.String(), s) {
			t.Errorf("metrics miss %q:\n%s", s, prom.String())
		}
	}
}
