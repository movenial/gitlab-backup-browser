package gitstore

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signature is an author, committer or tagger line.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

func parseSignature(s string) Signature {
	var sig Signature
	lt := strings.IndexByte(s, '<')
	gt := strings.LastIndexByte(s, '>')
	if lt < 0 || gt < lt {
		sig.Name = s
		return sig
	}
	sig.Name = strings.TrimSpace(s[:lt])
	sig.Email = s[lt+1 : gt]
	f := strings.Fields(s[gt+1:])
	if len(f) >= 1 {
		if sec, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			loc := time.UTC
			if len(f) >= 2 && len(f[1]) == 5 {
				if hh, err1 := strconv.Atoi(f[1][1:3]); err1 == nil {
					if mm, err2 := strconv.Atoi(f[1][3:5]); err2 == nil {
						off := hh*3600 + mm*60
						if f[1][0] == '-' {
							off = -off
						}
						loc = time.FixedZone(f[1], off)
					}
				}
			}
			sig.When = time.Unix(sec, 0).In(loc)
		}
	}
	return sig
}

// Commit is a parsed commit object.
type Commit struct {
	Hash      Hash
	Tree      Hash
	Parents   []Hash
	Author    Signature
	Committer Signature
	Message   string
}

// Subject returns the first line of the message.
func (c *Commit) Subject() string {
	s, _, _ := strings.Cut(c.Message, "\n")
	return s
}

// Body returns the message without its subject line.
func (c *Commit) Body() string {
	_, b, _ := strings.Cut(c.Message, "\n")
	return strings.TrimSpace(b)
}

// ParseCommit parses raw commit data.
func ParseCommit(h Hash, data []byte) (*Commit, error) {
	c := &Commit{Hash: h}
	hdr, msg, _ := bytes.Cut(data, []byte("\n\n"))
	c.Message = string(msg)
	for _, line := range strings.Split(string(hdr), "\n") {
		if strings.HasPrefix(line, " ") { // continuation (gpgsig, mergetag)
			continue
		}
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "tree":
			t, err := ParseHash(v)
			if err != nil {
				return nil, err
			}
			c.Tree = t
		case "parent":
			p, err := ParseHash(v)
			if err != nil {
				return nil, err
			}
			c.Parents = append(c.Parents, p)
		case "author":
			c.Author = parseSignature(v)
		case "committer":
			c.Committer = parseSignature(v)
		}
	}
	if c.Tree.IsZero() {
		return nil, fmt.Errorf("gitstore: commit %s has no tree", h)
	}
	return c, nil
}

// Tag is a parsed annotated tag.
type Tag struct {
	Hash    Hash
	Object  Hash
	Type    string
	Name    string
	Tagger  Signature
	Message string
}

// ParseTag parses raw tag data.
func ParseTag(h Hash, data []byte) (*Tag, error) {
	t := &Tag{Hash: h}
	hdr, msg, _ := bytes.Cut(data, []byte("\n\n"))
	t.Message = string(msg)
	for _, line := range strings.Split(string(hdr), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "object":
			o, err := ParseHash(v)
			if err != nil {
				return nil, err
			}
			t.Object = o
		case "type":
			t.Type = v
		case "tag":
			t.Name = v
		case "tagger":
			t.Tagger = parseSignature(v)
		}
	}
	return t, nil
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode uint32
	Name string
	Hash Hash
}

// IsDir reports whether the entry is a subtree.
func (e TreeEntry) IsDir() bool { return e.Mode == 0o40000 }

// IsSubmodule reports whether the entry is a gitlink.
func (e TreeEntry) IsSubmodule() bool { return e.Mode == 0o160000 }

// IsSymlink reports whether the entry is a symbolic link.
func (e TreeEntry) IsSymlink() bool { return e.Mode == 0o120000 }

// ParseTree parses raw tree data.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errors.New("gitstore: corrupt tree")
		}
		mode, err := strconv.ParseUint(string(data[:sp]), 8, 32)
		if err != nil {
			return nil, errors.New("gitstore: corrupt tree mode")
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			return nil, errors.New("gitstore: corrupt tree")
		}
		e := TreeEntry{Mode: uint32(mode), Name: string(data[:nul])}
		copy(e.Hash[:], data[nul+1:nul+21])
		out = append(out, e)
		data = data[nul+21:]
	}
	return out, nil
}

// SortTreeForDisplay puts directories first, then files, by name.
func SortTreeForDisplay(es []TreeEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		di, dj := es[i].IsDir(), es[j].IsDir()
		if di != dj {
			return di
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

// Commit reads and parses a commit, peeling annotated tags.
func (r *Repo) Commit(h Hash) (*Commit, error) {
	for i := 0; i < 10; i++ {
		t, data, err := r.Object(h)
		if err != nil {
			return nil, err
		}
		switch t {
		case ObjCommit:
			return ParseCommit(h, data)
		case ObjTag:
			tg, err := ParseTag(h, data)
			if err != nil {
				return nil, err
			}
			h = tg.Object
		default:
			return nil, fmt.Errorf("gitstore: %s is a %s, not a commit", h, t)
		}
	}
	return nil, errors.New("gitstore: tag chain too long")
}

// Tree reads and parses a tree.
func (r *Repo) Tree(h Hash) ([]TreeEntry, error) {
	t, data, err := r.Object(h)
	if err != nil {
		return nil, err
	}
	if t != ObjTree {
		return nil, fmt.Errorf("gitstore: %s is a %s, not a tree", h, t)
	}
	return ParseTree(data)
}

// Lookup resolves a slash-separated path inside the tree root. The empty
// path returns a synthetic directory entry for root itself.
func (r *Repo) Lookup(root Hash, path string) (TreeEntry, error) {
	cur := TreeEntry{Mode: 0o40000, Hash: root}
	path = strings.Trim(path, "/")
	if path == "" {
		return cur, nil
	}
	for _, part := range strings.Split(path, "/") {
		if !cur.IsDir() {
			return TreeEntry{}, ErrNotFound
		}
		es, err := r.Tree(cur.Hash)
		if err != nil {
			return TreeEntry{}, err
		}
		found := false
		for _, e := range es {
			if e.Name == part {
				cur, found = e, true
				break
			}
		}
		if !found {
			return TreeEntry{}, ErrNotFound
		}
	}
	return cur, nil
}

// IsAncestor reports whether anc is reachable from desc by following
// parents. It visits at most limit commits; complete is false when it gave
// up before deciding.
func (r *Repo) IsAncestor(anc, desc Hash, limit int) (found, complete bool) {
	if anc == desc {
		return true, true
	}
	seen := map[Hash]bool{desc: true}
	queue := []Hash{desc}
	for len(queue) > 0 {
		if len(seen) > limit {
			return false, false
		}
		h := queue[0]
		queue = queue[1:]
		c, err := r.Commit(h)
		if err != nil {
			continue // missing history (e.g. shallow): not an ancestor through here
		}
		for _, p := range c.Parents {
			if p == anc {
				return true, true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false, true
}
