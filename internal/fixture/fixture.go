// Package fixture fabricates small but realistic GitLab backups for tests
// and demos. It needs the git command-line tool.
package fixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Layout selects how repositories are laid out in the backup.
type Layout int

const (
	Gitaly  Layout = iota // GitLab 15+: <hash>.git/<backup_id>/NNN.bundle + NNN.refs
	Classic               // GitLab 13-15: <hash>.bundle
)

// Era selects which GitLab generation the backup imitates: the database
// schema, the storage paths and backup_information.yml differ.
type Era int

const (
	// Era17 is the default: hashed storage, state_id, draft, internal
	// notes, topics, snippet repositories, one uploads table.
	Era17 Era = iota
	// Era12 is an old GitLab: legacy storage (repositories under their full
	// path, no project_repositories table), text state columns, drafts as
	// "WIP:" titles, confidential notes, bot_type, no topics, snippets
	// without repositories.
	Era12
	// Era19 is a recent GitLab: attachments split into project_uploads,
	// user_uploads..., and the multi-line backup date of GitLab 19.
	Era19
)

// Format selects the container.
type Format int

const (
	Tar Format = iota
	TarGz
	Dir
)

const (
	backupID     = "1730000000_2024_10_27_17.5.1"
	nextBackupID = "1730086400_2024_10_28_17.5.1"
)

// Project ids used by the fixture.
const (
	WebsiteID  = 1
	CLIID      = 2
	DotfilesID = 3
	EmptyID    = 4
	OrphanID   = 99
)

// SnippetPath is where GitLab stores the repository of snippet id.
func SnippetPath(id int) string { return "@snippets/" + strings.TrimPrefix(hashed(id), "@hashed/") }

func hashed(id int) string {
	s := sha256.Sum256([]byte(fmt.Sprint(id)))
	h := hex.EncodeToString(s[:])
	return "@hashed/" + h[:2] + "/" + h[2:4] + "/" + h
}

// Result describes what was generated.
type Result struct {
	Path       string // backup tar / tgz / dir
	Next       bool   // built as "the next day's" backup (see BuildNext)
	Era        Era
	CLIHead    string // expected main commit of acme/tools/cli after the incremental
	MRBase     string // base and head commits of acme/website!1
	MRHead     string
	WebsiteDir string // working copy of acme/website (for content checks)
}

type gen struct {
	work, stage string
	layout      Layout
	id          string
}

func (g *gen) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada Lovelace", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada Lovelace", "GIT_COMMITTER_EMAIL=ada@example.com",
		"GIT_AUTHOR_DATE=2024-10-01T10:00:00+0200", "GIT_COMMITTER_DATE=2024-10-01T10:00:00+0200",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *gen) repo(name string, files map[string]string, msg string) (string, error) {
	dir := filepath.Join(g.work, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := g.git(dir, "init", "-q", "-b", "main"); err != nil {
		return "", err
	}
	return dir, g.commit(dir, files, msg)
}

func (g *gen) commit(dir string, files map[string]string, msg string) error {
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			return err
		}
	}
	if _, err := g.git(dir, "add", "-A"); err != nil {
		return err
	}
	_, err := g.git(dir, "commit", "-q", "-m", msg)
	return err
}

// place writes a bundle (and refs file) for disk path key at sequence seq.
func (g *gen) place(dir, key string, seq int, revs ...string) error {
	var target, refsTarget string
	if g.layout == Classic {
		target = filepath.Join(g.stage, "repositories", key+".bundle")
	} else {
		base := filepath.Join(g.stage, "repositories", "default", key+".git", g.id)
		target = filepath.Join(base, fmt.Sprintf("%03d.bundle", seq))
		refsTarget = filepath.Join(base, fmt.Sprintf("%03d.refs", seq))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	args := append([]string{"bundle", "create", "-q", target}, revs...)
	if _, err := g.git(dir, args...); err != nil {
		return err
	}
	if refsTarget != "" {
		out, err := g.git(dir, "for-each-ref", "--format=%(objectname) %(refname)")
		if err != nil {
			return err
		}
		head, _ := g.git(dir, "rev-parse", "HEAD")
		content := out + "\n" + head + " HEAD\n"
		if err := os.WriteFile(refsTarget, []byte(content), 0o644); err != nil {
			return err
		}
		os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(target)), "LATEST"), []byte(g.id+"\n"), 0o644)
		os.WriteFile(filepath.Join(filepath.Dir(target), "LATEST"), []byte(fmt.Sprintf("%03d\n", seq)), 0o644)
	}
	return nil
}

// Build creates a backup under workdir and returns where it is.
func Build(workdir string, layout Layout, format Format) (*Result, error) {
	return build(workdir, layout, format, false, Era17)
}

// BuildEra is Build for another GitLab generation (see Era).
func BuildEra(workdir string, layout Layout, format Format, era Era) (*Result, error) {
	return build(workdir, layout, format, false, era)
}

// BuildNext creates the backup of "the next day", to compare with Build's:
// zoe/dotfiles was deleted, acme/website's main went back one commit,
// its tag v1.0.0 was deleted and feature/dark-mode was force-pushed,
// acme/tools/cli moved forward normally, and one issue was deleted.
func BuildNext(workdir string, layout Layout, format Format) (*Result, error) {
	return build(workdir, layout, format, true, Era17)
}

func build(workdir string, layout Layout, format Format, next bool, era Era) (*Result, error) {
	// git runs in each repository's own directory: relative paths given to
	// it would resolve from there.
	workdir, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	g := &gen{work: filepath.Join(workdir, "work"), stage: filepath.Join(workdir, "stage"), layout: layout, id: backupID}
	if next {
		g.id = nextBackupID
	}
	res := &Result{Next: next, Era: era}
	// Where each project's repository lives: hashed, or its full path on
	// old legacy storage.
	store := func(id int, fullPath string) string {
		if era == Era12 {
			return fullPath
		}
		return hashed(id)
	}

	web, err := g.repo("website", map[string]string{
		"README.md":           "# ACME website\n\nThe **public** site.\n\n- fast\n- static\n",
		"src/index.html":      "<h1>Hello</h1>\n",
		"assets/logo.bin":     string([]byte{0, 1, 2, 3, 255, 254}),
		"docs/guide/intro.md": "Intro\n",
		"assets/hero.png":     lfsPointer(HeroPNG),
		".gitattributes":      "assets/hero.png filter=lfs diff=lfs merge=lfs -text\n",
		"assets/huge.bin":     lfsPointer(missingLFS), // pointer to an object not in lfs.tar.gz
	}, "Initial site")
	if err != nil {
		return nil, err
	}
	if err := g.commit(web, map[string]string{"src/about.html": "<p>About</p>\n"}, "Add about page\n\nWith a body."); err != nil {
		return nil, err
	}
	g.git(web, "tag", "-a", "v1.0.0", "-m", "First release")
	// A merge request: feature/dark-mode adds a stylesheet.
	mrBase, _ := g.git(web, "rev-parse", "main")
	g.git(web, "checkout", "-q", "-b", "feature/dark-mode")
	if err := g.commit(web, map[string]string{"src/dark.css": "body { background: #111; color: #eee; }\n", "src/index.html": "<h1>Hello</h1>\n<link rel=stylesheet href=dark.css>\n"}, "Add dark mode"); err != nil {
		return nil, err
	}
	mrHead, _ := g.git(web, "rev-parse", "HEAD")
	g.git(web, "checkout", "-q", "main")
	g.git(web, "update-ref", "refs/merge-requests/1/head", mrHead)
	g.git(web, "update-ref", "refs/keep-around/"+mrHead, mrHead)
	g.git(web, "update-ref", "refs/pipelines/42", mrHead)
	res.MRBase, res.MRHead = mrBase, mrHead
	g.git(web, "branch", "fix/typo") // source of the merged MR !3, deleted the next day
	if next {
		g.git(web, "branch", "-D", "fix/typo")
		g.git(web, "tag", "-d", "v1.0.0")
		g.git(web, "reset", "-q", "--hard", "HEAD~1")
		g.git(web, "checkout", "-q", "feature/dark-mode")
		g.git(web, "commit", "-q", "--amend", "-m", "Dark mode, take two")
		g.git(web, "checkout", "-q", "main")
	}
	res.WebsiteDir = web
	if err := g.place(web, store(WebsiteID, "acme/website"), 1, "--all"); err != nil {
		return nil, err
	}
	wiki, err := g.repo("website.wiki", map[string]string{"home.md": "# Wiki home\n"}, "Wiki")
	if err != nil {
		return nil, err
	}
	if err := g.place(wiki, store(WebsiteID, "acme/website")+".wiki", 1, "--all"); err != nil {
		return nil, err
	}

	cli, err := g.repo("cli", map[string]string{"main.go": "package main\n\nfunc main() {}\n", "README.md": "# cli\n"}, "Start CLI")
	if err != nil {
		return nil, err
	}
	if err := g.place(cli, store(CLIID, "acme/tools/cli"), 1, "--all"); err != nil {
		return nil, err
	}
	base, _ := g.git(cli, "rev-parse", "main")
	if err := g.commit(cli, map[string]string{"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n"}, "Say hi"); err != nil {
		return nil, err
	}
	if next {
		if err := g.commit(cli, map[string]string{"CHANGELOG.md": "next day\n"}, "Next day"); err != nil {
			return nil, err
		}
	}
	res.CLIHead, _ = g.git(cli, "rev-parse", "main")
	if layout == Gitaly {
		if err := g.place(cli, store(CLIID, "acme/tools/cli"), 2, base+"..main"); err != nil {
			return nil, err
		}
	} else {
		// Classic backups are always full.
		if err := g.place(cli, store(CLIID, "acme/tools/cli"), 1, "--all"); err != nil {
			return nil, err
		}
	}

	if !next {
		dot, err := g.repo("dotfiles", map[string]string{".bashrc": "export EDITOR=vim\n"}, "dotfiles")
		if err != nil {
			return nil, err
		}
		if err := g.place(dot, store(DotfilesID, "zoe/dotfiles"), 1, "--all"); err != nil {
			return nil, err
		}
	}
	// Snippets: one of acme/website, one personal (zoe).
	for _, sn := range []struct {
		id    int
		files map[string]string
	}{
		{1, map[string]string{"deploy.sh": "#!/bin/sh\nrsync -a public/ web:/srv/www/ # deploy\n", "notes.md": "Run **after** the build.\n"}},
		{2, map[string]string{"hello.py": "def hello():\n    return \"hi\"\n"}},
	} {
		if era == Era12 { // snippets kept their content in the database
			break
		}
		dir, err := g.repo(fmt.Sprintf("snippet%d", sn.id), sn.files, "Snippet")
		if err != nil {
			return nil, err
		}
		if err := g.place(dir, SnippetPath(sn.id), 1, "--all"); err != nil {
			return nil, err
		}
	}
	orphan, err := g.repo("orphan", map[string]string{"lost.txt": "nobody owns me\n"}, "orphan")
	if err != nil {
		return nil, err
	}
	if err := g.place(orphan, hashed(OrphanID), 1, "--all"); err != nil {
		return nil, err
	}

	if err := writeDB(g.stage, res); err != nil {
		return nil, err
	}
	created := map[bool]string{false: "2024-10-27", true: "2024-10-28"}[next]
	info := ":db_version: 20241010000000\n:backup_created_at: " + created + " 10:00:00 UTC\n:gitlab_version: 17.5.1\n:tar_version: tar (GNU tar) 1.34\n:installation_type: omnibus-gitlab\n:skipped: ''\n"
	switch era {
	case Era12:
		info = ":db_version: 20190722000000\n:backup_created_at: " + created + " 10:00:00 UTC\n:gitlab_version: 12.1.0\n:tar_version: tar (GNU tar) 1.30\n:skipped: ''\n"
	case Era19:
		info = ":db_version: 20251020000000\n:backup_created_at: !ruby/object:ActiveSupport::TimeWithZone\n  utc: " + created + " 10:00:00.123456789 Z\n  zone: !ruby/object:ActiveSupport::TimeZone\n    name: Etc/UTC\n  time: " + created + " 10:00:00.123456789 Z\n:gitlab_version: 19.5.3\n:tar_version: tar (GNU tar) 1.35\n:installation_type: omnibus-gitlab\n:skipped: ''\n"
	}
	os.WriteFile(filepath.Join(g.stage, "backup_information.yml"), []byte(info), 0o644)
	// Git LFS objects and issue attachments, laid out as GitLab backs them up.
	if err := writeTarGz(filepath.Join(g.stage, "lfs.tar.gz"), map[string][]byte{
		lfsStoragePath(HeroPNG): HeroPNG,
		lfsStoragePath(OldLFS):  OldLFS,
	}); err != nil {
		return nil, err
	}
	if err := writeTarGz(filepath.Join(g.stage, "uploads.tar.gz"), map[string][]byte{
		store(WebsiteID, "acme/website") + "/" + UploadSecret + "/screenshot.png": Screenshot,
		"-/system/user/avatar/1/avatar.png":                                       []byte("avatar"),
	}); err != nil {
		return nil, err
	}
	// Noise a real backup contains.
	os.WriteFile(filepath.Join(g.stage, "artifacts.tar.gz"), []byte("not needed"), 0o644)

	switch format {
	case Dir:
		res.Path = g.stage
	case Tar, TarGz:
		ext := ".tar"
		if format == TarGz {
			ext = ".tar.gz"
		}
		res.Path = filepath.Join(workdir, g.id+"_gitlab_backup"+ext)
		if err := tarDir(g.stage, res.Path, format == TarGz); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// Content used for Git LFS and attachments.
var (
	HeroPNG      = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("hero image bytes "), 300)...)
	Screenshot   = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("screenshot "), 50)...)
	missingLFS   = []byte("this object is not in lfs.tar.gz")
	OldLFS       = []byte("an LFS object no default branch points to any more")
	UploadSecret = "0123456789abcdef0123456789abcdef"
)

// LFSOid is the Git LFS object id of content.
func LFSOid(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func lfsPointer(content []byte) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", LFSOid(content), len(content))
}

func lfsStoragePath(content []byte) string {
	oid := LFSOid(content)
	return oid[:2] + "/" + oid[2:4] + "/" + oid[4:]
}

// writeTarGz writes files into a gzipped tar with GitLab's "./" prefixes.
func writeTarGz(dst string, files map[string][]byte) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeDB(stage string, res *Result) error {
	sql := `--
-- PostgreSQL database dump
--
SET client_encoding = 'UTF8';

COPY public.ci_builds (id, name, token_encrypted) FROM stdin;
1	build	c2VjcmV0
\.

COPY public.namespace_details (namespace_id, description, created_at) FROM stdin;
2	\N	2024-01-01
3	\N	2024-01-01
\.

COPY public.namespaces (id, name, path, owner_id, created_at, type, description, parent_id, traversal_ids) FROM stdin;
2	ACME	acme	\N	2024-01-01 00:00:00	Group	ACME Corp	\N	{2}
3	Tools	tools	\N	2024-01-01 00:00:00	Group	Internal tools	2	{2,3}
4	Zoé	zoe	1	2024-01-01 00:00:00	User		\N	{4}
6	renovate-bot	renovate-bot	3	2024-01-01 00:00:00	User	\N	\N	{6}
5	cli	cli	\N	2024-01-01 00:00:00	Project	\N	3	{2,3,5}
\.

COPY public.project_repositories (id, shard_id, disk_path, project_id, object_format) FROM stdin;
1	1	` + hashed(WebsiteID) + `	1	0
2	1	` + hashed(CLIID) + `	2	0
3	1	` + hashed(DotfilesID) + `	3	0
4	1	` + hashed(EmptyID) + `	4	0
\.

COPY public.projects (id, name, path, description, created_at, updated_at, creator_id, namespace_id, last_activity_at, visibility_level, archived, star_count, pending_delete) FROM stdin;
1	Website	website	Company website\twith tab	2024-01-01 00:00:00	2024-10-01 00:00:00	1	2	2024-10-01 00:00:00	20	f	3	f
2	CLI	cli	Command line tool	2024-01-01 00:00:00	2024-10-01 00:00:00	1	3	2024-10-02 00:00:00	0	f	0	f
3	dotfiles	dotfiles	\N	2024-01-01 00:00:00	2024-10-01 00:00:00	1	4	2024-10-03 00:00:00	0	t	0	f
4	Empty	empty	Never pushed	2024-01-01 00:00:00	2024-10-01 00:00:00	1	2	2024-10-04 00:00:00	10	f	0	f
5	renovate-config	renovate-config	Bot settings	2024-01-01 00:00:00	2024-10-01 00:00:00	3	6	2024-09-01 00:00:00	0	f	0	f
\.

COPY public.routes (id, source_id, source_type, path, created_at, updated_at, name, namespace_id) FROM stdin;
1	2	Namespace	acme	2024-01-01	2024-01-01	ACME	2
2	3	Namespace	acme/tools	2024-01-01	2024-01-01	ACME / Tools	3
3	4	Namespace	zoe	2024-01-01	2024-01-01	Zoé	4
4	1	Project	acme/website	2024-01-01	2024-01-01	ACME / Website	\N
5	2	Project	acme/tools/cli	2024-01-01	2024-01-01	ACME / Tools / CLI	\N
6	3	Project	zoe/dotfiles	2024-01-01	2024-01-01	Zoé / dotfiles	\N
7	4	Project	acme/empty	2024-01-01	2024-01-01	ACME / Empty	\N
8	6	Namespace	renovate-bot	2024-01-01	2024-01-01	renovate-bot	6
9	5	Project	renovate-bot/renovate-config	2024-01-01	2024-01-01	renovate-bot / renovate-config	\N
\.

COPY public.users (id, email, encrypted_password, name, username, otp_secret, user_type) FROM stdin;
1	zoe@example.com	$2a$10$abcdefghijklmnopqrstuv	Zoé M.	zoe	SECRET	\N
2	ada@example.com	$2a$10$zzzzzzzzzzzzzzzzzzzzzz	Ada Lovelace	ada	\N	0
3	bot@example.com	\N	Renovate Bot	renovate-bot	\N	6
\.

COPY public.topics (id, name, title, total_projects_count) FROM stdin;
1	web	\N	1
2	infrastructure	Infrastructure	2
\.

COPY public.lfs_objects (id, oid, size, created_at, updated_at, file, file_store) FROM stdin;
1	` + LFSOid(HeroPNG) + `	` + fmt.Sprint(len(HeroPNG)) + `	2024-01-01	2024-01-01	\N	1
2	` + LFSOid(OldLFS) + `	` + fmt.Sprint(len(OldLFS)) + `	2024-01-01	2024-01-01	\N	\N
3	` + LFSOid(missingLFS) + `	32	2024-01-01	2024-01-01	\N	2
\.

COPY public.uploads (id, size, path, checksum, model_id, model_type, uploader, created_at, store, mount_point, secret) FROM stdin;
1	600	` + UploadSecret + `/screenshot.png	\N	1	Project	FileUploader	2024-09-01	1	\N	` + UploadSecret + `
2	6	uploads/-/system/user/avatar/1/avatar.png	\N	1	User	AvatarUploader	2024-01-01	\N	avatar	\N
3	9	uploads/-/system/user/avatar/2/avatar.png	\N	2	User	AvatarUploader	2024-01-01	2	avatar	\N
4	9	whatever.bin	\N	1	AbuseReport	SomethingUploader	2024-01-01	1	\N	\N
\.

COPY public.project_uploads (id, size, path, checksum, model_id, uploader, created_at, store, mount_point, secret, project_id) FROM stdin;
1	600	` + UploadSecret + `/screenshot.png	\N	1	FileUploader	2024-09-01	1	\N	` + UploadSecret + `	1
\.

COPY public.user_uploads (id, size, path, checksum, model_id, model_type, uploader, created_at, store, mount_point, secret, uploaded_by_user_id) FROM stdin;
2	6	uploads/-/system/user/avatar/1/avatar.png	\N	1	User	AvatarUploader	2024-01-01	\N	avatar	\N	1
3	9	uploads/-/system/user/avatar/2/avatar.png	\N	2	User	AvatarUploader	2024-01-01	2	avatar	\N	2
\.

COPY public.abuse_report_uploads (id, size, path, checksum, model_id, model_type, uploader, created_at, store, mount_point, secret) FROM stdin;
4	9	whatever.bin	\N	1	AbuseReport	SomethingUploader	2024-01-01	1	\N	\N
\.

COPY public.bulk_import_export_uploads (id, export_id, export_file, created_at) FROM stdin;
1	1	export.tar.gz	2024-01-01
\.

COPY public.lfs_objects_projects (id, lfs_object_id, project_id, repository_type) FROM stdin;
1	1	1	\N
2	2	1	\N
\.

COPY public.snippet_repositories (snippet_id, shard_id, disk_path, verification_checksum) FROM stdin;
1	1	` + SnippetPath(1) + `	\N
\.

COPY public.snippets (id, title, content, author_id, project_id, created_at, updated_at, file_name, type, visibility_level, description, secret) FROM stdin;
1	Deploy script	\N	1	1	2024-09-10 10:00:00	2024-09-11 10:00:00	deploy.sh	ProjectSnippet	0	How we **deploy**.	f
2	Hello in Python	\N	2	\N	2024-09-12 10:00:00	2024-09-12 10:00:00	hello.py	PersonalSnippet	20	\N	f
3	Lost snippet	print(1)	2	\N	2024-09-13 10:00:00	2024-09-13 10:00:00	x.py	PersonalSnippet	0	\N	f
\.

COPY public.release_links (id, release_id, url, name, created_at, updated_at, filepath, link_type) FROM stdin;
1	1	https://gitlab.example.com/acme/website/uploads/` + UploadSecret + `/screenshot.png	Screenshot	2024-10-01	2024-10-01	\N	image
2	1	https://downloads.example.com/website-1.0.tar.gz	Tarball	2024-10-01	2024-10-01	/website.tar.gz	package
3	9	https://example.com/orphan	Orphan	2024-10-01	2024-10-01	\N	other
\.

COPY public.releases (id, tag, description, project_id, created_at, updated_at, description_html, cached_markdown_version, author_id, name, sha, released_at) FROM stdin;
1	v1.0.0	First **stable** release.\n\nSee the [README](README.md).	1	2024-10-01 10:00:00	2024-10-01 10:00:00	<p>x</p>	1	1	Website 1.0	\N	2024-10-01 12:00:00
2	v0.9.0	\N	1	2024-08-01 10:00:00	2024-08-01 10:00:00	\N	1	2	\N	\N	\N
\.

COPY public.project_topics (id, project_id, topic_id) FROM stdin;
1	1	1
2	1	2
3	2	2
\.

COPY public.issues (id, title, author_id, project_id, created_at, updated_at, description, iid, state_id, closed_at, confidential, milestone_id) FROM stdin;
10	Logo is blurry on retina	1	1	2024-09-01 10:00:00.123	2024-09-02 10:00:00	The **logo** looks fuzzy.\n\n- on iPhone\n- on Mac\n\n![shot](/uploads/` + UploadSecret + `/screenshot.png)	1	1	\N	f	1
11	Typo in footer	2	1	2024-09-03 10:00:00	2024-09-04 10:00:00	"Copyrigth"	2	2	2024-09-04 10:00:00	f	1
12	Security contact	1	1	2024-09-05 10:00:00	2024-09-05 10:00:00	internal only	3	1	\N	t	\N
20	CLI crashes without args	2	2	2024-09-06 10:00:00	2024-09-06 10:00:00	Panic	1	1	\N	f	2
\.

COPY public.label_links (id, label_id, target_id, target_type, created_at) FROM stdin;
1	1	10	Issue	2024-09-01 10:00:00
2	2	10	Issue	2024-09-01 10:00:00
3	3	11	Issue	2024-09-03 10:00:00
4	2	50	MergeRequest	2024-09-07 10:00:00
5	4	20	Issue	2024-09-06 10:00:00
6	99	10	Issue	2024-09-06 10:00:00
\.

COPY public.labels (id, title, color, project_id, created_at, template, description, type, group_id, text_color) FROM stdin;
1	bug	#d9534f	1	2024-01-01	f	Something is broken	ProjectLabel	\N	\N
2	frontend	#428BCA	\N	2024-01-01	f	UI work	GroupLabel	2	\N
3	good first issue	#69d100	1	2024-01-01	f	\N	ProjectLabel	\N	\N
4	priority::high	#ff0000	\N	2024-01-01	f	\N	GroupLabel	2	\N
5	unused	javascript:x	1	2024-01-01	f	\N	ProjectLabel	\N	\N
\.

COPY public.merge_request_diffs (id, state, merge_request_id, created_at, updated_at, base_commit_sha, real_size, head_commit_sha, start_commit_sha) FROM stdin;
100	collected	50	2024-09-07 10:00:00	2024-09-07 10:00:00	OLDBASE	1	OLDHEAD	OLDBASE
101	collected	50	2024-09-08 10:00:00	2024-09-08 10:00:00	` + res.MRBase + `	2	` + res.MRHead + `	` + res.MRBase + `
\.

COPY public.merge_requests (id, target_branch, source_branch, source_project_id, author_id, title, created_at, updated_at, target_project_id, iid, description, state_id, merge_commit_sha, draft, milestone_id) FROM stdin;
50	main	feature/dark-mode	1	2	Add dark mode	2024-09-07 10:00:00	2024-09-08 10:00:00	1	1	Closes #1 (sort of).	1	\N	f	1
52	main	fix/typo	1	2	Fix typo in footer	2024-10-20 10:00:00	2024-10-21 10:00:00	1	3	Closes #2	3	\N	f	\N
51	main	old-thing	1	1	Draft: Old experiment	2024-08-01 10:00:00	2024-08-02 10:00:00	1	2	\N	2	\N	t	\N
\.

COPY public.milestones (id, title, project_id, description, due_date, created_at, updated_at, state, iid, start_date, group_id) FROM stdin;
1	v1.0	1	First stable release	2024-10-31	2024-01-01	2024-01-01	active	1	2024-09-01	\N
2	Q4 2024	\N	Quarter goals	2024-12-31	2024-01-01	2024-01-01	active	1	\N	2
3	v0.9	1	\N	2024-08-31	2024-01-01	2024-01-01	closed	2	\N	\N
\.

COPY public.notes (id, note, noteable_type, author_id, created_at, updated_at, project_id, commit_id, noteable_id, system, internal) FROM stdin;
1000	I can reproduce on **Safari**.	Issue	2	2024-09-01 11:00:00	2024-09-01 11:00:00	1	\N	10	f	f
1001	changed the description	Issue	1	2024-09-01 12:00:00	2024-09-01 12:00:00	1	\N	10	t	f
1002	Looks good, merging soon.	MergeRequest	1	2024-09-08 11:00:00	2024-09-08 11:00:00	1	\N	50	f	f
1003	A note on another target	Issue	1	2024-09-08 11:00:00	2024-09-08 11:00:00	1	\N	100	f	f
1004	Only the **team** sees this.	Issue	1	2024-09-01 13:00:00	2024-09-01 13:00:00	1	\N	10	f	t
\.

--
-- PostgreSQL database dump complete
--
`
	sql = eraSQL(sql, res.Era)
	if res.Next { // the next day: dotfiles deleted, issue #2 of the website deleted
		var kept []string
		for _, line := range strings.Split(sql, "\n") {
			if strings.HasPrefix(line, "3\t1\t"+hashed(DotfilesID)) || strings.HasPrefix(line, "3\tdotfiles\t") ||
				strings.HasPrefix(line, "6\t3\tProject\t") || strings.HasPrefix(line, "11\tTypo in footer") {
				continue
			}
			kept = append(kept, line)
		}
		sql = strings.Join(kept, "\n")
	}
	dir := filepath.Join(stage, "db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "database.sql.gz"))
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	io.WriteString(gz, sql)
	gz.Close()
	return f.Close()
}

func tarDir(src, dst string, compress bool) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	var w io.Writer = f
	var gz *gzip.Writer
	if compress {
		gz = gzip.NewWriter(f)
		w = gz
	}
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = "./" + filepath.ToSlash(rel) // GitLab tars use ./ prefixes
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, in)
			in.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if gz != nil {
		return gz.Close()
	}
	return nil
}

// eraSQL rewrites the (current) dump into the schema of an older or newer
// GitLab.
func eraSQL(sql string, era Era) string {
	switch era {
	case Era12:
		sql = dropCopy(sql, "namespace_details", "project_repositories", "topics", "project_topics", "snippet_repositories",
			"project_uploads", "user_uploads", "abuse_report_uploads")
		state := func(m map[string]string) func(string) string {
			return func(v string) string { return m[v] }
		}
		sql = editCopy(sql, "issues", "state_id", "state", state(map[string]string{"1": "opened", "2": "closed"}))
		sql = editCopy(sql, "merge_requests", "state_id", "state", state(map[string]string{"1": "opened", "2": "closed", "3": "merged", "4": "locked"}))
		sql = editCopy(sql, "merge_requests", "draft", "", nil)
		sql = strings.Replace(sql, "Draft: Old experiment", "WIP: Old experiment", 1)
		sql = editCopy(sql, "notes", "internal", "confidential", func(v string) string { return v })
		sql = editCopy(sql, "users", "user_type", "bot_type", func(v string) string {
			if v == "0" {
				return `\N`
			}
			return v
		})
	case Era19:
		sql = dropCopy(sql, "uploads")
		// Group descriptions moved to namespace_details.
		sql = editCopy(sql, "namespaces", "description", "", nil)
		sql = strings.Replace(sql, "2\t\\N\t2024-01-01\n3\t\\N\t", "2\tACME Corp\t2024-01-01\n3\tInternal tools\t", 1)
	}
	return sql
}

// dropCopy removes the COPY blocks of tables.
func dropCopy(sql string, tables ...string) string {
	for _, t := range tables {
		start := strings.Index(sql, "COPY public."+t+" (")
		if start < 0 {
			continue
		}
		end := strings.Index(sql[start:], "\\.\n")
		sql = sql[:start] + sql[start+end+3:]
	}
	return sql
}

// editCopy renames column col of table to newName ("" removes it),
// rewriting its values with fn.
func editCopy(sql, table, col, newName string, fn func(string) string) string {
	start := strings.Index(sql, "COPY public."+table+" (")
	if start < 0 {
		return sql
	}
	end := start + strings.Index(sql[start:], "\\.\n")
	lines := strings.Split(sql[start:end], "\n")
	head := lines[0]
	open, close := strings.IndexByte(head, '('), strings.IndexByte(head, ')')
	cols := strings.Split(head[open+1:close], ", ")
	idx := -1
	for i, c := range cols {
		if c == col {
			idx = i
		}
	}
	if idx < 0 {
		return sql
	}
	edit := func(fields []string) []string {
		if newName == "" {
			return append(fields[:idx:idx], fields[idx+1:]...)
		}
		fields[idx] = fn(fields[idx])
		return fields
	}
	if newName == "" {
		cols = append(cols[:idx:idx], cols[idx+1:]...)
	} else {
		cols[idx] = newName
	}
	lines[0] = head[:open+1] + strings.Join(cols, ", ") + head[close:]
	for i := 1; i < len(lines); i++ {
		if lines[i] == "" {
			continue
		}
		lines[i] = strings.Join(edit(strings.Split(lines[i], "\t")), "\t")
	}
	return sql[:start] + strings.Join(lines, "\n") + sql[end:]
}
