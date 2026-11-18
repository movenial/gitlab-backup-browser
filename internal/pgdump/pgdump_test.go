package pgdump

import (
	"strings"
	"testing"
)

type collect struct {
	tables map[string]bool
	rows   map[string][]Row
	hdrs   map[string]*Table
}

func (c *collect) Want(t *Table) bool {
	c.hdrs[t.Name] = t
	return c.tables[t.Name]
}
func (c *collect) Row(t *Table, r Row) error {
	c.rows[t.Name] = append(c.rows[t.Name], r)
	return nil
}

const sample = `--
-- PostgreSQL database dump
--
SET statement_timeout = 0;
CREATE TABLE public.projects (id integer, name character varying);

COPY public.ci_builds (id, name) FROM stdin;
1	build
2	test
\.

COPY public.projects (id, name, description, "order") FROM stdin;
17	My project	Line one\nLine two\twith tab	\N
18	Back\\slash	caf\303\251 \x41	3
\.

COPY routes (id, source_id, path) FROM stdin;
1	17	group/my-project
\.
`

func TestParse(t *testing.T) {
	c := &collect{
		tables: map[string]bool{"projects": true, "routes": true},
		rows:   map[string][]Row{},
		hdrs:   map[string]*Table{},
	}
	if err := Parse(strings.NewReader(sample), c); err != nil {
		t.Fatal(err)
	}
	if len(c.rows["ci_builds"]) != 0 {
		t.Fatal("unwanted table decoded")
	}
	p := c.hdrs["projects"]
	if p.Schema != "public" || p.Col("order") != 3 {
		t.Fatalf("header: %+v", p)
	}
	r := c.rows["projects"]
	if len(r) != 2 {
		t.Fatalf("rows: %d", len(r))
	}
	if d, _ := Get(p, r[0], "description"); d != "Line one\nLine two\twith tab" {
		t.Fatalf("desc %q", d)
	}
	if _, ok := Get(p, r[0], "order"); ok {
		t.Fatal("expected NULL")
	}
	if n, _ := Get(p, r[1], "name"); n != `Back\slash` {
		t.Fatalf("name %q", n)
	}
	if d, _ := Get(p, r[1], "description"); d != "café A" {
		t.Fatalf("octal/hex %q", d)
	}
	rt := c.hdrs["routes"]
	if rt.Schema != "" || len(c.rows["routes"]) != 1 {
		t.Fatal("routes")
	}
	if id, ok := GetInt(rt, c.rows["routes"][0], "source_id"); !ok || id != 17 {
		t.Fatal("int")
	}
}

func TestTruncated(t *testing.T) {
	c := &collect{tables: map[string]bool{"x": true}, rows: map[string][]Row{}, hdrs: map[string]*Table{}}
	err := Parse(strings.NewReader("COPY x (a) FROM stdin;\n1\n"), c)
	if err == nil {
		t.Fatal("expected error for truncated dump")
	}
}

func TestLongLine(t *testing.T) {
	long := strings.Repeat("a", 1<<20)
	c := &collect{tables: map[string]bool{"x": true}, rows: map[string][]Row{}, hdrs: map[string]*Table{}}
	if err := Parse(strings.NewReader("COPY x (a) FROM stdin;\n"+long+"\n\\.\n"), c); err != nil {
		t.Fatal(err)
	}
	if v, _ := Get(c.hdrs["x"], c.rows["x"][0], "a"); len(v) != len(long) {
		t.Fatal("long line truncated")
	}
}

func TestLineLimit(t *testing.T) {
	defer func(n int) { maxRowLine = n }(maxRowLine)
	maxRowLine = 512 << 10
	long := strings.Repeat("a", 1<<20)
	c := &collect{tables: map[string]bool{"x": true}, rows: map[string][]Row{}, hdrs: map[string]*Table{}}
	// Overlong lines outside wanted tables are skipped without being kept.
	dump := "-- " + long + "\nCOPY y (a) FROM stdin;\n" + long + "\n\\.\nCOPY x (a) FROM stdin;\nok\n\\.\n"
	if err := Parse(strings.NewReader(dump), c); err != nil {
		t.Fatal(err)
	}
	if len(c.rows["x"]) != 1 {
		t.Fatal("rows after long lines lost")
	}
	err := Parse(strings.NewReader("COPY x (a) FROM stdin;\n"+long+"\n\\.\n"), c)
	if err == nil || !strings.Contains(err.Error(), "row longer than") {
		t.Fatalf("overlong row: %v", err)
	}
	err = Parse(strings.NewReader("COPY x ("+long+") FROM stdin;\n\\.\n"), c)
	if err == nil || !strings.Contains(err.Error(), "COPY header longer than") {
		t.Fatalf("overlong header: %v", err)
	}
}
