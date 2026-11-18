package web

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

// FileChange is one changed path between two trees.
type FileChange struct {
	Status   string // A, M, D, T
	Path     string
	Old, New gitstore.Hash
	OldMode  uint32
	NewMode  uint32
}

// diffTrees lists changed files between trees a and b (a may be zero for
// root commits). It stops after limit changes.
func diffTrees(r *gitstore.Repo, a, b gitstore.Hash, prefix string, out *[]FileChange, limit int) error {
	if len(*out) >= limit || a == b {
		return nil
	}
	var ae, be []gitstore.TreeEntry
	var err error
	if !a.IsZero() {
		if ae, err = r.Tree(a); err != nil {
			return err
		}
	}
	if !b.IsZero() {
		if be, err = r.Tree(b); err != nil {
			return err
		}
	}
	am := map[string]gitstore.TreeEntry{}
	for _, e := range ae {
		am[e.Name] = e
	}
	seen := map[string]bool{}
	emit := func(fc FileChange) bool {
		*out = append(*out, fc)
		return len(*out) < limit
	}
	for _, e := range be {
		seen[e.Name] = true
		p := prefix + e.Name
		old, ok := am[e.Name]
		switch {
		case !ok && e.IsDir():
			if err := diffTrees(r, gitstore.ZeroHash, e.Hash, p+"/", out, limit); err != nil {
				return err
			}
		case !ok:
			if !emit(FileChange{Status: "A", Path: p, New: e.Hash, NewMode: e.Mode}) {
				return nil
			}
		case old.IsDir() && e.IsDir():
			if err := diffTrees(r, old.Hash, e.Hash, p+"/", out, limit); err != nil {
				return err
			}
		case old.IsDir() != e.IsDir():
			if old.IsDir() {
				if err := diffTrees(r, old.Hash, gitstore.ZeroHash, p+"/", out, limit); err != nil {
					return err
				}
				emit(FileChange{Status: "A", Path: p, New: e.Hash, NewMode: e.Mode})
			} else {
				emit(FileChange{Status: "D", Path: p, Old: old.Hash, OldMode: old.Mode})
				if err := diffTrees(r, gitstore.ZeroHash, e.Hash, p+"/", out, limit); err != nil {
					return err
				}
			}
		case old.Hash != e.Hash || old.Mode != e.Mode:
			st := "M"
			if old.Hash == e.Hash {
				st = "T"
			}
			if !emit(FileChange{Status: st, Path: p, Old: old.Hash, New: e.Hash, OldMode: old.Mode, NewMode: e.Mode}) {
				return nil
			}
		}
		if len(*out) >= limit {
			return nil
		}
	}
	for _, e := range ae {
		if seen[e.Name] {
			continue
		}
		p := prefix + e.Name
		if e.IsDir() {
			if err := diffTrees(r, e.Hash, gitstore.ZeroHash, p+"/", out, limit); err != nil {
				return err
			}
		} else if !emit(FileChange{Status: "D", Path: p, Old: e.Hash, OldMode: e.Mode}) {
			return nil
		}
		if len(*out) >= limit {
			return nil
		}
	}
	return nil
}

// DiffLine is one line of a rendered unified diff.
type DiffLine struct {
	Kind     byte // ' ', '+', '-', '@'
	Text     string
	HTML     template.HTML // highlighted Text, when the language is known
	Old, New int           // line numbers (0 = none)
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// maxEdits bounds diff work (and memory) so huge rewrites stay cheap.
const maxEdits = 400

// myers returns the edit script between a and b as a sequence of ops:
// ' ' keep, '-' delete from a, '+' insert from b. ok is false when the
// files differ by more than maxEdits lines.
func myers(a, b []string) (ops []byte, ok bool) {
	// Trim common prefix and suffix first: most diffs are small.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	mid, ok := myersCore(a[pre:len(a)-suf], b[pre:len(b)-suf])
	if !ok {
		return nil, false
	}
	ops = append(bytes.Repeat([]byte{' '}, pre), mid...)
	return append(ops, bytes.Repeat([]byte{' '}, suf)...), true
}

func myersCore(a, b []string) ([]byte, bool) {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil, true
	}
	if n == 0 {
		return bytes.Repeat([]byte{'+'}, m), true
	}
	if m == 0 {
		return bytes.Repeat([]byte{'-'}, n), true
	}
	off := max
	v := make([]int, 2*max+2)
	var trace [][]int
	for d := 0; d <= max; d++ {
		if d > maxEdits {
			return nil, false
		}
		vc := make([]int, len(v))
		copy(vc, v)
		trace = append(trace, vc)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, off, d), true
			}
		}
	}
	return nil, false
}

func backtrack(trace [][]int, a, b []string, off, d int) []byte {
	x, y := len(a), len(b)
	var ops []byte
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var pk int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := v[off+pk]
		py := px - pk
		for x > px && y > py {
			ops = append(ops, ' ')
			x--
			y--
		}
		if x == px {
			ops = append(ops, '+')
			y--
		} else {
			ops = append(ops, '-')
			x--
		}
	}
	for x > 0 && y > 0 {
		ops = append(ops, ' ')
		x--
		y--
	}
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}

// unified renders a unified diff with the given context size; ok is false
// when the change is too large to diff.
func unified(a, b []string, ctx int) ([]DiffLine, bool) {
	ops, ok := myers(a, b)
	if !ok {
		return nil, false
	}
	type item struct {
		op       byte
		text     string
		old, new int
	}
	var items []item
	i, j := 0, 0
	for _, op := range ops {
		switch op {
		case ' ':
			items = append(items, item{' ', a[i], i + 1, j + 1})
			i++
			j++
		case '-':
			items = append(items, item{'-', a[i], i + 1, 0})
			i++
		case '+':
			items = append(items, item{'+', b[j], 0, j + 1})
			j++
		}
	}
	keep := make([]bool, len(items))
	for idx, it := range items {
		if it.op != ' ' {
			for c := idx - ctx; c <= idx+ctx; c++ {
				if c >= 0 && c < len(items) {
					keep[c] = true
				}
			}
		}
	}
	var out []DiffLine
	prev := -2
	for idx, it := range items {
		if !keep[idx] {
			continue
		}
		if (prev >= 0 && idx != prev+1) || (prev < 0 && idx > 0) {
			out = append(out, DiffLine{Kind: '@', Text: "…"})
		}
		out = append(out, DiffLine{Kind: it.op, Text: it.text, Old: it.old, New: it.new})
		prev = idx
	}
	return out, true
}
