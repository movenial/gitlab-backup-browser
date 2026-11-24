package web

import (
	"archive/zip"
	"bytes"
	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/index"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/app"
	"github.com/movenial/gitlab-backup-browser/internal/fixture"
)

func setup(t *testing.T) (*httptest.Server, *fixture.Result, *app.Manager) {
	return setupWith(t, nil)
}

// setupWith serves the test backup as a logged-in user sees it
// (confidential issues shown).
func setupWith(t *testing.T, tweak func(*app.Config)) (*httptest.Server, *fixture.Result, *app.Manager) {
	t.Helper()
	ts, s, res, m := setupServer(t, tweak)
	s.ShowConfidential = true
	return ts, res, m
}

func setupServer(t *testing.T, tweak func(*app.Config)) (*httptest.Server, *Server, *fixture.Result, *app.Manager) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	res, err := fixture.Build(tmp, fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{Backup: res.Path, DataDir: filepath.Join(tmp, "data"), Logf: t.Logf}
	if tweak != nil {
		tweak(&cfg)
	}
	m := app.New(cfg)
	if err := m.Run(); err != nil {
		t.Fatal(err)
	}
	s, err := New(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, s, res, m
}

// TestConfidentialNeedsLogin: without a login (the default), confidential
// issues and internal comments are neither shown nor counted.
func TestConfidentialNeedsLogin(t *testing.T) {
	ts, _, _, _ := setupServer(t, nil)
	// The list keeps the issue's number and state, nothing else.
	body := expect(t, ts, "/acme/website/-/issues?state=all", 200, "Logo is blurry on retina",
		"🔒 Confidential issue", "#3 · readable only when gbb is started with a login", `href="/acme/website/-/issues/3"`,
		`>Issues <span class="count">2</span>`)
	if strings.Contains(body, "Security contact") || strings.Contains(body, "left out of searches") {
		t.Fatal("confidential title or filter notice in the plain list")
	}
	// A search or filter leaves it out (a match would tell what it holds) and says so.
	for _, q := range []string{"q=internal", "q=contact", "milestone=none", "label=bug"} {
		body = expect(t, ts, "/acme/website/-/issues?state=all&"+q, 200, "1 confidential issue is left out of searches and filters", "--auth")
		if strings.Contains(body, "Security contact") || strings.Contains(body, "issues/3") {
			t.Fatalf("%s: confidential issue matched", q)
		}
	}
	// Its page: number, state and how to read it; no title, description or comments.
	body = expect(t, ts, "/acme/website/-/issues/3", 403, "🔒 Confidential issue", "#3", "gbb serve --auth user:password", "opened")
	if strings.Contains(body, "internal only") || strings.Contains(body, "Security contact") || strings.Contains(body, "Activity") {
		t.Fatal("confidential issue content shown")
	}
	expect(t, ts, "/acme/website/-/issues/99", 404)
	// Internal comments keep their place and date only.
	body = expect(t, ts, "/acme/website/-/issues/1", 200, "<strong>Safari</strong>", "Activity (3)",
		"🔒 Internal comment, readable only when gbb is started with a login", "2024-09-01 13:00")
	if strings.Contains(body, "<strong>team</strong>") {
		t.Fatal("internal comment shown without a login")
	}
	// Label and milestone counts leave confidential issues out.
	if _, body, _ = get(t, ts.URL+"/acme/website/-/milestones?state=all"); !strings.Contains(body, "issues closed") {
		t.Fatalf("milestones page: %s", body)
	}
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func expect(t *testing.T, ts *httptest.Server, path string, code int, contains ...string) string {
	t.Helper()
	c, body, _ := get(t, ts.URL+path)
	if c != code {
		t.Fatalf("GET %s: status %d want %d\n%s", path, c, code, body)
	}
	for _, s := range contains {
		if !strings.Contains(body, s) {
			t.Fatalf("GET %s: missing %q in\n%s", path, s, body)
		}
	}
	return body
}

func waitBuilt(t *testing.T, m *app.Manager, key string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !m.Built(key) {
		if time.Now().After(deadline) {
			t.Fatalf("repository %s never built: %s", key, m.BuildError(key))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBrowse(t *testing.T) {
	ts, _, m := setup(t)
	ix := m.Index()

	expect(t, ts, "/", 200, "ACME", "Zoé", "Unmatched repositories", "GitLab 17.5.1")
	expect(t, ts, "/acme", 200, "Website", "Tools", "Empty")
	expect(t, ts, "/-/search?q=command", 200, "acme/tools/cli")
	expect(t, ts, "/-/info", 200, "acme/empty", "17.5.1")

	// First visit triggers unbundling.
	expect(t, ts, "/acme/website", 202, "Unbundling")
	waitBuilt(t, m, ix.Project("acme/website").Repo)

	body := expect(t, ts, "/acme/website", 200, "<strong>public</strong>", "git clone", "Add about page", "Wiki")
	if strings.Contains(body, "Download the original bundle") == false {
		t.Fatal("single-bundle repo should offer its bundle")
	}
	expect(t, ts, "/acme/website/-/tree/main/src", 200, "index.html", "about.html")
	expect(t, ts, "/acme/website/-/tree/feature/dark-mode/src", 200, "index.html")
	expect(t, ts, "/acme/website/-/tree/v1.0.0", 200, "README.md")
	expect(t, ts, "/acme/website/-/tree/main/src/..", 200, "README.md")
	expect(t, ts, "/acme/website/-/blob/main/src/index.html", 200, `<span class="hk">h1</span><span class="hp">&gt;</span>Hello<span class="hp">&lt;/</span>`, "Markup")
	expect(t, ts, "/acme/website/-/blob/main/assets/logo.bin", 200, "Binary file")
	expect(t, ts, "/acme/website/-/blob/main/README.md", 200, "<li>fast</li>")
	expect(t, ts, "/acme/website/-/commits/main", 200, "Add about page", "Initial site")
	expect(t, ts, "/acme/website/-/commits/main/src/about.html", 200, "Add about page")
	expect(t, ts, "/acme/website/-/branches", 200, "feature/dark-mode", "main")
	expect(t, ts, "/acme/website/-/tags", 200, "v1.0.0")
	expect(t, ts, "/acme/website/-/tree/nope", 404)
	expect(t, ts, "/acme/website/-/blob/main/missing.txt", 404)

	// Raw content is sandboxed.
	c, raw, hdr := get(t, ts.URL+"/acme/website/-/raw/main/src/index.html")
	if c != 200 || raw != "<h1>Hello</h1>\n" || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") || strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("raw: %d %q %v", c, raw, hdr)
	}

	// Commit page with a diff.
	repo, release, err := m.Repo(ix.Project("acme/website").Repo)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	h, _, _ := repo.Resolve("main")
	expect(t, ts, "/acme/website/-/commit/"+h.String(), 200, "src/about.html", `<span class="hk">p</span><span class="hp">&gt;</span>About<span class="hp">&lt;/</span>`, "With a body.")

	// Zip archive.
	c, zipBody, _ := get(t, ts.URL+"/acme/website/-/archive/main.zip")
	if c != 200 {
		t.Fatalf("zip status %d", c)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(zipBody)), int64(len(zipBody)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["website-main/src/about.html"] || !names["website-main/README.md"] {
		t.Fatalf("zip entries %v", names)
	}

	// Wiki is browsable too.
	expect(t, ts, "/acme/website.wiki", 202)
	waitBuilt(t, m, ix.Project("acme/website").Wiki)
	expect(t, ts, "/acme/website.wiki", 200, "Wiki home")

	// Projects without a repository, unknown paths, unmatched bundles.
	expect(t, ts, "/acme/empty", 200, "no repository bundle")
	expect(t, ts, "/nope/nothing", 404)
	var unmatched, unmatchedKey string
	for _, p := range ix.Projects {
		if p.Unmatched {
			unmatched, unmatchedKey = p.FullPath, p.Repo
		}
	}
	expect(t, ts, "/"+unmatched, 202)
	waitBuilt(t, m, unmatchedKey)
	expect(t, ts, "/"+unmatched, 200, "lost.txt")

	// Git LFS pointers are recognised, never served as images.
	expect(t, ts, "/acme/website/-/blob/main/assets/hero.png", 200, "Git LFS", "Extract default-branch files", "Extract all (5.0 KiB on disk)", "oid sha256:"+fixture.LFSOid(fixture.HeroPNG)[:8])
	c, _, hdr = get(t, ts.URL+"/acme/website/-/raw/main/assets/hero.png")
	if c != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") || hdr.Get("X-Git-LFS-Pointer") == "" {
		t.Fatalf("lfs raw: %d %v", c, hdr)
	}

	// Only public refs are advertised to git clients.
	_, refs, _ := get(t, ts.URL+"/acme/website.git/info/refs")
	if strings.Contains(refs, "keep-around") || strings.Contains(refs, "pipelines") ||
		!strings.Contains(refs, "refs/merge-requests/1/head") || !strings.Contains(refs, "refs/tags/v1.0.0^{}") {
		t.Fatalf("info/refs:\n%s", refs)
	}

	// Projects page: filters, chips, sorting.
	body = expect(t, ts, "/-/projects", 200, "acme/website", "acme/tools/cli", "acme/empty", "Bots and service accounts", "Show archived")
	if strings.Contains(body, "renovate-bot/renovate-config") || strings.Contains(body, "zoe/dotfiles") || strings.Contains(body, "_unmatched/") {
		t.Fatal("default view should hide bots, archived projects and unmatched bundles")
	}
	expect(t, ts, "/-/projects?kind=bot", 200, "renovate-bot/renovate-config")
	expect(t, ts, "/-/projects?kind=all&archived=include", 200, "zoe/dotfiles", "renovate-bot/renovate-config", "_unmatched/")
	body = expect(t, ts, "/-/projects?archived=only&kind=all", 200, "zoe/dotfiles")
	if strings.Contains(body, "acme/website") {
		t.Fatal("archived=only shows active projects")
	}
	body = expect(t, ts, "/-/projects?topic=web", 200, "acme/website", "# web ✕")
	if strings.Contains(body, "acme/tools/cli") {
		t.Fatal("topic filter")
	}
	expect(t, ts, "/-/projects?vis=public", 200, "1 project")
	expect(t, ts, "/-/projects?repo=empty", 200, "acme/empty", "1 project")
	expect(t, ts, "/-/projects?group=acme/tools", 200, "acme/tools/cli", "1 project")
	expect(t, ts, "/-/projects?q=infrastructure", 200, "2 projects")
	body = expect(t, ts, "/-/projects?sort=name", 200)
	if strings.Index(body, ">CLI<") > strings.Index(body, ">Website<") {
		t.Fatal("sort by name")
	}
	expect(t, ts, "/", 200, "Browse all projects with filters", "Users (1)", "Bots and service accounts (1)")

	// Code search: only opened repositories by default, all with all=1.
	expect(t, ts, "/-/code", 200, "Search code")
	body = expect(t, ts, "/-/code?q=hello", 200, "<mark>Hello</mark>", "src/index.html", "not been opened yet")
	if strings.Contains(body, "acme/tools/cli") {
		t.Fatal("unopened repository searched without all=1")
	}
	expect(t, ts, "/-/code?q=fmt.Println&all=1", 200, "acme/tools/cli", "main.go", "<mark>fmt.Println</mark>")
	expect(t, ts, "/-/code?q=HELLO&case=1", 200, "0 matching lines")
	expect(t, ts, "/-/code?q=h.l%2Bo&re=1&path=src/", 200, "<mark>Hello</mark>")
	expect(t, ts, "/-/code?q=git-lfs", 200, "0 matching lines") // LFS pointers are skipped
	expect(t, ts, "/-/code?q=%28&re=1", 200, "missing closing") // bad regexp reported
	expect(t, ts, "/-/code?q=about&p=acme/website", 200, "<mark>About</mark>", "In <a")
	expect(t, ts, "/acme/website", 200, `/-/code?p=acme%2fwebsite`)

	// Issues and merge requests (no repository needed for lists).
	body = expect(t, ts, "/acme/website/-/issues", 200, "Logo is blurry on retina", "Security contact", "confidential", "Zoé M.", `>Issues <span class="count">2</span>`)
	if strings.Contains(body, "not shown") {
		t.Fatal("hidden-issues notice shown to a logged-in user")
	}
	body = expect(t, ts, "/acme/website/-/issues?state=closed", 200, "Typo in footer")
	if strings.Contains(body, "Logo is blurry") {
		t.Fatal("closed filter shows open issues")
	}
	expect(t, ts, "/acme/website/-/issues?state=all&q=retina", 200, "Logo is blurry")
	body = expect(t, ts, "/acme/website/-/issues/1", 200, "<strong>logo</strong>", "<strong>Safari</strong>", "changed the description", "Ada Lovelace",
		"Only the <strong>team</strong> sees this.", ">internal</span>", "Activity (3)")
	expect(t, ts, "/acme/website/-/issues/3", 200, "Security contact", "internal only")
	if strings.Contains(body, "A note on another target") {
		t.Fatal("note of another issue shown")
	}
	expect(t, ts, "/acme/website/-/issues/99", 404)
	expect(t, ts, "/acme/website/-/merge_requests", 200, "Add dark mode", "feature/dark-mode → main")
	expect(t, ts, "/acme/website/-/merge_requests/1", 200, "Looks good, merging soon.", "src/dark.css", `background: <span class="hn">#111</span>`)
	body = expect(t, ts, "/acme/website/-/merge_requests?state=closed", 200, "Draft: Old experiment")
	if strings.Contains(body, "Draft: Draft:") || strings.Contains(body, "Draft:</span>") {
		t.Fatal("Draft prefix added on top of the title")
	}
	expect(t, ts, "/acme/empty/-/issues", 200, "Nothing here.")
	expect(t, ts, "/acme/website", 200, `Issues <span class="count">2</span>`)

	// Write methods are refused.
	resp, err := http.Post(ts.URL+"/acme/website.git/git-receive-pack", "application/x-git-receive-pack-request", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
	expect(t, ts, "/acme/website.git/config", 404)
	expect(t, ts, "/acme/website.git/objects/../config", 404)
}

func TestGitClone(t *testing.T) {
	ts, res, _ := setup(t)
	tmp := t.TempDir()
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// A raw download on a never-opened repository waits for the unbundle
	// and returns the file, not the HTML "please wait" page.
	c, body, _ := get(t, ts.URL+"/zoe/dotfiles/-/raw/main/.bashrc")
	if c != 200 || body != "export EDITOR=vim\n" {
		t.Fatalf("raw on first access: %d %q", c, body)
	}

	// Incremental chain (two bundles) cloned over the dumb protocol.
	run(tmp, "clone", "-q", ts.URL+"/acme/tools/cli.git", "cli")
	if got := run(filepath.Join(tmp, "cli"), "rev-parse", "HEAD"); got != res.CLIHead {
		t.Fatalf("cloned HEAD %s want %s", got, res.CLIHead)
	}
	run(filepath.Join(tmp, "cli"), "fsck", "--full")

	run(tmp, "clone", "-q", ts.URL+"/acme/website.git", "web")
	if got := run(filepath.Join(tmp, "web"), "branch", "-r"); !strings.Contains(got, "feature/dark-mode") {
		t.Fatalf("branches: %s", got)
	}
	if got := run(filepath.Join(tmp, "web"), "tag"); got != "v1.0.0" {
		t.Fatalf("tags: %q", got)
	}
	run(tmp, "clone", "-q", ts.URL+"/acme/website.wiki.git", "wiki")
	if _, err := os.Stat(filepath.Join(tmp, "wiki", "home.md")); err != nil {
		t.Fatal("wiki clone missing home.md")
	}
	// A project without a repository cannot be cloned.
	cmd := exec.Command("git", "clone", "-q", ts.URL+"/acme/empty.git", filepath.Join(tmp, "empty"))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err == nil {
		t.Fatal("cloning an empty project should fail")
	}
}

func TestAuth(t *testing.T) {
	ts, _, m := setup(t)
	s, _ := New(m, "test")
	c, err := ParseCredentials("me:s3cret:with-colon")
	if err != nil || c.Password != "s3cret:with-colon" {
		t.Fatalf("parse: %+v %v", c, err)
	}
	if _, err := ParseCredentials("nope"); err == nil {
		t.Fatal("expected error")
	}
	protected := httptest.NewServer(RequireAuth(s, c))
	defer protected.Close()
	_ = ts
	resp, _ := http.Get(protected.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", protected.URL+"/", nil)
	req.SetBasicAuth("me", "wrong")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	req.SetBasicAuth("me", "s3cret:with-colon")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("good password: %d", resp.StatusCode)
	}
}

func TestLFSAndUploads(t *testing.T) {
	ts, _, m := setupWith(t, func(c *app.Config) { c.LFS, c.Uploads = true, true })
	if !m.Asset("lfs").Extracted || !m.Asset("uploads").Extracted {
		t.Fatalf("assets not extracted: %+v %+v", m.Asset("lfs"), m.Asset("uploads"))
	}
	expect(t, ts, "/acme/website", 202) // unbundle
	waitBuilt(t, m, m.Index().Project("acme/website").Repo)

	// The real LFS file is shown and served, not its pointer.
	expect(t, ts, "/acme/website/-/blob/main/assets/hero.png", 200, `<span class="tag">Git LFS</span>`, "<img src=")
	c, body, hdr := get(t, ts.URL+"/acme/website/-/raw/main/assets/hero.png")
	if c != 200 || body != string(fixture.HeroPNG) || hdr.Get("Content-Type") != "image/png" || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("raw LFS: %d %q %v", c, hdr.Get("Content-Type"), len(body))
	}
	expect(t, ts, "/acme/website/-/blob/main/assets/huge.bin", 200, "not in the backup")
	_, zipBody, _ := get(t, ts.URL+"/acme/website/-/archive/main.zip")
	zr, err := zip.NewReader(bytes.NewReader([]byte(zipBody)), int64(len(zipBody)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "website-main/assets/hero.png" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(b, fixture.HeroPNG) {
				t.Fatal("zip holds the LFS pointer, not the file")
			}
		}
	}

	// Attachments in issues.
	expect(t, ts, "/acme/website/-/issues/1", 200, `src="/acme/website/-/uploads/`+fixture.UploadSecret+`/screenshot.png"`)
	c, body, _ = get(t, ts.URL+"/acme/website/-/uploads/"+fixture.UploadSecret+"/screenshot.png")
	if c != 200 || body != string(fixture.Screenshot) {
		t.Fatalf("upload: %d", c)
	}
	for _, bad := range []string{"/acme/website/-/uploads/x/..%2f..%2fetc", "/acme/website/-/uploads/" + fixture.UploadSecret + "/missing.png", "/acme/website/-/uploads/../../lfs"} {
		if c, _, _ := get(t, ts.URL+bad); c != 404 {
			t.Fatalf("%s: %d", bad, c)
		}
	}

	// Git LFS batch API: download only.
	post := func(op string) (int, string) {
		req := `{"operation":"` + op + `","transfers":["basic"],"objects":[{"oid":"` + fixture.LFSOid(fixture.HeroPNG) + `","size":1}]}`
		resp, err := http.Post(ts.URL+"/acme/website.git/info/lfs/objects/batch", "application/vnd.git-lfs+json", strings.NewReader(req))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if c, b := post("download"); c != 200 || !strings.Contains(b, "/acme/website.git/gitlab-lfs/objects/"+fixture.LFSOid(fixture.HeroPNG)) {
		t.Fatalf("batch download: %d %s", c, b)
	}
	if c, _ := post("upload"); c != 403 {
		t.Fatalf("batch upload: %d", c)
	}

	// A real git clone with git-lfs gets the file.
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not installed")
	}
	tmp := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", ts.URL+"/acme/website.git", filepath.Join(tmp, "web"))
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(tmp, "gitconfig"), "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	os.WriteFile(filepath.Join(tmp, "gitconfig"), []byte("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = false\n"), 0o644)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone with git-lfs: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "web", "assets", "hero.png"))
	if !bytes.Equal(got, fixture.HeroPNG) {
		t.Fatalf("git-lfs clone did not fetch the file (%d bytes)", len(got))
	}
}

var tokenRE = regexp.MustCompile(`name="token" value="([0-9a-f]+)"`)

func TestBackupPickerAndActions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "backups")
	day1, err := fixture.Build(filepath.Join(tmp, "b1"), fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	day2, err := fixture.BuildNext(filepath.Join(tmp, "b2"), fixture.Gitaly, fixture.Tar)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o755)
	p1 := filepath.Join(dir, filepath.Base(day1.Path))
	p2 := filepath.Join(dir, filepath.Base(day2.Path))
	os.Rename(day1.Path, p1)
	os.Rename(day2.Path, p2)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a backup"), 0o644)

	hub := app.NewHub(app.Config{DataDir: filepath.Join(tmp, "data"), Logf: t.Logf}, []string{dir})
	srv, err := NewHub(hub, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Nothing open: everything leads to the picker, newest first.
	resp, _ := http.Get(ts.URL + "/acme")
	if resp.Request.URL.Path != "/-/backups" {
		t.Fatalf("redirected to %s", resp.Request.URL.Path)
	}
	resp.Body.Close()
	body := expect(t, ts, "/-/backups", 200, filepath.Base(p1), filepath.Base(p2), "GitLab 17.5.1")
	if strings.Contains(body, "notes.txt") || strings.Index(body, filepath.Base(p2)) > strings.Index(body, filepath.Base(p1)) {
		t.Fatal("picker lists a non-backup or is not newest first")
	}
	tok := tokenRE.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no form token")
	}
	post := func(path string, form url.Values, origin string) int {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Forms without the token, from another site, or for a file outside the
	// allowed directories are refused.
	if c := post("/-/action/open", url.Values{"path": {p1}}, ""); c != 403 {
		t.Fatalf("no token: %d", c)
	}
	if c := post("/-/action/open", url.Values{"path": {p1}, "token": {tok[1]}}, "https://evil.example"); c != 403 {
		t.Fatalf("cross-site: %d", c)
	}
	if c := post("/-/action/open", url.Values{"path": {"/etc/passwd"}, "token": {tok[1]}}, ""); c != 303 || hub.Active() != nil {
		t.Fatalf("outside path: %d %v", c, hub.Active())
	}
	// Open day 2, then switch to day 1.
	if c := post("/-/action/open", url.Values{"path": {p2}, "token": {tok[1]}}, ts.URL); c != 303 {
		t.Fatalf("open: %d", c)
	}
	waitReady := func() {
		deadline := time.Now().Add(20 * time.Second)
		for {
			if m := hub.Active(); m != nil && m.Status().Ready {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("backup never became ready")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitReady()
	body = expect(t, ts, "/", 200, "acme")
	if strings.Contains(body, "dotfiles") { // deleted on day 2
		t.Fatal("day 2 should not have dotfiles")
	}
	first := hub.Active()
	if c := post("/-/action/open", url.Values{"path": {p1}, "token": {tok[1]}}, ts.URL); c != 303 {
		t.Fatalf("switch: %d", c)
	}
	waitReady()
	if hub.Active() == first {
		t.Fatal("did not switch")
	}
	expect(t, ts, "/zoe/dotfiles", 202) // day 1 has it
	expect(t, ts, "/-/backups", 200, "indexed", ">open<")

	// Actions: extract LFS from the browser, remembered for this backup.
	if c := post("/-/action/extract", url.Values{"what": {"lfs"}, "token": {tok[1]}}, ts.URL); c != 303 {
		t.Fatalf("extract: %d", c)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !hub.Active().Asset("lfs").Extracted {
		if time.Now().After(deadline) {
			t.Fatalf("LFS never extracted: %+v", hub.Active().Asset("lfs"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c := post("/-/action/prefetch", url.Values{"token": {tok[1]}}, ts.URL); c != 303 {
		t.Fatalf("prefetch: %d", c)
	}
	expect(t, ts, "/-/info", 200, "Delete cached data of other backups")
	if c := post("/-/action/prune", url.Values{"token": {tok[1]}}, ts.URL); c != 303 {
		t.Fatalf("prune: %d", c)
	}
	deadline = time.Now().Add(20 * time.Second)
	for hub.Active().Busy() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// Without a login on the network, actions are disabled.
	srv.ActionsAllowed = false
	if c := post("/-/action/prefetch", url.Values{"token": {tok[1]}}, ts.URL); c != 403 {
		t.Fatalf("disabled actions: %d", c)
	}
	body = expect(t, ts, "/-/info", 200)
	if strings.Contains(body, "/-/action/") {
		t.Fatal("action buttons shown while disabled")
	}
}

func TestLabelsAndMilestones(t *testing.T) {
	ts, _, m := setup(t)
	// The MR page starts unbundling the repository: let it finish before
	// the temporary directory is removed.
	defer waitBuilt(t, m, m.Index().Project("acme/website").Repo)
	// List: labels (project + group), milestone, link to the stylesheet.
	body := expect(t, ts, "/acme/website/-/issues", 200,
		`class="label lb-1"`, `>bug</a>`, `class="label lb-2"`, `title="UI work"`, `⚑ v1.0`, `/-/labels.css?b=`,
		`>Labels</a>`, `>Milestones</a>`)
	if strings.Contains(body, "lb-99") {
		t.Error("dangling label link shown")
	}
	// Filter by label, then by milestone.
	body = expect(t, ts, "/acme/website/-/issues?state=all&label=BUG", 200, "Logo is blurry", "label: BUG ✕")
	if strings.Contains(body, "Typo in footer") {
		t.Error("label filter let another issue through")
	}
	body = expect(t, ts, "/acme/website/-/issues?state=all&milestone=v1.0", 200, "Logo is blurry", "Typo in footer")
	if strings.Contains(body, "Security contact") {
		t.Error("milestone filter let an issue without milestone through")
	}
	expect(t, ts, "/acme/website/-/issues?state=all&milestone=none", 200, "Security contact")
	// Paging and state links keep the filters.
	expect(t, ts, "/acme/website/-/issues?label=bug", 200, `href="/acme/website/-/issues?label=bug&amp;state=closed"`)
	// MR list and page.
	expect(t, ts, "/acme/website/-/merge_requests", 200, `class="label lb-2"`, `⚑ v1.0`)
	expect(t, ts, "/acme/website/-/merge_requests/1", 200, `class="label lb-2"`, `⚑ v1.0`, `due 2024-10-31`)
	// Scoped group label inherited by a subgroup project.
	expect(t, ts, "/acme/tools/cli/-/issues/1", 200, `class="label lb-4 scoped"`, `<span class="lk">priority</span><span class="lv">high</span>`, `⚑ Q4 2024`)
	// Labels page: project and group labels with counts; another project's label absent.
	expect(t, ts, "/acme/website/-/labels", 200, ">bug</a>", "group label", "1 open issue", ">unused</a>")
	if !strings.Contains(expect(t, ts, "/acme/tools/cli/-/labels", 200, "frontend"), "priority") || strings.Contains(expect(t, ts, "/acme/tools/cli/-/labels", 200), ">bug</a>") {
		t.Error("cli labels")
	}
	// Milestones: open by default with progress, closed tab, group milestone.
	body = expect(t, ts, "/acme/website/-/milestones", 200, "v1.0", "Q4 2024", "group milestone", "50 % · 1/2 issues closed", "1 MR")
	if strings.Contains(body, "v0.9") {
		t.Error("closed milestone in the open tab")
	}
	expect(t, ts, "/acme/website/-/milestones?state=closed", 200, "v0.9")
	// Stylesheet: one class per label, unsafe colours replaced.
	css := expect(t, ts, "/-/labels.css", 200, ".lb-1{--lb:#d9534f;--lbt:#ffffff}", ".lb-2{--lb:#428bca;", ".lb-3{--lb:#69d100;--lbt:#1f1d18}")
	if strings.Contains(css, "javascript") {
		t.Error("unsafe colour copied into the stylesheet")
	}
}

func TestLFSHeadOnly(t *testing.T) {
	ts, _, m := setupWith(t, func(c *app.Config) { c.LFS, c.LFSMode = true, app.LFSHead })
	st := m.Asset("lfs")
	if !st.Extracted || st.Mode != app.LFSHead || st.Files != 1 {
		t.Fatalf("head extraction: %+v", st)
	}
	if _, ok := m.LFSObject(fixture.LFSOid(fixture.HeroPNG)); !ok {
		t.Fatal("object on the default branch not extracted")
	}
	if _, ok := m.LFSObject(fixture.LFSOid(fixture.OldLFS)); ok {
		t.Fatal("object outside default branches extracted")
	}
	expect(t, ts, "/acme/website/-/blob/main/assets/huge.bin", 200, "Only the LFS files of default branches", "Extract all (")
	expect(t, ts, "/-/info", 200, "(default branches only)")
	// Asking for everything extracts again, keeping the old files served meanwhile.
	if err := m.EnableAsset("lfs", app.LFSAll); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for st = m.Asset("lfs"); !(st.Extracted && st.Mode == app.LFSAll); st = m.Asset("lfs") {
		if time.Now().After(deadline) {
			t.Fatalf("never extracted all: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := m.LFSObject(fixture.LFSOid(fixture.OldLFS)); !ok || st.Files != 2 {
		t.Fatalf("all: %+v", st)
	}
}

func TestReleases(t *testing.T) {
	ts, _, m := setupWith(t, func(c *app.Config) { c.Uploads = true })
	expect(t, ts, "/acme/website", 202) // starts unbundling
	waitBuilt(t, m, m.Index().Project("acme/website").Repo)
	body := expect(t, ts, "/acme/website/-/releases", 200,
		"Website 1.0", `href="/acme/website/-/tree/v1.0.0"`, "First <strong>stable</strong> release.",
		`href="/acme/website/-/blob/v1.0.0/README.md"`, `href="/acme/website/-/archive/v1.0.0.zip"`,
		`href="/acme/website/-/uploads/`+fixture.UploadSecret+`/screenshot.png"`, "Screenshot",
		`href="https://downloads.example.com/website-1.0.tar.gz" rel="noopener noreferrer"`, "↗ external · package",
		">v0.9.0</h2>", "Releases <span class=\"count\">2</span>")
	if strings.Index(body, "Website 1.0") > strings.Index(body, ">v0.9.0<") {
		t.Error("releases not newest first")
	}
	if strings.Contains(body, "Orphan") {
		t.Error("link of an unknown release shown")
	}
	expect(t, ts, "/acme/tools/cli/-/releases", 200, "No releases.")
}

func TestSnippets(t *testing.T) {
	// Without issues (no user names) the pages still work.
	nts, _, _ := setupWith(t, func(c *app.Config) { c.NoIssues = true })
	expect(t, nts, "/-/snippets", 200, "Deploy script", "user #1")

	ts, _, m := setup(t)
	body := expect(t, ts, "/-/snippets", 200, "Deploy script", "Hello in Python", "Lost snippet", "not in the backup", `href="/acme/website"`, "personal")
	if strings.Index(body, "Lost snippet") > strings.Index(body, "Deploy script") {
		t.Error("snippets not newest first")
	}
	expect(t, ts, "/acme/website", 202)
	waitBuilt(t, m, m.Index().Project("acme/website").Repo)
	expect(t, ts, "/acme/website", 200, `Snippets <span class="count">1</span>`)
	body = expect(t, ts, "/acme/website/-/snippets", 200, "Deploy script")
	if strings.Contains(body, "Hello in Python") {
		t.Error("personal snippet in the project list")
	}
	// The snippet page unbundles its repository first.
	sn := m.Index().Snippet(1)
	expect(t, ts, "/-/snippets/1", 202)
	waitBuilt(t, m, sn.Repo)
	expect(t, ts, "/-/snippets/1", 200, "How we <strong>deploy</strong>.", "deploy.sh", "notes.md", "Shell",
		`<span class="hc"># deploy</span>`, "/-/snippets/1.git", `href="/acme/website/-/snippets"`)
	c, raw, hdr := get(t, ts.URL+"/-/snippets/1/raw/notes.md")
	if c != 200 || raw != "Run **after** the build.\n" || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("raw: %d %q", c, raw)
	}
	expect(t, ts, "/acme/website/-/snippets/1", 200, "Deploy script") // redirect
	expect(t, ts, "/-/snippets/3", 200, "not in the backup")
	expect(t, ts, "/-/snippets/99", 404)
	expect(t, ts, "/acme/tools/cli/-/snippets/1.git/HEAD", 404) // wrong project

	// git clone, both URL forms.
	for _, u := range []string{"/-/snippets/2.git", "/acme/website/-/snippets/1.git"} {
		dir := filepath.Join(t.TempDir(), "c")
		if out, err := exec.Command("git", "clone", "-q", ts.URL+u, dir).CombinedOutput(); err != nil {
			t.Fatalf("clone %s: %v %s", u, err, out)
		}
	}
}

func TestCompare(t *testing.T) {
	ts, res, m := setup(t)
	expect(t, ts, "/acme/website", 202)
	waitBuilt(t, m, m.Index().Project("acme/website").Repo)
	expect(t, ts, "/acme/website", 200, `href="/acme/website/-/compare"`)
	expect(t, ts, "/acme/website/-/compare", 200, `<option selected>main</option>`, "feature/dark-mode", "v1.0.0")
	// The MR branch: one commit and its two files since the merge base.
	body := expect(t, ts, "/acme/website/-/compare/main...feature/dark-mode", 200,
		"1 commit ", "2 changed files", "Add dark mode", "src/dark.css", "merge base", res.MRBase[:8])
	if strings.Contains(body, "Add about page") {
		t.Error("commit of the base listed")
	}
	// Reversed: main has nothing that the branch lacks.
	expect(t, ts, "/acme/website/-/compare/feature/dark-mode...main", 200, "0 commits", "has no commits that")
	// The form redirects; unknown refs are reported.
	expect(t, ts, "/acme/website/-/compare?from=v1.0.0&to=main", 200, "0 commits")
	expect(t, ts, "/acme/website/-/compare/main...nope", 200, "Unknown branch, tag or commit: nope")
	// git's ancestor syntax, in compare and in file URLs.
	expect(t, ts, "/acme/website/-/compare/main~1...main", 200, "1 commit ", "Add about page")
	expect(t, ts, "/acme/website/-/blob/feature/dark-mode~1/src/index.html", 200, "Hello")
	expect(t, ts, "/acme/website/-/blob/main~1/src/about.html", 404)
	expect(t, ts, "/acme/website/-/blob/main~1/README.md", 200, "ACME website")
	// Short ids and names for commits, and short ids with a path.
	short := res.MRHead[:8]
	expect(t, ts, "/acme/website/-/commit/"+short, 200, "Add dark mode")
	expect(t, ts, "/acme/website/-/commit/"+short+"~1", 200, "Add about page")
	expect(t, ts, "/acme/website/-/commit/main~1", 200, "Initial site")
	expect(t, ts, "/acme/website/-/blob/"+short+"/src/dark.css", 200, "background")
	expect(t, ts, "/acme/website/-/commit/ffff", 404)
	expect(t, ts, "/acme/website/-/tree/main~1/src", 200, "index.html")
}

func TestThemeAndPermalink(t *testing.T) {
	ts, _, m := setup(t)
	expect(t, ts, "/acme/website", 202)
	waitBuilt(t, m, m.Index().Project("acme/website").Repo)
	body := expect(t, ts, "/acme/website/-/blob/main/README.md?plain=1", 200, `data-permalink="/acme/website/-/blob/`, "Theme: auto", `href="/-/theme?back=`)
	if strings.Contains(body, "data-theme=") {
		t.Error("theme set without a choice")
	}
	// Cycle auto -> light -> dark -> auto, coming back to the page.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	for _, want := range []string{`data-theme="light"`, `data-theme="dark"`, `Theme: auto`} {
		resp, err := c.Get(ts.URL + "/-/theme?back=" + url.QueryEscape("/acme/website/-/blob/main/README.md?plain=1"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Request.URL.Path != "/acme/website/-/blob/main/README.md" || !strings.Contains(string(b), want) {
			t.Fatalf("theme: landed on %s, want %q", resp.Request.URL, want)
		}
	}
	// Only local paths are accepted to come back to.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(ts.URL + "/-/theme?back=//evil.example")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("open redirect: %q", resp.Header.Get("Location"))
	}
}

func TestHomeWhileImporting(t *testing.T) {
	ts, res, m := setup(t)
	_ = ts
	src, err := backup.OpenSource(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	d := homeFor(index.Partial(src, "fp"))
	if !d.Pending || d.HashOnly != 7 || len(d.ByHash) != 7 {
		t.Fatalf("%+v", d)
	}
	s, _ := New(m, "test")
	var b strings.Builder
	if err := s.tmpl.ExecuteTemplate(&b, "home.html", page{Data: d, Index: index.Partial(src, "fp")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Repositories found so far") || strings.Contains(b.String(), "No groups.") {
		t.Fatal(b.String())
	}
}

func TestSmartProtocol(t *testing.T) {
	ts, res, m := setup(t)
	tmp := t.TempDir()
	git := func(dir string, env []string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, env []string, args ...string) string {
		out, err := git(dir, env, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}

	// The advertisement does not wait for the unbundle.
	key := m.Index().Project("acme/tools/cli").Repo
	c, body, hdr := get(t, ts.URL+"/acme/tools/cli.git/info/refs?service=git-upload-pack")
	if c != 200 || hdr.Get("Content-Type") != "application/x-git-upload-pack-advertisement" ||
		!strings.Contains(body, "symref=HEAD:refs/heads/main") || !strings.Contains(body, res.CLIHead+" refs/heads/main") {
		t.Fatalf("advertisement: %d %q", c, body)
	}

	// Clone over the smart protocol, with the unbundle progress shown.
	// acme/tools/cli is an incremental chain: several packs joined in one.
	trace := []string{"GIT_TRACE_CURL=1", "GIT_TRACE_CURL_NO_DATA=1"}
	out := must(tmp, trace, "clone", "--progress", ts.URL+"/acme/tools/cli.git", "cli")
	if !strings.Contains(out, "POST /acme/tools/cli.git/git-upload-pack") {
		t.Fatalf("not cloned over the smart protocol:\n%s", out)
	}
	repo, release, err := m.Repo(key)
	if err != nil {
		t.Fatal(err)
	}
	packs := len(repo.PackPaths())
	release()
	if packs < 2 {
		t.Errorf("cli has %d pack(s): the join of several packs is not tested", packs)
	}
	cli := filepath.Join(tmp, "cli")
	if got := strings.TrimSpace(must(cli, nil, "rev-parse", "HEAD")); got != res.CLIHead {
		t.Fatalf("HEAD %s want %s", got, res.CLIHead)
	}
	must(cli, nil, "fsck", "--full", "--strict")

	// A fetch with haves negotiates and gets everything again.
	must(cli, nil, "update-ref", "-d", "refs/remotes/origin/main")
	must(cli, nil, "fetch", "-q", "origin")
	if got := strings.TrimSpace(must(cli, nil, "rev-parse", "origin/main")); got != res.CLIHead {
		t.Fatalf("fetched origin/main %s", got)
	}
	must(cli, nil, "fsck", "--full")
	if out := must(cli, nil, "fetch", "origin"); strings.TrimSpace(out) != "" {
		t.Errorf("up-to-date fetch printed %q", out)
	}

	// Tags (annotated, peeled) and a wiki.
	web := filepath.Join(tmp, "web")
	must(tmp, nil, "clone", "-q", ts.URL+"/acme/website.git", web)
	if got := strings.TrimSpace(must(web, nil, "tag")); got != "v1.0.0" {
		t.Fatalf("tags: %q", got)
	}
	must(tmp, nil, "clone", "-q", ts.URL+"/acme/website.wiki.git", filepath.Join(tmp, "wiki"))

	// Shallow clones are refused clearly; pushing is refused.
	if out, err := git(tmp, nil, "clone", "--depth", "1", ts.URL+"/acme/website.git", "shallow"); err == nil ||
		!strings.Contains(out, "shallow") {
		t.Fatalf("shallow clone: %v\n%s", err, out)
	}
	expect(t, ts, "/acme/website.git/info/refs?service=git-receive-pack", 403)
	must(web, []string{"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b"}, "commit", "--allow-empty", "-q", "-m", "x")
	if _, err := git(web, nil, "push", "origin", "HEAD:main"); err == nil {
		t.Fatal("push accepted")
	}
	// A broken request.
	resp, err := http.Post(ts.URL+"/acme/website.git/git-upload-pack", "application/x-git-upload-pack-request", strings.NewReader("zzzz"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad request: %d", resp.StatusCode)
	}
}
