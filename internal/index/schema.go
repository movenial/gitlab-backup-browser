package index

import (
	"sort"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/pgdump"
)

// The database schema gbb reads, in one place. GitLab adds, renames and
// splits tables between versions; describing what is read (and what each
// column is for) lets the import pick older or newer column names, and
// lets gbb say what it could not find instead of showing wrong data. The
// same list is checked in CI against GitLab's db/structure.sql.

// Need says what happens when a table or column is missing.
type Need int

const (
	// Needed: the data shown would be wrong or missing (a warning).
	Needed Need = iota
	// Nice: a feature is degraded or a fallback is used (a note).
	Nice
)

// Column is a column gbb reads; Alts are the names it had in other GitLab
// versions (tried in order when Name is absent).
type Column struct {
	Name   string
	Alts   []string
	Need   Need
	Effect string // what goes wrong without it
}

// TableSpec is a table gbb reads.
type TableSpec struct {
	Name    string
	Tracker bool   // only with issues imported (not with --no-issues)
	Need    Need   // when the whole table is missing
	Effect  string // what goes wrong without it
	Columns []Column
	// Match, when set, selects tables by name instead (GitLab 18+ splits
	// uploads into project_uploads, user_uploads...).
	Match func(t *pgdump.Table) bool
}

func cols(names ...string) []Column {
	out := make([]Column, len(names))
	for i, n := range names {
		out[i] = Column{Name: n, Need: Nice}
	}
	return out
}

func need(name, effect string, alts ...string) Column {
	return Column{Name: name, Alts: alts, Need: Needed, Effect: effect}
}

func nice(name, effect string, alts ...string) Column {
	return Column{Name: name, Alts: alts, Need: Nice, Effect: effect}
}

// Schema lists every table and column the import uses.
var Schema = []TableSpec{
	{Name: "namespaces", Need: Needed, Effect: "groups and users unknown: projects listed by hash",
		Columns: append([]Column{need("id", ""), need("name", "groups shown without names"), need("path", "group paths unknown"),
			nice("description", "group descriptions read from namespace_details (GitLab 19)")},
			cols("parent_id", "type", "owner_id")...)},
	{Name: "namespace_details", Need: Nice, Effect: "group descriptions not shown, unless namespaces.description has them (GitLab < 19)",
		Columns: []Column{need("namespace_id", ""), need("description", "group descriptions not shown")}},
	{Name: "routes", Need: Needed, Effect: "full paths unknown: projects listed by hash",
		Columns: []Column{need("source_id", ""), need("source_type", ""), need("path", "")}},
	{Name: "projects", Need: Needed, Effect: "projects unknown: repositories listed by hash",
		Columns: append([]Column{need("id", ""), need("name", "projects shown without names"), need("path", ""),
			need("namespace_id", "projects not placed in their groups")},
			cols("description", "archived", "visibility_level", "pending_delete", "created_at", "last_activity_at")...)},
	{Name: "project_repositories", Need: Nice, Effect: "storage paths guessed from project ids and full paths (GitLab < 12.x)",
		Columns: []Column{need("project_id", ""), need("disk_path", "")}},
	{Name: "users", Need: Needed, Effect: "authors shown as numbers, bots not told apart",
		Columns: []Column{need("id", ""), nice("username", "authors shown as numbers"), nice("name", ""),
			nice("user_type", "bots and service accounts shown as people", "bot_type")}},
	{Name: "topics", Need: Nice, Effect: "project topics not shown (GitLab < 14 used tags/taggings)",
		Columns: []Column{need("id", ""), need("name", ""), nice("title", "topic titles shown as their names")}},
	{Name: "project_topics", Need: Nice, Effect: "project topics not shown",
		Columns: []Column{need("project_id", ""), need("topic_id", "")}},
	{Name: "lfs_objects_projects", Need: Nice, Effect: "--lfs=head extracts every LFS object",
		Columns: []Column{need("project_id", "")}},
	{Name: "lfs_objects", Need: Nice, Effect: "verify cannot check lfs.tar.gz",
		Columns: []Column{need("oid", ""), need("size", ""), nice("file_store", "objects in object storage reported as missing")}},
	{Name: "*_uploads", Need: Nice, Effect: "verify cannot check uploads.tar.gz", Match: isUploadTable,
		Columns: []Column{need("path", ""), need("uploader", ""), need("store", ""), nice("model_type", "owner guessed from the table name"),
			need("model_id", ""), nice("size", "extraction size estimated from the archive")}},
	{Name: "snippets", Need: Nice, Effect: "snippets not listed",
		Columns: append([]Column{need("id", ""), need("title", "")},
			cols("description", "file_name", "author_id", "project_id", "visibility_level", "created_at", "updated_at")...)},
	{Name: "snippet_repositories", Need: Nice, Effect: "snippet repositories found by hashed id only",
		Columns: []Column{need("snippet_id", ""), need("disk_path", "")}},

	{Name: "issues", Tracker: true, Need: Needed, Effect: "no issues",
		Columns: append([]Column{need("id", ""), need("iid", ""), need("project_id", ""), need("title", ""),
			need("state_id", "issue states unknown", "state")},
			cols("description", "author_id", "created_at", "updated_at", "closed_at", "confidential", "milestone_id")...)},
	{Name: "merge_requests", Tracker: true, Need: Needed, Effect: "no merge requests",
		Columns: append([]Column{need("id", ""), need("iid", ""), need("target_project_id", ""), need("title", ""),
			need("state_id", "merge request states unknown", "state"),
			need("source_branch", ""), need("target_branch", ""),
			nice("draft", "drafts recognised from their title (Draft:, WIP:)")},
			cols("description", "author_id", "source_project_id", "created_at", "updated_at", "merge_commit_sha", "milestone_id")...)},
	{Name: "merge_request_diffs", Tracker: true, Need: Nice, Effect: "merge request changes shown from the MR ref only",
		Columns: []Column{need("id", ""), need("merge_request_id", ""), need("base_commit_sha", ""), need("head_commit_sha", "")}},
	{Name: "notes", Tracker: true, Need: Needed, Effect: "no comments",
		Columns: append([]Column{need("id", ""), need("noteable_type", ""), need("noteable_id", ""), need("note", ""),
			need("project_id", ""), nice("internal", "internal comments not marked", "confidential")},
			cols("commit_id", "author_id", "created_at", "system")...)},
	{Name: "labels", Tracker: true, Need: Nice, Effect: "no labels",
		Columns: append([]Column{need("id", ""), need("title", "")}, cols("color", "description", "project_id", "group_id")...)},
	{Name: "label_links", Tracker: true, Need: Nice, Effect: "labels not attached to issues and merge requests",
		Columns: []Column{need("label_id", ""), need("target_type", ""), need("target_id", "")}},
	{Name: "milestones", Tracker: true, Need: Nice, Effect: "no milestones",
		Columns: append([]Column{need("id", ""), need("title", "")},
			cols("iid", "description", "state", "start_date", "due_date", "project_id", "group_id")...)},
	{Name: "releases", Tracker: true, Need: Nice, Effect: "no releases",
		Columns: append([]Column{need("id", ""), need("tag", ""), need("project_id", "")},
			cols("name", "description", "author_id", "created_at", "released_at", "sha")...)},
	{Name: "release_links", Tracker: true, Need: Nice, Effect: "releases without asset links",
		Columns: []Column{need("release_id", ""), need("url", ""), nice("name", ""), nice("filepath", ""), nice("link_type", "")}},
}

// specFor returns the spec of a dump table, or nil if gbb does not read it.
func specFor(t *pgdump.Table) *TableSpec {
	for i := range Schema {
		s := &Schema[i]
		if s.Match != nil && s.Match(t) || s.Match == nil && s.Name == t.Name {
			return s
		}
	}
	return nil
}

// row reads the columns of a spec'd table by their current names: the
// column positions are resolved once per COPY block, alternative names
// included.
type row struct {
	pos  map[string]int    // spec name -> position in the row
	used map[string]string // spec name -> column actually found (an alt, or itself)
	r    pgdump.Row
}

func resolve(t *pgdump.Table, s *TableSpec) *row {
	rw := &row{pos: map[string]int{}, used: map[string]string{}}
	for _, c := range s.Columns {
		for _, n := range append([]string{c.Name}, c.Alts...) {
			if i := t.Col(n); i >= 0 {
				rw.pos[c.Name], rw.used[c.Name] = i, n
				break
			}
		}
	}
	return rw
}

// get returns a column's value; ok is false when NULL or absent.
func (rw *row) get(name string) (string, bool) {
	i, ok := rw.pos[name]
	if !ok || i >= len(rw.r) || rw.r[i] == nil {
		return "", false
	}
	return *rw.r[i], true
}

func (rw *row) str(name string) string { v, _ := rw.get(name); return v }

func (rw *row) num(name string) int64 { v, _ := rw.int(name); return v }

func (rw *row) int(name string) (int64, bool) {
	s, ok := rw.get(name)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func (rw *row) bool(name string) bool {
	s, ok := rw.get(name)
	return ok && (s == "t" || s == "true")
}

// has reports whether the dump has the column (under any of its names).
func (rw *row) has(name string) bool { _, ok := rw.pos[name]; return ok }

// via returns the column name the value is read from (an older name
// tells which encoding the value uses).
func (rw *row) via(name string) string { return rw.used[name] }

// SchemaNote is a table or column of Schema the dump does not have.
type SchemaNote struct {
	Table    string `json:"table"`
	Column   string `json:"column,omitempty"` // "" when the whole table is missing
	Severity string `json:"severity"`         // warning (data wrong or missing) or info
	Effect   string `json:"effect"`
	Using    string `json:"using,omitempty"` // older column name used instead
}

func (n SchemaNote) String() string {
	what := n.Table
	if n.Column != "" {
		what += "." + n.Column
	}
	if n.Using != "" {
		return what + ": read from " + n.Using + " (older GitLab)"
	}
	if n.Effect == "" {
		return what + " missing"
	}
	return what + " missing: " + n.Effect
}

// schemaNotes compares what the dump had with Schema. seen holds the
// spec names of the tables found, with their resolved columns.
func schemaNotes(seen map[string]*row, tracker bool) []SchemaNote {
	var out []SchemaNote
	sev := func(n Need) string {
		if n == Needed {
			return "warning"
		}
		return "info"
	}
	for _, s := range Schema {
		if s.Tracker && !tracker {
			continue
		}
		rw := seen[s.Name]
		if rw == nil {
			out = append(out, SchemaNote{Table: s.Name, Severity: sev(s.Need), Effect: s.Effect})
			continue
		}
		for _, c := range s.Columns {
			switch used := rw.used[c.Name]; {
			case used == "":
				out = append(out, SchemaNote{Table: s.Name, Column: c.Name, Severity: sev(c.Need), Effect: c.Effect})
			case used != c.Name:
				out = append(out, SchemaNote{Table: s.Name, Column: c.Name, Severity: "info", Effect: c.Effect, Using: used})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == "warning"
		}
		return false
	})
	return out
}

// draftTitle recognises drafts by title, for GitLab versions without the
// draft column.
func draftTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, p := range []string{"draft:", "[draft]", "(draft)", "wip:", "[wip]"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}
