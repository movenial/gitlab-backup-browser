// Package pgdump streams the data sections of a plain-text pg_dump file
// (as produced by GitLab's backup task) without a PostgreSQL server.
//
// Only "COPY <table> (<cols>) FROM stdin;" blocks are understood. Every
// other statement is skipped. Rows are decoded from PostgreSQL's text COPY
// format: tab-separated fields, \N for NULL, backslash escapes.
package pgdump

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Row is one decoded COPY row. A nil entry means SQL NULL.
type Row []*string

// Table describes a COPY block header.
type Table struct {
	Name    string   // unqualified table name, e.g. "projects"
	Schema  string   // "public" or "" when the dump has no schema prefix
	Columns []string // column names in row order
}

// Col returns the index of the named column, or -1.
func (t *Table) Col(name string) int {
	for i, c := range t.Columns {
		if c == name {
			return i
		}
	}
	return -1
}

// Handler decides which tables to decode. Want is called once per COPY
// block; when it returns false the block is skipped cheaply. Row is called
// for every row of a wanted block.
type Handler interface {
	Want(t *Table) bool
	Row(t *Table, r Row) error
}

// maxRowLine bounds the memory spent on one row of a wanted table, and
// maxOtherLine on any other line (statements, COPY headers, rows of
// skipped tables), which only needs its first bytes to be recognised.
// GitLab caps descriptions and notes at 1M characters, so longer lines
// mean a damaged or hostile dump. Vars so tests can lower them.
var (
	maxRowLine   = 32 << 20
	maxOtherLine = 64 << 10
)

// Parse streams r (an uncompressed SQL dump) and feeds wanted tables to h.
// It never holds more than one line in memory.
func Parse(r io.Reader, h Handler) error {
	br := bufio.NewReaderSize(r, 256*1024)
	var (
		cur     *Table
		want    bool
		lineNo  int
		scratch []byte
	)
	for {
		limit := maxOtherLine
		if cur != nil && want {
			limit = maxRowLine
		}
		line, long, err := readLine(br, &scratch, limit)
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				if cur != nil {
					return fmt.Errorf("pgdump: unexpected end of dump inside COPY %s", cur.Name)
				}
				return nil
			}
			return err
		}
		lineNo++
		if cur == nil {
			if !bytes.HasPrefix(line, []byte("COPY ")) {
				continue
			}
			if long {
				return fmt.Errorf("pgdump: line %d: COPY header longer than %d bytes", lineNo, limit)
			}
			t, perr := parseCopyHeader(string(line))
			if perr != nil {
				return fmt.Errorf("pgdump: line %d: %w", lineNo, perr)
			}
			cur = t
			want = h.Want(t)
			continue
		}
		if len(line) == 2 && line[0] == '\\' && line[1] == '.' {
			cur = nil
			continue
		}
		if !want {
			continue
		}
		if long {
			return fmt.Errorf("pgdump: line %d (table %s): row longer than %d bytes", lineNo, cur.Name, limit)
		}
		row, derr := decodeRow(line, len(cur.Columns))
		if derr != nil {
			return fmt.Errorf("pgdump: line %d (table %s): %w", lineNo, cur.Name, derr)
		}
		if err := h.Row(cur, row); err != nil {
			return err
		}
	}
}

// readLine returns the next line without its trailing newline. The returned
// slice is only valid until the next call. A line longer than max is
// consumed entirely but only its first max bytes (or more, up to the
// reader's buffer size) are returned, with long set.
func readLine(br *bufio.Reader, scratch *[]byte, max int) (line []byte, long bool, err error) {
	line, err = br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		// Long line: accumulate up to max, then skip the rest.
		buf := append((*scratch)[:0], line...)
		for errors.Is(err, bufio.ErrBufferFull) {
			line, err = br.ReadSlice('\n')
			if len(buf)+len(line) > max {
				long = true
				continue
			}
			buf = append(buf, line...)
		}
		*scratch = buf
		line = buf
		if long {
			return line, true, err
		}
	}
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
	}
	return line, false, err
}

// parseCopyHeader parses `COPY public.projects (id, name, "order") FROM stdin;`.
func parseCopyHeader(s string) (*Table, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "COPY "))
	open := strings.IndexByte(s, '(')
	if open < 0 {
		// COPY without a column list: we cannot map fields reliably.
		name := strings.Fields(s)
		if len(name) == 0 {
			return nil, fmt.Errorf("bad COPY header %q", s)
		}
		schema, tbl := splitQualified(name[0])
		return &Table{Name: tbl, Schema: schema}, nil
	}
	closeIdx := strings.LastIndexByte(s, ')')
	if closeIdx < open {
		return nil, fmt.Errorf("bad COPY header %q", s)
	}
	schema, tbl := splitQualified(strings.TrimSpace(s[:open]))
	var cols []string
	for _, c := range strings.Split(s[open+1:closeIdx], ",") {
		cols = append(cols, unquoteIdent(strings.TrimSpace(c)))
	}
	return &Table{Name: tbl, Schema: schema, Columns: cols}, nil
}

func splitQualified(n string) (schema, table string) {
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		return unquoteIdent(n[:i]), unquoteIdent(n[i+1:])
	}
	return "", unquoteIdent(n)
}

func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// decodeRow splits a COPY text-format line into fields.
func decodeRow(line []byte, ncols int) (Row, error) {
	row := make(Row, 0, ncols)
	start := 0
	for i := 0; i <= len(line); i++ {
		if i == len(line) || line[i] == '\t' {
			f := line[start:i]
			if len(f) == 2 && f[0] == '\\' && f[1] == 'N' {
				row = append(row, nil)
			} else {
				v := unescape(f)
				row = append(row, &v)
			}
			start = i + 1
		}
	}
	if ncols > 0 && len(row) != ncols {
		return nil, fmt.Errorf("expected %d fields, got %d", ncols, len(row))
	}
	return row, nil
}

// unescape decodes PostgreSQL COPY text escapes.
func unescape(f []byte) string {
	if bytes.IndexByte(f, '\\') < 0 {
		return string(f)
	}
	out := make([]byte, 0, len(f))
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c != '\\' || i+1 >= len(f) {
			out = append(out, c)
			continue
		}
		i++
		switch e := f[i]; e {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, '\v')
		case 'x':
			j := i + 1
			for j < len(f) && j < i+3 && isHex(f[j]) {
				j++
			}
			if j == i+1 {
				out = append(out, 'x')
				continue
			}
			v, _ := strconv.ParseUint(string(f[i+1:j]), 16, 8)
			out = append(out, byte(v))
			i = j - 1
		default:
			if e >= '0' && e <= '7' {
				j := i
				for j < len(f) && j < i+3 && f[j] >= '0' && f[j] <= '7' {
					j++
				}
				v, _ := strconv.ParseUint(string(f[i:j]), 8, 16)
				out = append(out, byte(v))
				i = j - 1
				continue
			}
			out = append(out, e)
		}
	}
	return string(out)
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Get returns the string value of column name in r, and whether it was
// present and non-NULL.
func Get(t *Table, r Row, name string) (string, bool) {
	i := t.Col(name)
	if i < 0 || i >= len(r) || r[i] == nil {
		return "", false
	}
	return *r[i], true
}

// GetInt parses an integer column; ok is false on NULL, missing or bad data.
func GetInt(t *Table, r Row, name string) (int64, bool) {
	s, ok := Get(t, r, name)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// GetBool parses a boolean column ("t"/"f").
func GetBool(t *Table, r Row, name string) bool {
	s, ok := Get(t, r, name)
	return ok && (s == "t" || s == "true")
}
