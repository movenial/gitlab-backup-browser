package pgdump

import (
	"strings"
	"testing"
)

// fuzzHandler wants every table and exercises the column helpers.
type fuzzHandler struct{ rows int }

func (h *fuzzHandler) Want(t *Table) bool { return !strings.HasPrefix(t.Name, "skip") }
func (h *fuzzHandler) Row(t *Table, r Row) error {
	h.rows++
	for _, c := range t.Columns {
		Get(t, r, c)
		GetInt(t, r, c)
		GetBool(t, r, c)
	}
	Get(t, r, "missing")
	return nil
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(sample))
	f.Add([]byte("COPY x (a) FROM stdin;\n1\n"))
	f.Add([]byte("COPY x FROM stdin;\na\tb\n\\.\n"))
	f.Add([]byte("COPY skip_me (a) FROM stdin;\nzz\n\\.\nCOPY \"s\".\"t\"\"q\" (\"a\"\"b\", c) FROM stdin;\n\\N\t\\x4g\\777\\\n\\.\n"))
	f.Add([]byte("COPY x (a, b) FROM stdin;\r\n\\\\\t\\b\\f\\n\\r\\t\\v\\0\\x\r\n\\.\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		h := &fuzzHandler{}
		err := Parse(strings.NewReader(string(data)), h)
		if err == nil && h.rows > len(data) {
			t.Fatalf("%d rows from %d bytes", h.rows, len(data))
		}
	})
}

func FuzzUnescape(f *testing.F) {
	f.Add([]byte(`caf\303\251 \x41\\\N\t`))
	f.Add([]byte(`\`))
	f.Add([]byte(`\x`))
	f.Add([]byte(`\0\00\000\0000`))
	f.Fuzz(func(t *testing.T, field []byte) {
		if s := unescape(field); len(s) > len(field) {
			t.Fatalf("unescape grew %q to %q", field, s)
		}
		row, err := decodeRow(field, 0)
		if err != nil || len(row) != strings.Count(string(field), "\t")+1 {
			t.Fatalf("decodeRow(%q) = %d fields, %v", field, len(row), err)
		}
	})
}
