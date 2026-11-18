package search

import (
	"context"
	"strings"
	"testing"
)

func TestMatcher(t *testing.T) {
	m, _ := Compile(Query{Pattern: "db_host"})
	if s, e := m.find([]byte("export DB_HOST=x")); s != 7 || e != 14 {
		t.Fatalf("fold: %d %d", s, e)
	}
	m, _ = Compile(Query{Pattern: "db_host", CaseSensitive: true})
	if s, _ := m.find([]byte("export DB_HOST=x")); s != -1 {
		t.Fatal("case-sensitive matched")
	}
	m, err := Compile(Query{Pattern: `port\s*=\s*\d+`, Regex: true})
	if err != nil {
		t.Fatal(err)
	}
	if s, e := m.find([]byte("  PORT = 5432 # db")); s != 2 || e != 13 {
		t.Fatalf("regex: %d %d", s, e)
	}
	if _, err := Compile(Query{Pattern: "(", Regex: true}); err == nil {
		t.Fatal("bad regex accepted")
	}
	if _, err := Compile(Query{Pattern: "  "}); err == nil {
		t.Fatal("empty accepted")
	}
	if _, err := Compile(Query{Pattern: strings.Repeat("a", MaxPattern+1)}); err == nil {
		t.Fatal("long accepted")
	}
	if indexFold([]byte("abcABC"), []byte("cab")) != 2 || indexFold([]byte("ab"), []byte("abc")) != -1 {
		t.Fatal("indexFold")
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("é", 400) + "NEEDLE" + strings.Repeat("x", 400)
	i := strings.Index(long, "NEEDLE")
	m := clip(3, []byte(long), i, i+6)
	if len(m.Text) > MaxLineLen || m.Text[m.Start:m.End] != "NEEDLE" {
		t.Fatalf("clip: %d %q", len(m.Text), m.Text[m.Start:m.End])
	}
	if !strings.HasPrefix(m.Text, "é") {
		t.Fatal("cut inside a rune")
	}
	_ = context.Background
}
