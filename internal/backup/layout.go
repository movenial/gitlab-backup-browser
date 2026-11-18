package backup

import (
	"bufio"
	"io"
	"sort"
	"strconv"
	"strings"
)

// RepoBackup is everything the archive holds for one repository.
//
// Two on-disk layouts are recognised under repositories/:
//
//	classic (GitLab 13-15):  [<storage>/]@hashed/ab/cd/<hash>[.wiki|.design].bundle
//	gitaly-backup (15+):     [<storage>/]@hashed/ab/cd/<hash>[.wiki|.design].git/<backup_id>/NNN.bundle (+ NNN.refs)
//
// Legacy non-hashed paths (group/project.bundle) are handled the same way.
type RepoBackup struct {
	Key     string `json:"key"`     // disk path without suffix, e.g. @hashed/ab/cd/<hash>.wiki
	Storage string `json:"storage"` // e.g. "default"; empty for the classic layout
	Steps   []Step `json:"steps"`   // bundles in apply order
	Size    int64  `json:"size"`    // total bytes of all bundles
}

// Step is one bundle of a repository's chain.
type Step struct {
	BackupID string `json:"backup_id,omitempty"`
	Seq      int    `json:"seq,omitempty"`
	Bundle   string `json:"bundle"`         // member name of the .bundle
	Refs     string `json:"refs,omitempty"` // member name of the matching .refs, if any
	Size     int64  `json:"size"`
}

// Kind returns "wiki", "design" or "project" from the key suffix.
func (r *RepoBackup) Kind() string {
	switch {
	case strings.HasSuffix(r.Key, ".wiki"):
		return "wiki"
	case strings.HasSuffix(r.Key, ".design"):
		return "design"
	}
	return "project"
}

// skippedPrefixes are repository families this tool does not browse.
var skippedPrefixes = []string{"@pools/", "@groups/", "manifests/"}

// Layout groups the repositories/ members of a backup into per-repo
// bundle chains.
func Layout(entries []Entry) map[string]*RepoBackup {
	type stepKey struct {
		key, id string
		seq     int
	}
	repos := map[string]*RepoBackup{}
	refsFiles := map[stepKey]string{}

	for _, e := range entries {
		rel, ok := strings.CutPrefix(e.Name, "repositories/")
		if !ok {
			continue
		}
		parts := strings.Split(rel, "/")
		storage := ""
		if len(parts) > 2 && !strings.HasPrefix(parts[0], "@") &&
			(strings.HasPrefix(parts[1], "@") || parts[0] == "default") {
			storage, parts = parts[0], parts[1:]
		}
		joined := strings.Join(parts, "/") + "/"
		skip := false
		for _, p := range skippedPrefixes {
			if strings.HasPrefix(joined, p) {
				skip = true
			}
		}
		if skip {
			continue
		}

		// gitaly-backup layout: .../<name>.git/<backup_id>/<NNN>.(bundle|refs)
		gitIdx := -1
		for i := 0; i < len(parts)-1; i++ {
			if strings.HasSuffix(parts[i], ".git") {
				gitIdx = i
				break
			}
		}
		if gitIdx >= 0 {
			key := strings.TrimSuffix(strings.Join(parts[:gitIdx+1], "/"), ".git")
			rest := parts[gitIdx+1:]
			if len(rest) != 2 {
				continue // LATEST pointers, custom hooks, ...
			}
			id, file := rest[0], rest[1]
			base, ext, ok := cutExt(file)
			if !ok {
				continue
			}
			seq, err := strconv.Atoi(base)
			if err != nil {
				continue
			}
			switch ext {
			case "bundle":
				rb := repos[key]
				if rb == nil {
					rb = &RepoBackup{Key: key, Storage: storage}
					repos[key] = rb
				}
				rb.Steps = append(rb.Steps, Step{BackupID: id, Seq: seq, Bundle: e.Name, Size: e.Size})
				rb.Size += e.Size
			case "refs":
				refsFiles[stepKey{key, id, seq}] = e.Name
			}
			continue
		}

		// classic layout: .../<name>.bundle
		last := parts[len(parts)-1]
		if !strings.HasSuffix(last, ".bundle") {
			continue
		}
		key := strings.TrimSuffix(strings.Join(parts, "/"), ".bundle")
		rb := repos[key]
		if rb == nil {
			rb = &RepoBackup{Key: key, Storage: storage}
			repos[key] = rb
		}
		rb.Steps = append(rb.Steps, Step{Bundle: e.Name, Size: e.Size})
		rb.Size += e.Size
	}

	for key, rb := range repos {
		sort.Slice(rb.Steps, func(i, j int) bool {
			a, b := rb.Steps[i], rb.Steps[j]
			if a.BackupID != b.BackupID {
				return a.BackupID < b.BackupID
			}
			return a.Seq < b.Seq
		})
		for i := range rb.Steps {
			s := &rb.Steps[i]
			s.Refs = refsFiles[stepKey{key, s.BackupID, s.Seq}]
		}
	}
	return repos
}

func cutExt(file string) (base, ext string, ok bool) {
	i := strings.LastIndexByte(file, '.')
	if i <= 0 {
		return "", "", false
	}
	return file[:i], file[i+1:], true
}

// BackupInfo parses backup_information.yml. Values are flat strings,
// except Ruby objects such as GitLab 19's
//
//	:backup_created_at: !ruby/object:ActiveSupport::TimeWithZone
//	  utc: 2026-11-03 02:00:30.123456789 Z
//	  zone: ...
//
// which are reduced to their "utc" (or "time") sub-key.
func BackupInfo(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	objKey := "" // top-level key whose value is a nested object
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, " ") {
			if objKey == "" {
				continue
			}
			k, v, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), ":"), ":")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v != "" && (k == "utc" || (k == "time" && out[objKey] == "")) {
				out[objKey] = cleanTime(v)
			}
			continue
		}
		objKey = ""
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "#") {
			continue
		}
		// Keys are Ruby symbols: ":gitlab_version: 17.0.1"
		k, v, ok := strings.Cut(strings.TrimPrefix(line, ":"), ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" {
			continue
		}
		if v == "" || strings.HasPrefix(v, "!") {
			objKey = k // the real value follows on indented lines
			continue
		}
		out[k] = v
	}
	return out
}

// cleanTime turns "2026-11-03 02:00:30.123456789 Z" into
// "2026-11-03 02:00:30 UTC".
func cleanTime(v string) string {
	if strings.HasSuffix(v, " Z") {
		v = strings.TrimSuffix(v, " Z")
		if i := strings.IndexByte(v, '.'); i > 0 {
			v = v[:i]
		}
		return v + " UTC"
	}
	return v
}
