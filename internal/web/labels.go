package web

import (
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/index"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// labelColors returns a label's background and a readable text colour.
// Colours come from the database, so anything but #rgb/#rrggbb is ignored.
func labelColors(c string) (bg, fg string) {
	if !hexColor.MatchString(c) {
		return "#6f6a5f", "#ffffff"
	}
	h := c[1:]
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	v, _ := strconv.ParseUint(h, 16, 32)
	r, g, b := float64(v>>16&0xff), float64(v>>8&0xff), float64(v&0xff)
	// Perceived brightness (ITU-R BT.601), as GitLab does.
	if 0.299*r+0.587*g+0.114*b > 150 {
		return "#" + strings.ToLower(h), "#1f1d18"
	}
	return "#" + strings.ToLower(h), "#ffffff"
}

// labelsCSS serves one class per label: the page CSP forbids inline
// styles, and this keeps every label page cacheable.
func (s *Server) labelsCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	ts := s.mgr().Tracker()
	if ts == nil {
		return
	}
	ids := make([]int64, 0, len(ts.Labels))
	for id := range ts.Labels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var b strings.Builder
	for _, id := range ids {
		bg, fg := labelColors(ts.Labels[id].Color)
		fmt.Fprintf(&b, ".lb-%d{--lb:%s;--lbt:%s}\n", id, bg, fg)
	}
	w.Write([]byte(b.String()))
}

// scope splits a scoped label "priority::high" into key and value.
func scope(title string) []string {
	if i := strings.LastIndex(title, "::"); i > 0 && i+2 < len(title) {
		return []string{title[:i], title[i+2:]}
	}
	return nil
}

// inScope reports whether a label or milestone (owned by a project or a
// group) applies to project p: its own, or one of its groups and their
// parents.
func inScope(ix *index.Index, p *index.Project) func(projectID, groupID int64) bool {
	groups := map[int64]bool{}
	for id := p.NamespaceID; id != 0 && !groups[id]; {
		groups[id] = true
		ns := ix.NamespaceByID(id)
		if ns == nil {
			break
		}
		id = ns.ParentID
	}
	return func(projectID, groupID int64) bool {
		return projectID == p.ID || groupID != 0 && groups[groupID]
	}
}

type labelRow struct {
	*tracker.Label
	Group bool
	Use   *tracker.Usage
}

type labelsData struct {
	T      *target
	Labels []labelRow
}

func (s *Server) labelsPage(w http.ResponseWriter, r *http.Request, t *target) {
	ts := s.mgr().Tracker()
	if ts == nil || t.P.ID == 0 {
		s.noTracker(w, r, t)
		return
	}
	use, err := ts.LabelUsage(t.P.ID)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	applies := inScope(s.mgr().Index(), t.P)
	d := labelsData{T: t}
	for _, l := range ts.Labels {
		if applies(l.ProjectID, l.GroupID) {
			u := use[l.ID]
			if u == nil {
				u = &tracker.Usage{}
			}
			d.Labels = append(d.Labels, labelRow{Label: l, Group: l.GroupID != 0, Use: u})
		}
	}
	sort.Slice(d.Labels, func(i, j int) bool {
		return strings.ToLower(d.Labels[i].Title) < strings.ToLower(d.Labels[j].Title)
	})
	cs := append(s.crumbsFor(t), crumb{"Labels", ""})
	s.render(w, r, http.StatusOK, "labels.html", "Labels · "+t.Name, cs, d)
}

type milestoneRow struct {
	*tracker.Milestone
	Group   bool
	Use     *tracker.Usage
	Percent int
}

type milestonesData struct {
	T          *target
	State      string
	Milestones []milestoneRow
	Open       int
	Closed     int
}

func (s *Server) milestonesPage(w http.ResponseWriter, r *http.Request, t *target) {
	ts := s.mgr().Tracker()
	if ts == nil || t.P.ID == 0 {
		s.noTracker(w, r, t)
		return
	}
	use, err := ts.MilestoneUsage(t.P.ID)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	d := milestonesData{T: t, State: r.URL.Query().Get("state")}
	if d.State != "closed" && d.State != "all" {
		d.State = "active"
	}
	applies := inScope(s.mgr().Index(), t.P)
	for _, m := range ts.Milestones {
		if !applies(m.ProjectID, m.GroupID) {
			continue
		}
		if m.State == "closed" {
			d.Closed++
		} else {
			d.Open++
		}
		if d.State != "all" && (m.State == "closed") != (d.State == "closed") {
			continue
		}
		u := use[m.ID]
		if u == nil {
			u = &tracker.Usage{}
		}
		row := milestoneRow{Milestone: m, Group: m.GroupID != 0, Use: u}
		if u.Issues > 0 {
			row.Percent = u.ClosedIssues * 100 / u.Issues
		}
		d.Milestones = append(d.Milestones, row)
	}
	// Soonest due first; milestones without a due date last.
	sort.Slice(d.Milestones, func(i, j int) bool {
		a, b := d.Milestones[i], d.Milestones[j]
		if (a.Due == "") != (b.Due == "") {
			return b.Due == ""
		}
		if a.Due != b.Due {
			if d.State == "closed" {
				return a.Due > b.Due
			}
			return a.Due < b.Due
		}
		return a.Title < b.Title
	})
	cs := append(s.crumbsFor(t), crumb{"Milestones", ""})
	s.render(w, r, http.StatusOK, "milestones.html", "Milestones · "+t.Name, cs, d)
}

// uploadLink matches GitLab attachment URLs ("…/uploads/<secret>/<file>").
var uploadLink = regexp.MustCompile(`/uploads/([0-9a-f]{32})/([^/?#]+)$`)

type releaseLinkView struct {
	tracker.ReleaseLink
	Href  string
	Local bool // served from the backup's attachments
}

type releaseView struct {
	*tracker.Release
	Title      string
	AuthorName string
	Desc       template.HTML
	Links      []releaseLinkView
}

type releasesData struct {
	T        *target
	Releases []releaseView
}

func (s *Server) releasesPage(w http.ResponseWriter, r *http.Request, t *target) {
	ts := s.mgr().Tracker()
	if ts == nil || t.P.ID == 0 {
		s.noTracker(w, r, t)
		return
	}
	rels, err := ts.Releases(t.P.ID)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	d := releasesData{T: t}
	for _, rel := range rels {
		v := releaseView{Release: rel, Title: rel.Name, AuthorName: ts.UserName(rel.Author)}
		if v.Title == "" {
			v.Title = rel.Tag
		}
		// Notes link relative to the release's tag, like on GitLab.
		v.Desc = renderMarkdown([]byte(rel.Description), "/"+t.Base+"/-/blob/"+rel.Tag, "/"+t.Base+"/-/raw/"+rel.Tag, "")
		for _, l := range rel.Links {
			lv := releaseLinkView{ReleaseLink: l, Href: l.URL}
			if m := uploadLink.FindStringSubmatch(l.URL); m != nil {
				if _, ok := s.mgr().Upload(t.P.DiskPath, t.P.FullPath, m[1], m[2]); ok {
					lv.Href, lv.Local = "/"+t.Base+"/-/uploads/"+m[1]+"/"+m[2], true
				}
			}
			v.Links = append(v.Links, lv)
		}
		d.Releases = append(d.Releases, v)
	}
	cs := append(s.crumbsFor(t), crumb{"Releases", ""})
	s.render(w, r, http.StatusOK, "releases.html", "Releases · "+t.Name, cs, d)
}
