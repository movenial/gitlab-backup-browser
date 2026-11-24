package web

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/index"
)

// projectFilter is the state of the /-/projects page, all from the URL so
// any view can be bookmarked or shared.
type projectFilter struct {
	Q        string
	Kind     string // "" = groups and people, or group|personal|bot|unmatched|all
	Archived string // "" = hide, include, only
	Vis      string // "", public, internal, private
	Repo     string // "", code, empty
	Topic    string
	Group    string // full path prefix
	Sort     string // activity (default), name, size, created, issues, mrs
	Page     int
}

func (f projectFilter) values() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("q", f.Q)
	set("kind", f.Kind)
	set("archived", f.Archived)
	set("vis", f.Vis)
	set("repo", f.Repo)
	set("topic", f.Topic)
	set("group", f.Group)
	set("sort", f.Sort)
	if f.Page > 1 {
		v.Set("page", strconv.Itoa(f.Page))
	}
	return v
}

// With returns the URL of this view with one parameter changed (and the
// page reset).
func (f projectFilter) With(key, val string) string {
	v := f.values()
	v.Del("page")
	if val == "" {
		v.Del(key)
	} else {
		v.Set(key, val)
	}
	if len(v) == 0 {
		return "/-/projects"
	}
	return "/-/projects?" + v.Encode()
}

// Active counts the dropdown filters that differ from the defaults.
func (f projectFilter) Active() int {
	n := 0
	for _, v := range []string{f.Vis, f.Repo, f.Sort} {
		if v != "" {
			n++
		}
	}
	return n
}

// PageURL returns the URL of another page of this view.
func (f projectFilter) PageURL(p int) string {
	f.Page = p
	if v := f.values(); len(v) > 0 {
		return "/-/projects?" + v.Encode()
	}
	return "/-/projects"
}

func (f projectFilter) match(p *index.Project) bool {
	switch f.Kind {
	case "":
		if p.Kind == "bot" || p.Kind == "unmatched" {
			return false
		}
	case "all":
	default:
		if p.Kind != f.Kind {
			return false
		}
	}
	switch f.Archived {
	case "":
		if p.Archived {
			return false
		}
	case "only":
		if !p.Archived {
			return false
		}
	}
	if f.Vis != "" && (p.Unmatched || p.VisibilityName() != f.Vis) {
		return false
	}
	switch f.Repo {
	case "code":
		if p.Repo == "" {
			return false
		}
	case "empty":
		if p.Repo != "" {
			return false
		}
	}
	if f.Topic != "" {
		found := false
		for _, t := range p.Topics {
			if strings.EqualFold(t, f.Topic) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Group != "" {
		g := strings.ToLower(strings.Trim(f.Group, "/")) + "/"
		if !strings.HasPrefix(strings.ToLower(p.FullPath), g) {
			return false
		}
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		hay := strings.ToLower(p.FullPath + " " + p.Name + " " + p.Description + " " + strings.Join(p.Topics, " "))
		for _, t := range strings.Fields(strings.ToLower(q)) {
			if !strings.Contains(hay, t) {
				return false
			}
		}
	}
	return true
}

type kindCount struct {
	Key, Label string
	Count      int
}

type projectsData struct {
	F        projectFilter
	Projects []*index.Project
	Total    int
	More     bool
	Kinds    []kindCount
	Archived int // archived projects matching the other filters
	Topics   []index.TopicCount
	Sorts    [][2]string
	GroupNS  *index.Namespace
}

const projectsPageSize = 50

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	ix := s.mgr().Index()
	if ix == nil {
		s.loading(w, r, http.StatusOK)
		return
	}
	q := r.URL.Query()
	f := projectFilter{Q: q.Get("q"), Kind: q.Get("kind"), Archived: q.Get("archived"), Vis: q.Get("vis"),
		Repo: q.Get("repo"), Topic: q.Get("topic"), Group: strings.Trim(q.Get("group"), "/"), Sort: q.Get("sort")}
	f.Page, _ = strconv.Atoi(q.Get("page"))
	if f.Page < 1 {
		f.Page = 1
	}
	d := projectsData{F: f, Topics: ix.Topics()}
	if len(d.Topics) > 20 {
		d.Topics = d.Topics[:20]
	}
	if f.Group != "" {
		d.GroupNS = ix.Namespace(f.Group)
	}

	// Counts per kind (and archived) under the other filters, for the chips.
	counts := map[string]int{}
	for _, p := range ix.Projects {
		g := f
		g.Kind = p.Kind
		if g.match(p) {
			counts[p.Kind]++
		}
		if p.Archived {
			g := f
			g.Archived = "only"
			if g.match(p) {
				d.Archived++
			}
		}
	}
	d.Kinds = []kindCount{
		{"", "Groups and people", counts["group"] + counts["personal"]},
		{"group", "In groups", counts["group"]},
		{"personal", "Personal", counts["personal"]},
		{"bot", "Bots and service accounts", counts["bot"]},
	}
	if counts["unmatched"] > 0 {
		d.Kinds = append(d.Kinds, kindCount{"unmatched", "Unmatched bundles", counts["unmatched"]})
	}
	d.Kinds = append(d.Kinds, kindCount{"all", "Everything", counts["group"] + counts["personal"] + counts["bot"] + counts["unmatched"]})
	d.Sorts = [][2]string{{"", "Last activity"}, {"name", "Name"}, {"size", "Size"}, {"created", "Newest"}, {"issues", "Open issues"}, {"mrs", "Open merge requests"}}

	var list []*index.Project
	for _, p := range ix.Projects {
		if f.match(p) {
			list = append(list, p)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch f.Sort {
		case "name":
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		case "size":
			return a.Size > b.Size
		case "created":
			return a.CreatedAt > b.CreatedAt
		case "issues":
			return s.openIssues(a) > s.openIssues(b)
		case "mrs":
			return a.OpenMRs > b.OpenMRs
		default:
			return a.LastActivity > b.LastActivity
		}
	})
	d.Total = len(list)
	lo := (f.Page - 1) * projectsPageSize
	if lo > len(list) {
		lo = len(list)
	}
	hi := lo + projectsPageSize
	if hi > len(list) {
		hi = len(list)
	}
	d.Projects, d.More = list[lo:hi], hi < len(list)
	s.render(w, r, http.StatusOK, "projects.html", "Projects", nil, d)
}
