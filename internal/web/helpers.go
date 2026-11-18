package web

// Small helpers shared by the pages.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/app"
)

// --- helpers ---

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 2*365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	}
	return plural(int(d.Hours()/24/365), "year")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

type pathPart struct{ Name, Path string }

func pathParts(p string) []pathPart {
	var out []pathPart
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		out = append(out, pathPart{parts[i], strings.Join(parts[:i+1], "/")})
	}
	return out
}

func initial(s string) string {
	for _, r := range s {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// shortDir shortens a directory for display: the home directory becomes
// "~" and long paths keep only their last two elements.
func shortDir(d string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		if d == home {
			return "~"
		}
		if strings.HasPrefix(d, home+string(filepath.Separator)) {
			d = "~" + d[len(home):]
		}
	}
	parts := strings.Split(filepath.ToSlash(d), "/")
	if len(parts) > 4 {
		return "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	return d
}

// loadStep is one line of the "opening the backup" checklist.
type loadStep struct {
	Name, Hint string
	State      string // done, now, todo
	Done       int64
	Total      int64
	Percent    int
}

// loadSteps turns the import phase into a checklist. The phases come from
// app.Manager and index.Build.
func loadSteps(st app.Status, backupName string) []loadStep {
	steps := []loadStep{
		{Name: "Extract the compressed archive", Hint: "only for .tar.gz backups, done once"},
		{Name: "Find the repositories", Hint: "reads the archive's table of contents"},
		{Name: "Read the database", Hint: "projects, groups, users, issues and merge requests"},
	}
	cur := 1
	switch {
	case strings.HasPrefix(st.Phase, "extracting"):
		cur = 0
	case strings.HasPrefix(st.Phase, "reading database"):
		cur = 2
	case st.Phase == "ready" || strings.HasPrefix(st.Phase, "unbundling"):
		cur = 3
	}
	if cur != 0 && !strings.HasSuffix(backupName, ".gz") {
		steps = steps[1:]
		cur--
	}
	for i := range steps {
		switch {
		case i < cur:
			steps[i].State = "done"
		case i == cur:
			steps[i].State = "now"
			steps[i].Done, steps[i].Total, steps[i].Percent = st.Done, st.Total, st.Percent()
		default:
			steps[i].State = "todo"
		}
	}
	return steps
}

// themeOf returns the theme chosen in the menu ("" = follow the system).
func themeOf(r *http.Request) string {
	if c, err := r.Cookie("gbb_theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return ""
}

// setTheme cycles auto → light → dark (a cookie, so pages are rendered with
// the right colours from the start, without script) and goes back.
func setTheme(w http.ResponseWriter, r *http.Request) {
	next := map[string]string{"": "light", "light": "dark", "dark": ""}[themeOf(r)]
	c := &http.Cookie{Name: "gbb_theme", Value: next, Path: "/", MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode, HttpOnly: true}
	if next == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
	back := r.URL.Query().Get("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, "/\\") {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
