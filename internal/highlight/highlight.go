// Package highlight is a small syntax highlighter for the file view. It
// knows the shape of the common languages (comments, strings, numbers,
// keywords) rather than their grammar: enough to make code readable on a
// phone, with no dependency and a single pass over the file.
package highlight

import (
	"html"
	"html/template"
	"path"
	"strings"
)

// Limits past which a file is shown plain: highlighting is for reading,
// and huge or minified files are not read line by line.
const (
	MaxBytes    = 512 << 10
	MaxLineSize = 4000
)

// Lines returns the highlighted HTML of each line of src (same split as
// strings.Split(strings.TrimSuffix(src, "\n"), "\n")), or nil when the
// language is unknown or the file too big.
func Lines(name string, src []byte) []template.HTML {
	if len(src) == 0 || len(src) > MaxBytes {
		return nil
	}
	l := Detect(name, src)
	if l == nil {
		return nil
	}
	s := strings.TrimSuffix(string(src), "\n")
	for rest := s; rest != ""; {
		i := strings.IndexByte(rest, '\n')
		if i < 0 {
			i = len(rest)
		}
		if i > MaxLineSize {
			return nil
		}
		if i == len(rest) {
			break
		}
		rest = rest[i+1:]
	}
	w := &writer{}
	switch l.mode {
	case modeMarkup:
		markup(w, s)
	case modeDiff:
		diff(w, s)
	default:
		code(w, l, s)
	}
	return w.done()
}

// Name returns the language name for a file, or "" if unknown.
func Name(name string, src []byte) string {
	if l := Detect(name, src); l != nil {
		return l.name
	}
	return ""
}

// writer collects tokens into per-line HTML, closing and reopening spans
// at line ends so every line stands alone in the table.
type writer struct {
	cur   strings.Builder
	lines []template.HTML
}

func (w *writer) emit(class, text string) {
	for {
		i := strings.IndexByte(text, '\n')
		part := text
		if i >= 0 {
			part = text[:i]
		}
		if part != "" {
			if class == "" {
				w.cur.WriteString(html.EscapeString(part))
			} else {
				w.cur.WriteString(`<span class="h`)
				w.cur.WriteString(class)
				w.cur.WriteString(`">`)
				w.cur.WriteString(html.EscapeString(part))
				w.cur.WriteString(`</span>`)
			}
		}
		if i < 0 {
			return
		}
		w.lines = append(w.lines, template.HTML(w.cur.String()))
		w.cur.Reset()
		text = text[i+1:]
	}
}

func (w *writer) done() []template.HTML {
	w.lines = append(w.lines, template.HTML(w.cur.String()))
	return w.lines
}

// Token classes (CSS class "h" + letter).
const (
	cKeyword  = "k"
	cString   = "s"
	cComment  = "c"
	cNumber   = "n"
	cConst    = "b"
	cType     = "t"
	cFunc     = "f"
	cAttr     = "a"
	cVar      = "v"
	cAdded    = "g"
	cDeleted  = "d"
	cMeta     = "m"
	cHeading  = "h"
	cTag      = "k"
	cPunctTag = "p"
)

type mode int

const (
	modeCode mode = iota
	modeMarkup
	modeDiff
)

type strDelim struct {
	open, close string
	multiline   bool
	raw         bool // no backslash escapes
}

type lang struct {
	name      string
	mode      mode
	line      []string    // line comment openers
	block     [][2]string // block comment open/close
	strs      []strDelim
	keywords  map[string]bool
	consts    map[string]bool
	types     map[string]bool
	fold      bool            // keywords are case-insensitive
	keyLine   bool            // "key:" / "key =" at line start is an attribute (YAML, TOML, ini)
	sections  bool            // "[section]" lines (TOML, ini)
	jsonKeys  bool            // a string followed by ':' is a key
	vars      bool            // $name, ${...}
	at        bool            // @decorator / @rule
	lineStart map[string]bool // keywords only recognised first on a line (Dockerfile)
	hexColors bool            // #fff, #a0b1c2 are numbers (CSS)
}

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdent(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// code highlights C-like, script and config languages.
func code(w *writer, l *lang, s string) {
	plainStart := 0
	flush := func(i int) {
		if i > plainStart {
			w.emit("", s[plainStart:i])
		}
	}
	tok := func(class string, from, to int) {
		flush(from)
		w.emit(class, s[from:to])
		plainStart = to
	}
	lineStart := true
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\n' {
			lineStart = true
			i++
			continue
		}
		if lineStart {
			lineStart = false
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			if l.sections && j < len(s) && s[j] == '[' {
				end := strings.IndexByte(s[j:], '\n')
				if end < 0 {
					end = len(s) - j
				}
				if k := strings.LastIndexByte(s[j:j+end], ']'); k > 0 {
					tok(cKeyword, j, j+k+1)
					i = j + k + 1
					continue
				}
			}
			if l.keyLine {
				if st, k := keyAt(s, j); k > st {
					tok(cAttr, st, k)
					i = k
					continue
				}
			}
			if l.lineStart != nil {
				k := j
				for k < len(s) && isIdent(s[k]) {
					k++
				}
				if k > j && l.lineStart[strings.ToUpper(s[j:k])] {
					tok(cKeyword, j, k)
					i = k
					continue
				}
			}
		}
		// Comments.
		if end, ok := l.comment(s, i); ok {
			tok(cComment, i, end)
			i = end
			continue
		}
		// Strings.
		if end, ok := l.str(s, i); ok {
			class := cString
			if l.jsonKeys {
				k := end
				for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
					k++
				}
				if k < len(s) && s[k] == ':' {
					class = cAttr
				}
			}
			tok(class, i, end)
			i = end
			continue
		}
		// Variables.
		if l.vars && c == '$' && i+1 < len(s) {
			if s[i+1] == '{' {
				if k := strings.IndexByte(s[i:], '}'); k > 0 && !strings.Contains(s[i:i+k], "\n") {
					tok(cVar, i, i+k+1)
					i += k + 1
					continue
				}
			} else if isIdent(s[i+1]) || strings.IndexByte("@*#?!$0", s[i+1]) >= 0 {
				k := i + 2
				if isIdent(s[i+1]) {
					for k < len(s) && isIdent(s[k]) {
						k++
					}
				}
				tok(cVar, i, k)
				i = k
				continue
			}
		}
		// Decorators, annotations, CSS at-rules.
		if l.at && c == '@' && i+1 < len(s) && isIdentStart(s[i+1]) && (i == 0 || !isIdent(s[i-1])) {
			k := i + 1
			for k < len(s) && (isIdent(s[k]) || s[k] == '.' || s[k] == '-') {
				k++
			}
			tok(cMeta, i, k)
			i = k
			continue
		}
		if l.hexColors && c == '#' {
			k := i + 1
			for k < len(s) && (isDigit(s[k]) || s[k] >= 'a' && s[k] <= 'f' || s[k] >= 'A' && s[k] <= 'F') {
				k++
			}
			if n := k - i - 1; (n == 3 || n == 4 || n == 6 || n == 8) && (k == len(s) || !isIdent(s[k])) {
				tok(cNumber, i, k)
				i = k
				continue
			}
		}
		// Numbers.
		if isDigit(c) && (i == 0 || !isIdent(s[i-1])) {
			k := i + 1
			for k < len(s) && (isIdent(s[k]) || s[k] == '.' && k+1 < len(s) && isDigit(s[k+1])) {
				k++
			}
			tok(cNumber, i, k)
			i = k
			continue
		}
		// Words.
		if isIdentStart(c) {
			k := i + 1
			for k < len(s) && isIdent(s[k]) {
				k++
			}
			word := s[i:k]
			key := word
			if l.fold {
				key = strings.ToLower(word)
			}
			switch {
			case l.keywords[key]:
				tok(cKeyword, i, k)
			case l.consts[key]:
				tok(cConst, i, k)
			case l.types[key]:
				tok(cType, i, k)
			case k < len(s) && s[k] == '(':
				tok(cFunc, i, k)
			}
			i = k
			continue
		}
		i++
	}
	flush(len(s))
}

// keyAt returns the end of a "key:" or "key =" at position j (YAML, TOML,
// ini, Makefile) at position j: start and end of the key, equal when
// there is none. A YAML list dash is not part of the key.
func keyAt(s string, j int) (int, int) {
	k := j
	if k+1 < len(s) && s[k] == '-' && s[k+1] == ' ' { // YAML list item
		k += 2
		j = k
	}
	if k < len(s) && (s[k] == '"' || s[k] == '\'') {
		q := s[k]
		e := strings.IndexByte(s[k+1:], q)
		if e < 0 {
			return j, j
		}
		k += e + 2
	} else {
		for k < len(s) && (isIdent(s[k]) || s[k] == '-' || s[k] == '.' || s[k] == '/') {
			k++
		}
	}
	if k == j {
		return j, j
	}
	e := k
	for e < len(s) && (s[e] == ' ' || s[e] == '\t') {
		e++
	}
	if e < len(s) && (s[e] == ':' && (e+1 == len(s) || s[e+1] == ' ' || s[e+1] == '\n' || s[e+1] == '\r') || s[e] == '=') {
		return j, k
	}
	return j, j
}

func (l *lang) comment(s string, i int) (int, bool) {
	for _, b := range l.block {
		if strings.HasPrefix(s[i:], b[0]) {
			e := strings.Index(s[i+len(b[0]):], b[1])
			if e < 0 {
				return len(s), true
			}
			return i + len(b[0]) + e + len(b[1]), true
		}
	}
	for _, lc := range l.line {
		if strings.HasPrefix(s[i:], lc) {
			// "#" starts a comment only at a word boundary in shell-like
			// languages ("a#b", "$#" are not comments).
			if lc == "#" && i > 0 && s[i-1] != ' ' && s[i-1] != '\t' && s[i-1] != '\n' && s[i-1] != ';' {
				continue
			}
			e := strings.IndexByte(s[i:], '\n')
			if e < 0 {
				return len(s), true
			}
			return i + e, true
		}
	}
	return 0, false
}

func (l *lang) str(s string, i int) (int, bool) {
	for _, d := range l.strs {
		if !strings.HasPrefix(s[i:], d.open) {
			continue
		}
		// A quote inside a word is an apostrophe ("don't" in a comment-less
		// language, Rust lifetimes 'a).
		if d.open == "'" && i > 0 && isIdent(s[i-1]) {
			continue
		}
		j := i + len(d.open)
		for j < len(s) {
			if !d.raw && s[j] == '\\' {
				j += 2
				continue
			}
			if strings.HasPrefix(s[j:], d.close) {
				return j + len(d.close), true
			}
			if s[j] == '\n' && !d.multiline {
				return j, true
			}
			j++
		}
		if j > len(s) {
			j = len(s)
		}
		return j, true
	}
	return 0, false
}

// markup highlights HTML, XML and SVG: tags, attributes, strings, comments.
func markup(w *writer, s string) {
	plainStart := 0
	tok := func(class string, from, to int) {
		if from > plainStart {
			w.emit("", s[plainStart:from])
		}
		w.emit(class, s[from:to])
		plainStart = to
	}
	i := 0
outer:
	for i < len(s) {
		if s[i] != '<' {
			if s[i] == '&' {
				if k := strings.IndexByte(s[i:], ';'); k > 1 && k < 12 {
					tok(cConst, i, i+k+1)
					i += k + 1
					continue
				}
			}
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "<!--") {
			e := strings.Index(s[i+4:], "-->")
			end := len(s)
			if e >= 0 {
				end = i + 4 + e + 3
			}
			tok(cComment, i, end)
			i = end
			continue
		}
		j := i + 1
		if j < len(s) && (s[j] == '/' || s[j] == '!' || s[j] == '?') {
			j++
		}
		k := j
		for k < len(s) && (isIdent(s[k]) || s[k] == '-' || s[k] == ':' || s[k] == '.') {
			k++
		}
		if k == j {
			i++
			continue
		}
		tok(cPunctTag, i, j)
		tok(cTag, j, k)
		i = k
		// Attributes until '>'.
		for i < len(s) && s[i] != '>' {
			c := s[i]
			switch {
			case c == '"' || c == '\'':
				e := strings.IndexByte(s[i+1:], c)
				end := len(s)
				if e >= 0 {
					end = i + 1 + e + 1
				}
				tok(cString, i, end)
				i = end
			case isIdentStart(c):
				e := i + 1
				for e < len(s) && (isIdent(s[e]) || s[e] == '-' || s[e] == ':' || s[e] == '.') {
					e++
				}
				tok(cAttr, i, e)
				i = e
			case c == '<': // broken markup: start over
				continue outer
			default:
				i++
			}
		}
		if i < len(s) {
			start := i
			if i > 0 && s[i-1] == '/' || i > 0 && s[i-1] == '?' {
				start = i - 1
				if start < plainStart {
					start = i
				}
			}
			tok(cPunctTag, start, i+1)
			i++
		}
	}
	if plainStart < len(s) {
		w.emit("", s[plainStart:])
	}
}

// diff colours patch files line by line.
func diff(w *writer, s string) {
	for n, line := range strings.Split(s, "\n") {
		if n > 0 {
			w.emit("", "\n")
		}
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index "):
			w.emit(cKeyword, line)
		case strings.HasPrefix(line, "@@"):
			w.emit(cMeta, line)
		case strings.HasPrefix(line, "+"):
			w.emit(cAdded, line)
		case strings.HasPrefix(line, "-"):
			w.emit(cDeleted, line)
		default:
			w.emit("", line)
		}
	}
}

// Detect picks the language of a file from its name, or its first line.
func Detect(name string, src []byte) *lang {
	base := path.Base(name)
	lower := strings.ToLower(base)
	if l, ok := byName[lower]; ok {
		return l
	}
	if strings.HasPrefix(lower, "dockerfile") || strings.HasSuffix(lower, ".dockerfile") || strings.HasPrefix(lower, "containerfile") {
		return langs["dockerfile"]
	}
	if strings.HasPrefix(lower, ".env") {
		return langs["ini"]
	}
	ext := strings.TrimPrefix(path.Ext(lower), ".")
	if l, ok := byExt[ext]; ok {
		return l
	}
	// Shebang.
	if len(src) > 2 && src[0] == '#' && src[1] == '!' {
		first := string(src[:min(len(src), 120)])
		if i := strings.IndexByte(first, '\n'); i > 0 {
			first = first[:i]
		}
		switch {
		case strings.Contains(first, "python"):
			return langs["python"]
		case strings.Contains(first, "node"):
			return langs["js"]
		case strings.Contains(first, "ruby"):
			return langs["ruby"]
		case strings.Contains(first, "perl"):
			return langs["perl"]
		case strings.Contains(first, "sh"):
			return langs["shell"]
		}
	}
	return nil
}
