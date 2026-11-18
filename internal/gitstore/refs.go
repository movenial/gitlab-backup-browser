package gitstore

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// preferredHeads are tried, in order, when HEAD's target is unknown.
var preferredHeads = []string{"refs/heads/main", "refs/heads/master", "refs/heads/develop", "refs/heads/trunk"}

// peel follows annotated tags to the tagged object.
func (r *Repo) peel(h Hash) (Hash, bool) {
	peeled := false
	for i := 0; i < 10; i++ {
		t, data, err := r.Object(h)
		if err != nil || t != ObjTag {
			return h, peeled
		}
		tg, err := ParseTag(h, data)
		if err != nil {
			return h, peeled
		}
		h, peeled = tg.Object, true
	}
	return h, peeled
}

// guessHead maps a detached "HEAD <oid>" entry from a ref list (as found
// in bundles) to the branch it most likely points at. It returns "" when
// the list has no HEAD entry.
func guessHead(refs []Ref) string {
	var headHash Hash
	found := false
	byName := map[string]Hash{}
	var names []string
	for _, r := range refs {
		if r.Name == "HEAD" {
			headHash, found = r.Hash, true
			continue
		}
		byName[r.Name] = r.Hash
		names = append(names, r.Name)
	}
	if !found {
		return ""
	}
	for _, p := range preferredHeads {
		if h, ok := byName[p]; ok && h == headHash {
			return p
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasPrefix(n, "refs/heads/") && byName[n] == headHash {
			return n
		}
	}
	return ""
}

// Advertised reports whether a ref is shown to git clients. Like GitLab,
// internal refs (keep-around, pipelines, environments, tmp...) are hidden;
// they stay in packed-refs for the browser's own use.
func Advertised(name string) bool {
	return strings.HasPrefix(name, "refs/heads/") ||
		strings.HasPrefix(name, "refs/tags/") ||
		strings.HasPrefix(name, "refs/merge-requests/")
}

// InfoRefs renders the dumb-HTTP info/refs file from packed-refs, listing
// advertised refs only.
func (r *Repo) InfoRefs() ([]byte, error) {
	refs, _, err := r.Refs()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, ref := range refs {
		if !Advertised(ref.Name) {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", ref.Hash, ref.Name)
		if !ref.Peeled.IsZero() {
			fmt.Fprintf(&b, "%s\t%s^{}\n", ref.Peeled, ref.Name)
		}
	}
	return []byte(b.String()), nil
}

// PlanRefs merges the ref lists of a bundle chain as a build does: later
// bundles move refs, a Gitaly .refs file (final) replaces them all. It
// returns the refs in first-seen order (bundle HEAD entries included) and
// the branch HEAD points at, if known. Reading only bundle headers, it
// tells the refs a repository will have before it is unbundled.
func PlanRefs(headers [][]Ref, final *RefSet) ([]Ref, string) {
	refs := map[string]Hash{}
	var order []string
	set := func(name string, h Hash) {
		if _, ok := refs[name]; !ok {
			order = append(order, name)
		}
		refs[name] = h
	}
	headRef := ""
	for _, hdr := range headers {
		for _, r := range hdr {
			set(r.Name, r.Hash)
		}
		// Pin HEAD to a branch name while the bundle's own refs are at
		// hand: a later incremental may move that branch.
		if h := guessHead(hdr); h != "" {
			headRef = h
		}
	}
	if final != nil && len(final.Refs) > 0 {
		refs, order = map[string]Hash{}, nil
		for _, r := range final.Refs {
			set(r.Name, r.Hash)
		}
		if final.HeadRef != "" {
			headRef = final.HeadRef
		} else if h := guessHead(final.Refs); h != "" {
			headRef = h
		}
	}
	out := make([]Ref, 0, len(order))
	for _, n := range order {
		out = append(out, Ref{Name: n, Hash: refs[n]})
	}
	return out, headRef
}

// ChooseHead returns the branch HEAD points at: headRef if it exists, else
// a usual default branch, else the first branch.
func ChooseHead(refs []Ref, headRef string) string {
	has := map[string]bool{}
	for _, r := range refs {
		has[r.Name] = true
	}
	if has[headRef] {
		return headRef
	}
	for _, p := range preferredHeads {
		if has[p] {
			return p
		}
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/heads/") {
			names = append(names, r.Name)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return names[0]
	}
	return "refs/heads/main"
}

// writeRefs writes HEAD, packed-refs, config and the dumb-HTTP helper
// files (info/refs, objects/info/packs).
func writeRefs(repo *Repo, dir string, refs []Ref, headRef string) error {
	var real []Ref
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "refs/") {
			continue
		}
		real = append(real, r)
	}
	sort.Slice(real, func(i, j int) bool { return real[i].Name < real[j].Name })

	headRef = ChooseHead(real, headRef)

	var packed, info strings.Builder
	packed.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for _, r := range real {
		adv := Advertised(r.Name)
		fmt.Fprintf(&packed, "%s %s\n", r.Hash, r.Name)
		if adv {
			fmt.Fprintf(&info, "%s\t%s\n", r.Hash, r.Name)
		}
		if strings.HasPrefix(r.Name, "refs/tags/") {
			if p, ok := repo.peel(r.Hash); ok {
				fmt.Fprintf(&packed, "^%s\n", p)
				if adv {
					fmt.Fprintf(&info, "%s\t%s^{}\n", p, r.Name)
				}
			}
		}
	}
	var packs strings.Builder
	for _, p := range repo.Packs() {
		fmt.Fprintf(&packs, "P %s.pack\n", p)
	}
	packs.WriteString("\n")

	files := map[string]string{
		"HEAD":               "ref: " + headRef + "\n",
		"packed-refs":        packed.String(),
		"info/refs":          info.String(),
		"objects/info/packs": packs.String(),
		"config":             "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n",
		"description":        "Restored from a GitLab backup by gitlab-backup-browser\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RefInfo is a ref as read back from a built repository.
type RefInfo struct {
	Name   string
	Hash   Hash
	Peeled Hash // zero unless an annotated tag
}

// ShortName strips refs/heads/ or refs/tags/.
func (r RefInfo) ShortName() string {
	if s, ok := strings.CutPrefix(r.Name, "refs/heads/"); ok {
		return s
	}
	if s, ok := strings.CutPrefix(r.Name, "refs/tags/"); ok {
		return s
	}
	return r.Name
}

// Target returns the commit-ish the ref points at (peeled if a tag).
func (r RefInfo) Target() Hash {
	if !r.Peeled.IsZero() {
		return r.Peeled
	}
	return r.Hash
}

// Refs reads packed-refs and the symbolic HEAD of a built repository.
func (r *Repo) Refs() (refs []RefInfo, head string, err error) {
	if b, err := os.ReadFile(filepath.Join(r.Dir, "HEAD")); err == nil {
		head = strings.TrimSpace(strings.TrimPrefix(string(b), "ref:"))
	}
	f, err := os.Open(filepath.Join(r.Dir, "packed-refs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, head, nil
		}
		return nil, head, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '^' {
			if len(refs) > 0 {
				if h, err := ParseHash(line[1:]); err == nil {
					refs[len(refs)-1].Peeled = h
				}
			}
			continue
		}
		id, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		h, err := ParseHash(id)
		if err != nil {
			continue
		}
		refs = append(refs, RefInfo{Name: name, Hash: h})
	}
	return refs, head, sc.Err()
}

// Resolve turns a branch name, tag name, full ref or hex id into a
// commit-ish hash. Ancestors can be named as in git: main~3 (third
// first-parent ancestor), main^ or main^2 (first or second parent).
func (r *Repo) Resolve(name string) (Hash, string, error) {
	if i := strings.IndexAny(name, "~^"); i > 0 {
		h, _, err := r.resolveName(name[:i])
		if err != nil {
			return ZeroHash, "", err
		}
		h, err = r.ancestor(h, name[i:])
		return h, "", err
	}
	return r.resolveName(name)
}

// ancestor applies git's ~N and ^N suffixes to commit h.
func (r *Repo) ancestor(h Hash, ops string) (Hash, error) {
	for ops != "" {
		op := ops[0]
		if op != '~' && op != '^' { // "main~1/README.md" is a ref and a path, not more ancestors
			return ZeroHash, fmt.Errorf("%w: bad ancestor suffix %q", ErrNotFound, ops)
		}
		ops = ops[1:]
		n := 0
		digits := 0
		for digits < len(ops) && ops[digits] >= '0' && ops[digits] <= '9' && digits < 6 {
			n = n*10 + int(ops[digits]-'0')
			digits++
		}
		ops = ops[digits:]
		if digits == 0 {
			n = 1
		}
		if ops != "" && ops[0] >= '0' && ops[0] <= '9' {
			return ZeroHash, fmt.Errorf("%w: number too large", ErrNotFound)
		}
		h, _ = r.peel(h)
		steps, parent := n, 0 // ~n: n first parents; ^n: the n-th parent once
		if op == '^' {
			steps, parent = 1, n-1
			if n == 0 { // ^0 is the commit itself
				steps = 0
			}
		}
		for ; steps > 0; steps-- {
			c, err := r.Commit(h)
			if err != nil {
				return ZeroHash, err
			}
			if parent >= len(c.Parents) {
				return ZeroHash, fmt.Errorf("%w: no such ancestor", ErrNotFound)
			}
			h = c.Parents[parent]
		}
	}
	return h, nil
}

func (r *Repo) resolveName(name string) (Hash, string, error) {
	refs, head, err := r.Refs()
	if err != nil {
		return ZeroHash, "", err
	}
	if name == "" || name == "HEAD" {
		name = head
	}
	for _, cand := range []string{name, "refs/heads/" + name, "refs/tags/" + name} {
		for _, ref := range refs {
			if ref.Name == cand {
				return ref.Target(), ref.Name, nil
			}
		}
	}
	if h, err := ParseHash(name); err == nil && r.Has(h) {
		return h, "", nil
	}
	if isHexPrefix(name) { // a short id, as git prints them
		if h, err := r.ResolvePrefix(name); err == nil {
			return h, "", nil
		} else if errors.Is(err, ErrAmbiguous) {
			return ZeroHash, "", err
		}
	}
	return ZeroHash, "", fmt.Errorf("%w: ref %q", ErrNotFound, name)
}

func isHexPrefix(s string) bool {
	if len(s) < 4 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
