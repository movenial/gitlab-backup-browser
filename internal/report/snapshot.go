package report

import (
	"strings"
	"time"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
	"github.com/movenial/gitlab-backup-browser/internal/tracker"
)

// SnapshotVersion is bumped when the snapshot format changes.
const SnapshotVersion = 1

// Snapshot is what a comparison needs to know about a backup: projects,
// bundle presence, issue/MR counts and branch/tag tips.
type Snapshot struct {
	Version  int           `json:"version"`
	TakenAt  time.Time     `json:"taken_at"`
	Summary  Summary       `json:"summary"`
	Projects []SnapProject `json:"projects"`
}

// SnapProject is one project (or unmatched bundle) in a snapshot.
type SnapProject struct {
	ID        int64             `json:"id,omitempty"`
	Path      string            `json:"path"`
	Unmatched bool              `json:"unmatched,omitempty"`
	Repo      string            `json:"repo,omitempty"` // bundle key, "" when none
	Issues    int               `json:"issues,omitempty"`
	MRs       int               `json:"mrs,omitempty"`
	Refs      map[string]string `json:"refs,omitempty"` // branches and tags only
	// Source branches of merged or closed merge requests of this project.
	Merged map[string]bool `json:"merged_sources,omitempty"`
}

func (p *SnapProject) mergedSource(ref string) bool {
	b, ok := strings.CutPrefix(ref, "refs/heads/")
	return ok && p.Merged[b]
}

// TakeSnapshot reads branch and tag tips from bundle headers (no objects
// are unpacked).
func TakeSnapshot(l *Loaded, sum Summary) *Snapshot {
	s := &Snapshot{Version: SnapshotVersion, TakenAt: time.Now().UTC(), Summary: sum}
	for _, p := range l.Index.Projects {
		sp := SnapProject{ID: p.ID, Path: p.FullPath, Unmatched: p.Unmatched, Repo: p.Repo, Issues: p.Issues, MRs: p.MRs}
		if rb := l.Index.Repos[p.Repo]; rb != nil && p.Repo != "" {
			if refs, err := backup.Refs(l.Src, rb); err == nil {
				sp.Refs = map[string]string{}
				for n, h := range refs {
					if strings.HasPrefix(n, "refs/heads/") || strings.HasPrefix(n, "refs/tags/") {
						sp.Refs[n] = h
					}
				}
			}
		}
		if l.Tracker != nil && p.MRs > 0 && p.ID != 0 {
			for _, state := range []string{"merged", "closed"} {
				mrs, _, err := l.Tracker.MergeRequests(p.ID, tracker.Query{State: state, Limit: 1 << 30})
				if err != nil {
					continue
				}
				for _, mr := range mrs {
					if mr.SourceProject == 0 || mr.SourceProject == p.ID {
						if sp.Merged == nil {
							sp.Merged = map[string]bool{}
						}
						sp.Merged[mr.Source] = true
					}
				}
			}
		}
		s.Projects = append(s.Projects, sp)
	}
	return s
}
