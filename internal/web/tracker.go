package web

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

const trackerPageSize = 50

// confidentialHidden explains why a confidential issue is not shown.
const confidentialHidden = "This issue is confidential. gbb shows confidential issues and internal comments only when it is started with a login (--auth user:password)."

type trackerListData struct {
	T         *target
	Kind      string // issues | merge_requests
	State     string
	Q         string
	Label     string
	Milestone string
	Issues    []*tracker.Issue
	MRs       []*tracker.MergeRequest
	Total     int
	Page      int
	More      bool
	Users     *tracker.Store
	States    []string
	// HiddenConfidential counts the project's confidential issues left
	// out because no login protects the server.
	HiddenConfidential int
}

func (s *Server) noTracker(w http.ResponseWriter, r *http.Request, t *target) {
	s.errorPage(w, r, http.StatusNotFound, "Issues and merge requests were not imported for this backup (the database could not be read, or gbb runs with --no-issues).")
}

func (s *Server) trackerList(w http.ResponseWriter, r *http.Request, t *target, kind string) {
	ts := s.mgr().Tracker()
	if ts == nil || t.P.ID == 0 {
		s.noTracker(w, r, t)
		return
	}
	q := r.URL.Query()
	pg, _ := strconv.Atoi(q.Get("page"))
	if pg < 1 {
		pg = 1
	}
	d := trackerListData{T: t, Kind: kind, State: q.Get("state"), Q: strings.TrimSpace(q.Get("q")), Page: pg, Users: ts,
		Label: q.Get("label"), Milestone: q.Get("milestone")}
	if d.State == "" {
		d.State = "opened"
	}
	query := tracker.Query{State: d.State, Search: d.Q, Label: d.Label, Milestone: d.Milestone,
		Offset: (pg - 1) * trackerPageSize, Limit: trackerPageSize, HideConfidential: !s.ShowConfidential}
	var err error
	title := "Issues"
	if kind == "issues" {
		d.States = []string{"opened", "closed", "all"}
		d.Issues, d.Total, err = ts.Issues(t.P.ID, query)
		if !s.ShowConfidential {
			d.HiddenConfidential = t.P.ConfidentialIssues
		}
	} else {
		title = "Merge requests"
		d.States = []string{"opened", "merged", "closed", "all"}
		d.MRs, d.Total, err = ts.MergeRequests(t.P.ID, query)
	}
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	d.More = d.Total > pg*trackerPageSize
	cs := append(s.crumbsFor(t), crumb{title, "/" + t.Base + "/-/" + kind})
	s.render(w, r, http.StatusOK, "tracker_list.html", title+" · "+t.Name, cs, d)
}

type noteView struct {
	*tracker.Note
	AuthorName string
	HTML       template.HTML
}

type trackerItemData struct {
	T          *target
	Kind       string
	Issue      *tracker.Issue
	MR         *tracker.MergeRequest
	Title      string
	State      string
	AuthorName string
	Created    string
	Desc       template.HTML
	Labels     []*tracker.Label
	Milestone  *tracker.Milestone
	Notes      []noteView
	MoreNotes  bool
	// Merge request changes
	Files     []fileDiff
	Truncated bool
	DiffNote  string
	Building  bool
	HeadRef   string
	BaseShort string
	HeadShort string
}

const maxNotes = 500

func (s *Server) trackerItem(w http.ResponseWriter, r *http.Request, t *target, kind, rest string) {
	ts := s.mgr().Tracker()
	if ts == nil || t.P.ID == 0 {
		s.noTracker(w, r, t)
		return
	}
	iid, err := strconv.ParseInt(strings.Trim(rest, "/"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "Bad number.")
		return
	}
	d := trackerItemData{T: t, Kind: kind}
	// Markdown links in descriptions resolve against the default branch.
	blobBase, rawBase := "/"+t.Base+"/-/blob/HEAD", "/"+t.Base+"/-/raw/HEAD"
	var noteType string
	var target int64
	title := "Issues"
	if kind == "issues" {
		is, err := ts.Issue(t.P.ID, iid)
		if err != nil {
			s.errorPage(w, r, http.StatusNotFound, "No such issue in this backup.")
			return
		}
		if is.Confidential && !s.ShowConfidential {
			s.errorPage(w, r, http.StatusNotFound, confidentialHidden)
			return
		}
		d.Issue, d.Title, d.State, d.Created = is, is.Title, is.State, is.Created
		d.AuthorName = ts.UserName(is.Author)
		d.Desc = renderMarkdown([]byte(is.Description), blobBase, rawBase, "")
		d.Labels, d.Milestone = ts.SortLabels(is.Labels), ts.MilestoneOf(is.Milestone)
		noteType, target = "Issue", is.ID
	} else {
		title = "Merge requests"
		mr, err := ts.MergeRequest(t.P.ID, iid)
		if err != nil {
			s.errorPage(w, r, http.StatusNotFound, "No such merge request in this backup.")
			return
		}
		d.MR, d.Title, d.State, d.Created = mr, mr.Title, mr.State, mr.Created
		d.AuthorName = ts.UserName(mr.Author)
		d.Desc = renderMarkdown([]byte(mr.Description), blobBase, rawBase, "")
		d.Labels, d.Milestone = ts.SortLabels(mr.Labels), ts.MilestoneOf(mr.Milestone)
		noteType, target = "MergeRequest", mr.ID
		s.mrChanges(&d, t, mr)
	}
	notes, more, err := ts.Notes(t.P.ID, noteType, target, maxNotes, s.ShowConfidential)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	for _, n := range notes {
		nv := noteView{Note: n, AuthorName: ts.UserName(n.Author)}
		if !n.System {
			nv.HTML = renderMarkdown([]byte(n.Body), blobBase, rawBase, "")
		}
		d.Notes = append(d.Notes, nv)
	}
	d.MoreNotes = more
	cs := append(s.crumbsFor(t), crumb{title, "/" + t.Base + "/-/" + kind})
	s.render(w, r, http.StatusOK, "tracker_item.html", d.Title, cs, d)
}

// mrChanges fills the "Changes" part of a merge request: the diff between
// its recorded base and head commits, which GitLab keeps reachable via
// refs/merge-requests/<iid>/head and keep-around refs.
func (s *Server) mrChanges(d *trackerItemData, t *target, mr *tracker.MergeRequest) {
	if t.Key == "" {
		d.DiffNote = "The repository is not in the backup."
		return
	}
	if !s.mgr().Built(t.Key) {
		s.mgr().BuildAsync(t.Key)
		d.Building = true
		return
	}
	repo, release, err := s.mgr().Repo(t.Key)
	if err != nil {
		d.DiffNote = err.Error()
		return
	}
	defer release()
	head, herr := gitstore.ParseHash(mr.HeadSHA)
	base, berr := gitstore.ParseHash(mr.BaseSHA)
	if herr != nil || !repo.Has(head) {
		// Fall back to the MR ref when the diff record is missing.
		if h, ref, err := repo.Resolve("refs/merge-requests/" + strconv.FormatInt(mr.IID, 10) + "/head"); err == nil && ref != "" {
			head, herr = h, nil
		}
	}
	if herr != nil || !repo.Has(head) {
		d.DiffNote = "The commits of this merge request are not in the backup."
		return
	}
	hc, err := repo.Commit(head)
	if err != nil {
		d.DiffNote = err.Error()
		return
	}
	var oldTree gitstore.Hash
	switch {
	case berr == nil && repo.Has(base):
		bc, err := repo.Commit(base)
		if err != nil {
			d.DiffNote = err.Error()
			return
		}
		oldTree = bc.Tree
		d.BaseShort = base.Short()
	case len(hc.Parents) > 0:
		// No recorded base: show the last commit's changes only.
		if pc, err := repo.Commit(hc.Parents[0]); err == nil {
			oldTree = pc.Tree
		}
		d.DiffNote = "Base commit unknown: showing only the last commit of the merge request."
	}
	d.HeadShort = head.String()
	d.Files, d.Truncated, err = buildDiffs(repo, oldTree, hc.Tree)
	if err != nil {
		d.DiffNote = err.Error()
	}
}

// Link returns this list with another state or page, keeping the filters.
func (d trackerListData) Link(state string, page int) string {
	v := url.Values{}
	for k, val := range map[string]string{"state": state, "q": d.Q, "label": d.Label, "milestone": d.Milestone} {
		if val != "" {
			v.Set(k, val)
		}
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	base := "/" + d.T.Base + "/-/" + d.Kind
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// Filter returns this list with one filter changed ("" removes it).
func (d trackerListData) Filter(key, val string) string {
	switch key {
	case "label":
		d.Label = val
	case "milestone":
		d.Milestone = val
	}
	return d.Link(d.State, 1)
}

// shortTime trims "2024-09-01 10:00:00.123456" to minutes.
func shortTime(s string) string {
	if len(s) >= 16 {
		return s[:16]
	}
	return s
}
