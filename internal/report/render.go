package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/human"
)

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes a human summary.
func (r *Report) WriteText(w io.Writer) {
	b := r.Backup
	fmt.Fprintf(w, "Backup:     %s\n", b.Path)
	if b.GitLabVersion != "" || b.CreatedAt != nil {
		created := "unknown date"
		if b.CreatedAt != nil {
			created = fmt.Sprintf("%s, %s ago", b.CreatedAt.Format("2006-01-02 15:04 UTC"), age(b.AgeSeconds))
		}
		fmt.Fprintf(w, "GitLab:     %s (%s)\n", orDash(b.GitLabVersion), created)
	}
	fmt.Fprintf(w, "Database:   %s\n", b.Database)
	fmt.Fprintf(w, "Projects:   %d (%d with a repository, %d without, %d wikis) in %d groups and users\n",
		b.Projects, b.WithRepo, b.WithoutRepo, b.Wikis, b.Namespaces)
	fmt.Fprintf(w, "Bundles:    %d repositories, %s\n", b.Bundles, human.Bytes(b.BundleBytes))
	if b.IssuesKnown {
		fmt.Fprintf(w, "Tracker:    %d issues, %d merge requests\n", b.Issues, b.MergeRequests)
	}
	if n := len(r.Schema); n > 0 {
		fmt.Fprintf(w, "Schema:     %d difference(s) with what gbb expects (see the problems, or --json)\n", n)
	}
	for _, a := range r.Assets {
		label := map[bool]string{true: "LFS:", false: "Uploads:"}[a.Archive == backup.LFSMember]
		switch {
		case a.Expected == 0 && a.Remote == 0:
			fmt.Fprintf(w, "%-11s none in the database\n", label)
		case !a.Present:
			fmt.Fprintf(w, "%-11s %s absent (%d files expected)\n", label, a.Archive, a.Expected)
		default:
			fmt.Fprintf(w, "%-11s %d of %d files in %s", label, a.Found, a.Expected, a.Archive)
			if a.Remote > 0 {
				fmt.Fprintf(w, ", %d in object storage (never in backups)", a.Remote)
			}
			fmt.Fprintln(w)
		}
	}

	if c := r.Comparison; c != nil {
		p := c.Previous
		when := filepath.Base(p.Path)
		if p.CreatedAt != nil {
			when = p.CreatedAt.Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(w, "\nCompared with the backup of %s:\n", when)
		fmt.Fprintf(w, "  projects   %+d (%d added, %d removed, %d renamed)\n", b.Projects-p.Projects, len(c.ProjectsAdded), len(c.ProjectsRemoved), len(c.ProjectsRenamed))
		if b.IssuesKnown && p.IssuesKnown {
			fmt.Fprintf(w, "  issues     %+d, merge requests %+d\n", c.IssuesDelta, c.MRsDelta)
		}
		counts := map[string]int{}
		for _, rc := range c.Refs {
			counts[rc.Change]++
		}
		var parts []string
		for _, k := range []string{"added", "fast-forward", "moved", "deleted-merged", "deleted", "rewound", "diverged", "rewritten"} {
			if counts[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
			}
		}
		if len(parts) == 0 {
			parts = []string{"no change"}
		}
		fmt.Fprintf(w, "  refs       %s\n", strings.Join(parts, ", "))
		ignored := 0
		for _, rc := range c.Refs {
			if rc.Ignored {
				ignored++
			}
		}
		if ignored > 0 {
			fmt.Fprintf(w, "             %d change(s) not reported, matched by --ignore-refs\n", ignored)
		}
		if !c.RefsAnalysed && counts["moved"] > 0 {
			fmt.Fprintf(w, "             (use --deep to tell normal pushes from rewound or rewritten branches)\n")
		}
		printList(w, "  added: ", c.ProjectsAdded)
		for _, rn := range c.ProjectsRenamed {
			fmt.Fprintf(w, "  renamed: %s -> %s\n", rn.From, rn.To)
		}
	}

	if len(r.Heaviest) > 0 {
		if r.Jobs > 1 {
			fmt.Fprintf(w, "\nSlowest repositories to unbundle (%d in parallel; run with --jobs 1 to measure memory per repository):\n", r.Jobs)
			for _, h := range r.Heaviest {
				fmt.Fprintf(w, "  %-50s %8d objects  %.1fs\n", h.Project, h.Objects, h.Seconds)
			}
		} else {
			fmt.Fprintf(w, "\nHeaviest repositories to unbundle (peak heap; also the first-visit cost in serve):\n")
			for _, h := range r.Heaviest {
				fmt.Fprintf(w, "  %-50s %9s  %8d objects  %.1fs\n", h.Project, human.Bytes(int64(h.PeakHeap)), h.Objects, h.Seconds)
			}
		}
	}
	if len(r.NoBundle) > 0 {
		fmt.Fprintf(w, "\n%d project(s) have no repository bundle (normal for empty projects).\n", len(r.NoBundle))
	}
	if b.Unmatched > 0 && b.Database == "ok" {
		fmt.Fprintf(w, "%d bundle(s) belong to no project in the database.\n", b.Unmatched)
	}

	if len(r.Problems) > 0 {
		fmt.Fprintf(w, "\nProblems:\n")
		shown := 0
		for _, sev := range []Severity{Error, Warning} {
			for _, p := range r.Problems {
				if p.Severity != sev {
					continue
				}
				if shown == 50 {
					fmt.Fprintf(w, "  … and %d more (see --json)\n", len(r.Problems)-50)
					goto done
				}
				label := "WARN "
				if sev == Error {
					label = "ERROR"
				}
				if p.Project != "" {
					fmt.Fprintf(w, "  %s %s: %s\n", label, p.Project, p.Message)
				} else {
					fmt.Fprintf(w, "  %s %s\n", label, p.Message)
				}
				shown++
			}
		}
	done:
	}

	fmt.Fprintln(w)
	switch r.Status {
	case StatusOK:
		if r.Deep {
			fmt.Fprintf(w, "OK: %d repositories restored and checked in %.0fs.\n", r.ReposChecked, r.Seconds)
		} else {
			fmt.Fprintf(w, "OK: all %d bundles are readable. Use --deep to fully check their contents.\n", r.ReposChecked)
		}
	case StatusWarning:
		fmt.Fprintf(w, "WARNING: %d warning(s), no errors.\n", r.WarningCount)
	default:
		fmt.Fprintf(w, "ERROR: %d error(s), %d warning(s).\n", r.ErrorCount, r.WarningCount)
	}
}

func printList(w io.Writer, prefix string, xs []string) {
	for i, x := range xs {
		if i == 10 {
			fmt.Fprintf(w, "%s… and %d more\n", prefix, len(xs)-10)
			return
		}
		fmt.Fprintf(w, "%s%s\n", prefix, x)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(sec float64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.0fd", d.Hours()/24)
}

// --- Prometheus ---

type promWriter struct {
	w    io.Writer
	seen map[string]bool
}

func (p *promWriter) metric(name, help string, value float64, labels ...string) {
	if !p.seen[name] {
		fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		p.seen[name] = true
	}
	lbl := ""
	if len(labels) > 0 {
		var parts []string
		for i := 0; i+1 < len(labels); i += 2 {
			v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(labels[i+1])
			parts = append(parts, fmt.Sprintf(`%s="%s"`, labels[i], v))
		}
		lbl = "{" + strings.Join(parts, ",") + "}"
	}
	fmt.Fprintf(p.w, "%s%s %g\n", name, lbl, value)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// WritePrometheus writes metrics in the text exposition format, for the
// node_exporter textfile collector.
func (r *Report) WritePrometheus(w io.Writer) {
	p := &promWriter{w: w, seen: map[string]bool{}}
	b := r.Backup
	status := map[string]float64{StatusOK: 0, StatusWarning: 1, StatusError: 2}[r.Status]
	p.metric("gbb_verify_status", "Result of the last check: 0 ok, 1 warnings, 2 errors.", status)
	p.metric("gbb_verify_errors", "Errors found by the last check.", float64(r.ErrorCount))
	p.metric("gbb_verify_warnings", "Warnings found by the last check.", float64(r.WarningCount))
	p.metric("gbb_verify_deep", "1 when every repository was fully restored.", b2f(r.Deep))
	p.metric("gbb_verify_timestamp_seconds", "When the last check ran.", float64(r.CheckedAt.Unix()))
	p.metric("gbb_verify_duration_seconds", "How long the last check took, import included.", r.Seconds)
	p.metric("gbb_verify_import_seconds", "Part of it spent indexing the backup (near 0 when cached).", r.ImportSeconds)
	p.metric("gbb_backup_info", "Backup being checked.", 1, "gitlab_version", b.GitLabVersion, "file", filepath.Base(b.Path))
	if b.CreatedAt != nil {
		p.metric("gbb_backup_created_timestamp_seconds", "When GitLab created the backup (alert on time() minus this).", float64(b.CreatedAt.Unix()))
	}
	p.metric("gbb_backup_size_bytes", "Size of the backup archive.", float64(b.SizeBytes))
	p.metric("gbb_backup_database_ok", "1 when the database dump was read.", b2f(b.Database == "ok"))
	p.metric("gbb_backup_projects", "Projects in the backup.", float64(b.WithRepo), "state", "with_repository")
	p.metric("gbb_backup_projects", "Projects in the backup.", float64(b.WithoutRepo), "state", "without_repository")
	p.metric("gbb_backup_unmatched_bundles", "Bundles no project points to.", float64(b.Unmatched))
	p.metric("gbb_backup_repositories", "Repository bundles (code, wikis, designs).", float64(b.Bundles))
	p.metric("gbb_backup_repository_bytes", "Total size of repository bundles.", float64(b.BundleBytes))
	p.metric("gbb_backup_wikis", "Wikis in the backup.", float64(b.Wikis))
	p.metric("gbb_backup_namespaces", "Groups and user namespaces.", float64(b.Namespaces))
	if b.IssuesKnown {
		p.metric("gbb_backup_issues", "Issues in the backup.", float64(b.Issues))
		p.metric("gbb_backup_merge_requests", "Merge requests in the backup.", float64(b.MergeRequests))
	}
	for _, a := range r.Assets {
		name := strings.TrimSuffix(a.Archive, ".tar.gz")
		const help = "Files the database lists, by archive and state (with --deep)."
		p.metric("gbb_backup_assets", help, float64(a.Found), "archive", name, "state", "found")
		p.metric("gbb_backup_assets", help, float64(a.Missing), "archive", name, "state", "missing")
		p.metric("gbb_backup_assets", help, float64(a.Remote), "archive", name, "state", "object_storage")
	}
	kinds := map[string]int{}
	for _, pr := range r.Problems {
		kinds[string(pr.Severity)+"\x00"+pr.Kind]++
	}
	keys := make([]string, 0, len(kinds))
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sev, kind, _ := strings.Cut(k, "\x00")
		p.metric("gbb_verify_problems", "Problems by kind.", float64(kinds[k]), "severity", sev, "kind", kind)
	}
	if c := r.Comparison; c != nil {
		p.metric("gbb_compare_available", "1 when the backup was compared with a previous one.", 1)
		p.metric("gbb_compare_projects_added", "Projects added since the previous backup.", float64(len(c.ProjectsAdded)))
		p.metric("gbb_compare_projects_removed", "Projects missing since the previous backup.", float64(len(c.ProjectsRemoved)))
		p.metric("gbb_compare_projects_renamed", "Projects whose path changed.", float64(len(c.ProjectsRenamed)))
		p.metric("gbb_compare_repositories_lost", "Projects that lost their repository bundle.", float64(len(c.ReposLost)))
		// Refs matched by --ignore-refs are counted apart, so alerts on
		// gbb_compare_refs follow the same rule as the report's warnings.
		counts, ignored := map[string]int{}, map[string]int{}
		for _, rc := range c.Refs {
			if rc.Ignored {
				ignored[rc.Change]++
			} else {
				counts[rc.Change]++
			}
		}
		for _, k := range []string{"added", "deleted", "deleted-merged", "moved", "fast-forward", "rewound", "diverged", "rewritten"} {
			p.metric("gbb_compare_refs", "Branch and tag changes since the previous backup (refs matched by --ignore-refs excluded).", float64(counts[k]), "change", k)
		}
		for _, k := range []string{"added", "deleted", "deleted-merged", "moved", "fast-forward", "rewound", "diverged", "rewritten"} {
			p.metric("gbb_compare_refs_ignored", "Branch and tag changes matched by --ignore-refs.", float64(ignored[k]), "change", k)
		}
		if b.IssuesKnown && c.Previous.IssuesKnown {
			p.metric("gbb_compare_issues_delta", "Change in the number of issues.", float64(c.IssuesDelta))
			p.metric("gbb_compare_merge_requests_delta", "Change in the number of merge requests.", float64(c.MRsDelta))
		}
	} else {
		p.metric("gbb_compare_available", "1 when the backup was compared with a previous one.", 0)
	}
}

// WritePrometheusFile writes the metrics atomically (the textfile
// collector must never read a half-written file).
func (r *Report) WritePrometheusFile(path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	r.WritePrometheus(f)
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
