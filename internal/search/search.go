// Package search greps the default branch of repositories in a backup.
//
// It is built to keep memory flat on small devices: repositories are
// searched one at a time, files one at a time, large files and binaries are
// skipped from their pack header without being inflated, results are
// streamed to the caller as they are found, and only one search runs at a
// time.
package search

import (
	"bytes"
	"context"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

// Defaults.
const (
	MaxResults  = 500     // matching lines per search
	MaxPerFile  = 5       // matching lines shown per file
	MaxFileSize = 1 << 20 // larger files are skipped
	MaxLineLen  = 300     // longer lines are cut around the match
	MaxPattern  = 200
)

// skipExt are extensions never worth grepping.
var skipExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".ico": true, ".bmp": true,
	".pdf": true, ".zip": true, ".gz": true, ".tgz": true, ".xz": true, ".bz2": true, ".7z": true, ".rar": true,
	".jar": true, ".war": true, ".class": true, ".so": true, ".dll": true, ".exe": true, ".a": true, ".o": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp3": true, ".mp4": true, ".mov": true, ".avi": true, ".wav": true, ".ogg": true, ".flac": true,
	".psd": true, ".blend": true, ".fbx": true, ".glb": true, ".gltf": true, ".obj": true, ".uasset": true, ".umap": true,
	".min.js": true, ".map": true, ".lock": false,
}

// Query describes a search.
type Query struct {
	Pattern       string
	Regex         bool
	CaseSensitive bool
	PathFilter    string // substring the file path must contain
}

// Match is one matching line.
type Match struct {
	Line  int    `json:"line"`
	Text  string `json:"text"`
	Start int    `json:"start"` // byte offsets of the first match in Text
	End   int    `json:"end"`
}

// FileHit is a file with matching lines.
type FileHit struct {
	Path    string  `json:"path"`
	Matches []Match `json:"matches"`
	More    int     `json:"more,omitempty"` // matching lines not shown
}

// Stats summarise a search.
type Stats struct {
	Repos, Files, Skipped int
	Bytes                 int64
	Lines                 int
	Truncated             bool
	Elapsed               time.Duration
}

// Matcher finds the pattern in a line.
type Matcher struct {
	re    *regexp.Regexp
	lit   []byte
	lower bool
}

// Compile validates and prepares a query.
func Compile(q Query) (*Matcher, error) {
	p := q.Pattern
	if strings.TrimSpace(p) == "" {
		return nil, errors.New("empty search")
	}
	if len(p) > MaxPattern {
		return nil, errors.New("search too long")
	}
	if q.Regex {
		if !q.CaseSensitive {
			p = "(?i)" + p
		}
		re, err := regexp.Compile(p) // RE2: linear time, safe on any input
		if err != nil {
			return nil, err
		}
		return &Matcher{re: re}, nil
	}
	if !q.CaseSensitive {
		return &Matcher{lit: bytes.ToLower([]byte(p)), lower: true}, nil
	}
	return &Matcher{lit: []byte(p)}, nil
}

// anyMatch is a fast whole-file pre-check.
func (m *Matcher) anyMatch(data []byte) bool {
	if m.re != nil {
		return m.re.Match(data)
	}
	if m.lower {
		return indexFold(data, m.lit) >= 0
	}
	return bytes.Contains(data, m.lit)
}

// find returns the first match in line, or -1.
func (m *Matcher) find(line []byte) (int, int) {
	if m.re != nil {
		if loc := m.re.FindIndex(line); loc != nil {
			return loc[0], loc[1]
		}
		return -1, -1
	}
	var i int
	if m.lower {
		i = indexFold(line, m.lit)
	} else {
		i = bytes.Index(line, m.lit)
	}
	if i < 0 {
		return -1, -1
	}
	return i, i + len(m.lit)
}

// indexFold is an ASCII case-insensitive bytes.Index that does not
// allocate (lowerSub must already be lower case).
func indexFold(s, lowerSub []byte) int {
	n := len(lowerSub)
	if n == 0 {
		return 0
	}
	first := lowerSub[0]
	for i := 0; i+n <= len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != first {
			continue
		}
		j := 1
		for ; j < n; j++ {
			d := s[i+j]
			if 'A' <= d && d <= 'Z' {
				d += 'a' - 'A'
			}
			if d != lowerSub[j] {
				break
			}
		}
		if j == n {
			return i
		}
	}
	return -1
}

func skipFile(p string) bool {
	lp := strings.ToLower(p)
	if strings.HasSuffix(lp, ".min.js") || strings.HasSuffix(lp, ".min.css") {
		return true
	}
	return skipExt[path.Ext(lp)]
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

func isLFSPointer(b []byte) bool {
	return len(b) < 1024 && bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1\n"))
}

// ErrLimit is returned when MaxResults was reached.
var ErrLimit = errors.New("search: result limit reached")

// Repo searches the tree of commit-ish root in repo, calling emit for each
// file with matches. budget is the number of matching lines still allowed;
// it is decremented. Only one file's content is in memory at a time.
func Repo(ctx context.Context, repo *gitstore.Repo, root gitstore.Hash, m *Matcher, q Query, budget *int, st *Stats, emit func(FileHit) error) error {
	c, err := repo.Commit(root)
	if err != nil {
		return err
	}
	type dir struct {
		hash   gitstore.Hash
		prefix string
	}
	stack := []dir{{c.Tree, ""}}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := repo.Tree(d.hash)
		if err != nil {
			continue
		}
		gitstore.SortTreeForDisplay(entries)
		// Push directories in reverse so they pop in display order.
		for i := len(entries) - 1; i >= 0; i-- {
			if e := entries[i]; e.IsDir() {
				stack = append(stack, dir{e.Hash, d.prefix + e.Name + "/"})
			}
		}
		for _, e := range entries {
			if e.IsDir() || e.IsSubmodule() || e.IsSymlink() {
				continue
			}
			p := d.prefix + e.Name
			if q.PathFilter != "" && !strings.Contains(strings.ToLower(p), strings.ToLower(q.PathFilter)) {
				continue
			}
			if skipFile(p) {
				st.Skipped++
				continue
			}
			if _, size, err := repo.Stat(e.Hash); err != nil || size > MaxFileSize {
				st.Skipped++
				continue
			}
			_, data, err := repo.Object(e.Hash)
			if err != nil || isBinary(data) || isLFSPointer(data) {
				st.Skipped++
				continue
			}
			st.Files++
			st.Bytes += int64(len(data))
			if !m.anyMatch(data) {
				continue
			}
			hit := FileHit{Path: p}
			lineNo := 0
			for len(data) > 0 {
				lineNo++
				line := data
				if i := bytes.IndexByte(data, '\n'); i >= 0 {
					line, data = data[:i], data[i+1:]
				} else {
					data = nil
				}
				s, e := m.find(line)
				if s < 0 {
					continue
				}
				if len(hit.Matches) >= MaxPerFile || *budget <= 0 {
					hit.More++
					continue
				}
				hit.Matches = append(hit.Matches, clip(lineNo, line, s, e))
				*budget--
				st.Lines++
			}
			if len(hit.Matches) > 0 {
				if err := emit(hit); err != nil {
					return err
				}
			}
			if *budget <= 0 {
				return ErrLimit
			}
		}
	}
	return nil
}

// clip keeps at most MaxLineLen bytes of a line around its match.
func clip(n int, line []byte, s, e int) Match {
	line = bytes.TrimRight(line, "\r")
	if e > len(line) {
		e = len(line)
	}
	if len(line) <= MaxLineLen {
		return Match{Line: n, Text: string(line), Start: s, End: e}
	}
	from := s - 80
	if from < 0 {
		from = 0
	}
	for from > 0 && !utf8.RuneStart(line[from]) {
		from--
	}
	to := from + MaxLineLen
	if to > len(line) {
		to = len(line)
	}
	for to < len(line) && !utf8.RuneStart(line[to]) {
		to--
	}
	if e > to {
		e = to
	}
	return Match{Line: n, Text: string(line[from:to]), Start: s - from, End: e - from}
}
