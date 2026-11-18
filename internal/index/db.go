package index

import (
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/pgdump"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

type route struct {
	sourceID   int64
	sourceType string
	path       string
}

type handler struct {
	tw             *tracker.Writer // nil when issues are not imported
	namespaces     []*Namespace
	projects       []*Project
	routes         []route
	diskPaths      map[int64]string
	botUsers       map[int64]bool    // users whose user_type is not human
	topics         map[int64]string  // topic id -> name
	projTopics     map[int64][]int64 // project id -> topic ids
	lfsProjects    map[int64]bool
	sawLFS         bool // lfs_objects_projects was in the dump
	exp            Expected
	uploadRows     []uploadRow
	uploadSeen     map[string]bool
	nsDescriptions map[int64]string // namespace_details (GitLab 19)
	snippets       []*Snippet
	snippetDisk    map[int64]string
	rows           map[string]*row // per dump table: its spec'd columns, resolved
	specs          map[string]*TableSpec
	seen           map[string]*row // per spec: the first table found (for schema notes)
}

// expected resolves attachment paths (they need project storage paths,
// known once projects are read) and returns what the dump lists.
func (h *handler) expected(ix *Index) *Expected {
	e := h.exp
	if e.LFS == nil {
		e.LFS = map[string]int64{}
	}
	// A project's attachments live under its storage path: the hashed
	// path, or its full path on legacy storage (no project_repositories).
	dirs := map[int64]string{}
	for _, p := range ix.Projects {
		switch {
		case p.ID == 0:
		case p.DiskPath != "":
			dirs[p.ID] = p.DiskPath
		case p.Repo != "":
			dirs[p.ID] = p.Repo
		default:
			dirs[p.ID] = p.FullPath
		}
	}
	seen := map[string]bool{} // a row may appear in both the old and the split tables
	for _, u := range h.uploadRows {
		switch m := uploadMember(u, dirs); {
		case m == "":
			e.UploadsOther++
		case !seen[m]:
			seen[m] = true
			e.Uploads = append(e.Uploads, m)
		}
	}
	return &e
}

func newHandler() *handler {
	return &handler{diskPaths: map[int64]string{}, botUsers: map[int64]bool{},
		topics: map[int64]string{}, projTopics: map[int64][]int64{}, lfsProjects: map[int64]bool{},
		snippetDisk: map[int64]string{}, uploadSeen: map[string]bool{}, nsDescriptions: map[int64]string{}, rows: map[string]*row{}, specs: map[string]*TableSpec{}, seen: map[string]*row{}}
}

// Want decodes the tables of Schema (tracker ones only when issues are
// imported); everything else (CI, events, secrets, credentials...) is
// skipped without being parsed.
func (h *handler) Want(t *pgdump.Table) bool {
	if t.Schema != "" && t.Schema != "public" || len(t.Columns) == 0 {
		return false
	}
	spec := specFor(t)
	if spec == nil || spec.Tracker && h.tw == nil {
		return false
	}
	rw := resolve(t, spec)
	h.rows[t.Name], h.specs[t.Name] = rw, spec
	// Tables matched by pattern (the split uploads tables) count together:
	// a column is there if one of them has it.
	if prev := h.seen[spec.Name]; prev == nil {
		h.seen[spec.Name] = &row{pos: map[string]int{}, used: map[string]string{}}
	}
	for k, v := range rw.used {
		if h.seen[spec.Name].used[k] == "" {
			h.seen[spec.Name].used[k] = v
		}
	}
	return true
}

// isUploadTable: GitLab 18+ splits uploads into one table per kind
// (project_uploads, namespace_uploads, user_uploads, snippet_uploads...);
// older versions have a single uploads table. Any table with an
// attachment's columns is read.
func isUploadTable(t *pgdump.Table) bool {
	return (t.Name == "uploads" || strings.HasSuffix(t.Name, "_uploads")) &&
		t.Col("path") >= 0 && t.Col("uploader") >= 0 && t.Col("store") >= 0
}

// uploadModel guesses the owner type of rows of a per-kind table without a
// model_type column (project_uploads -> Project).
func uploadModel(table string) string {
	switch strings.TrimSuffix(table, "_uploads") {
	case "project":
		return "Project"
	case "namespace":
		return "Namespace"
	case "user":
		return "User"
	case "snippet":
		return "Snippet"
	}
	return ""
}

func (h *handler) Row(t *pgdump.Table, r pgdump.Row) error {
	c := h.rows[t.Name]
	c.r = r
	str, num := c.str, c.num
	switch h.specs[t.Name].Name {
	case "*_uploads":
		h.exp.Known = true
		model := str("model_type")
		if model == "" {
			model = uploadModel(t.Name)
		}
		// During GitLab's migration a row can be in uploads and in a split
		// table: count it once.
		key := model + "\x00" + str("model_id") + "\x00" + str("path")
		if h.uploadSeen[key] {
			return nil
		}
		h.uploadSeen[key] = true
		if st, ok := c.int("store"); !local(st, ok) {
			h.exp.UploadsRemote++
			return nil
		}
		h.uploadRows = append(h.uploadRows, uploadRow{path: str("path"), uploader: str("uploader"),
			modelType: model, modelID: num("model_id")})
		h.exp.UploadsBytes += num("size")
	case "namespaces":
		h.namespaces = append(h.namespaces, &Namespace{
			ID: num("id"), ParentID: num("parent_id"), Name: str("name"), Path: str("path"),
			Type: str("type"), Description: str("description"), OwnerID: num("owner_id"),
		})
	case "namespace_details":
		if d := str("description"); d != "" {
			h.nsDescriptions[num("namespace_id")] = d
		}
	case "routes":
		h.routes = append(h.routes, route{sourceID: num("source_id"), sourceType: str("source_type"), path: str("path")})
	case "projects":
		h.projects = append(h.projects, &Project{
			ID: num("id"), Name: str("name"), Path: str("path"), NamespaceID: num("namespace_id"),
			Description: str("description"), Archived: c.bool("archived"),
			Visibility: int(num("visibility_level")), PendingDel: c.bool("pending_delete"),
			CreatedAt: str("created_at"), LastActivity: str("last_activity_at"),
		})
	case "project_repositories":
		if id, ok := c.int("project_id"); ok {
			h.diskPaths[id] = str("disk_path")
		}
	case "issues":
		h.tw.AddIssue(num("project_id"), &tracker.Issue{
			ID: num("id"), IID: num("iid"), Title: str("title"), Description: str("description"),
			State: issueState(c), Author: num("author_id"),
			Created: str("created_at"), Updated: str("updated_at"), Closed: str("closed_at"),
			Confidential: c.bool("confidential"), Milestone: num("milestone_id"),
		})
	case "merge_requests":
		h.tw.AddMR(num("target_project_id"), &tracker.MergeRequest{
			ID: num("id"), IID: num("iid"), Title: str("title"), Description: str("description"),
			State: mrState(c), Author: num("author_id"),
			Source: str("source_branch"), Target: str("target_branch"), SourceProject: num("source_project_id"),
			Created: str("created_at"), Updated: str("updated_at"), Draft: c.bool("draft") || !c.has("draft") && draftTitle(str("title")),
			MergeSHA: str("merge_commit_sha"), Milestone: num("milestone_id"),
		})
	case "merge_request_diffs":
		h.tw.AddMRDiff(num("id"), num("merge_request_id"), str("base_commit_sha"), str("head_commit_sha"))
	case "notes":
		pid, ok := c.int("project_id")
		if !ok {
			return nil // personal snippets etc.
		}
		h.tw.AddNote(pid, &tracker.Note{
			ID: num("id"), Type: str("noteable_type"), Target: num("noteable_id"), CommitID: str("commit_id"),
			Author: num("author_id"), Body: str("note"), Created: str("created_at"),
			System: c.bool("system"), Internal: c.bool("internal"),
		})
	case "labels":
		h.tw.AddLabel(tracker.Label{ID: num("id"), Title: str("title"), Color: str("color"),
			Description: str("description"), ProjectID: num("project_id"), GroupID: num("group_id")})
	case "label_links":
		h.tw.AddLabelLink(num("label_id"), str("target_type"), num("target_id"))
	case "milestones":
		state := str("state")
		if state == "" {
			state = "active"
		}
		h.tw.AddMilestone(tracker.Milestone{ID: num("id"), IID: num("iid"), Title: str("title"),
			Description: str("description"), State: state, Start: str("start_date"), Due: str("due_date"),
			ProjectID: num("project_id"), GroupID: num("group_id")})
	case "releases":
		h.tw.AddRelease(num("project_id"), &tracker.Release{ID: num("id"), Tag: str("tag"), Name: str("name"),
			Description: str("description"), Author: num("author_id"), Created: str("created_at"),
			Released: str("released_at"), SHA: str("sha")})
	case "release_links":
		h.tw.AddReleaseLink(num("release_id"), tracker.ReleaseLink{Name: str("name"), URL: str("url"),
			Filepath: str("filepath"), Type: str("link_type")})
	case "users":
		// user_type: NULL/0 is a human; bots, service accounts, ghost and
		// placeholder users have other values.
		if ut, ok := c.int("user_type"); ok && ut != 0 {
			h.botUsers[num("id")] = true
		}
		if h.tw != nil {
			h.tw.AddUser(num("id"), tracker.User{Username: str("username"), Name: str("name")})
		}
	case "topics":
		name := str("title")
		if name == "" {
			name = str("name")
		}
		h.topics[num("id")] = name
	case "snippets":
		h.snippets = append(h.snippets, &Snippet{ID: num("id"), Title: str("title"), Description: str("description"),
			FileName: str("file_name"), Author: num("author_id"), ProjectID: num("project_id"),
			Visibility: int(num("visibility_level")), Created: str("created_at"), Updated: str("updated_at")})
	case "snippet_repositories":
		h.snippetDisk[num("snippet_id")] = str("disk_path")
	case "lfs_objects":
		h.exp.Known = true
		if h.exp.LFS == nil {
			h.exp.LFS = map[string]int64{}
		}
		if fs, ok := c.int("file_store"); !local(fs, ok) {
			h.exp.LFSRemote++
		} else if oid := str("oid"); len(oid) == 64 {
			h.exp.LFS[oid] = num("size")
		}
	case "lfs_objects_projects":
		h.sawLFS = true
		h.lfsProjects[num("project_id")] = true
	case "project_topics":
		pid := num("project_id")
		h.projTopics[pid] = append(h.projTopics[pid], num("topic_id"))
	}
	return nil
}

// issueState reads state_id, or the text state column of GitLab < 12.
func issueState(c *row) string {
	if c.via("state_id") == "state" {
		if c.str("state_id") == "closed" {
			return "closed"
		}
		return "opened"
	}
	return tracker.IssueState(c.num("state_id"))
}

// mrState reads state_id, or the text state column of GitLab < 12.
func mrState(c *row) string {
	if c.via("state_id") == "state" {
		switch v := c.str("state_id"); v {
		case "closed", "merged", "locked":
			return v
		}
		return "opened"
	}
	return tracker.MRState(c.num("state_id"))
}
