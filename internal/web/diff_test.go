package web

import (
	"strings"
	"testing"
)

func apply(a, b []string, ops []byte) []string {
	var out []string
	i, j := 0, 0
	for _, op := range ops {
		switch op {
		case ' ':
			out = append(out, a[i])
			i++
			j++
		case '-':
			i++
		case '+':
			out = append(out, b[j])
			j++
		}
	}
	return out
}

func TestMyers(t *testing.T) {
	cases := [][2]string{
		{"a b c d", "a x c d"},
		{"", "a b"},
		{"a b", ""},
		{"a b c a b b a", "c b a b a c"},
		{"same", "same"},
	}
	for _, c := range cases {
		a, b := strings.Fields(c[0]), strings.Fields(c[1])
		ops, ok := myers(a, b)
		if !ok {
			t.Fatal("not ok")
		}
		if got := strings.Join(apply(a, b, ops), " "); got != strings.Join(b, " ") {
			t.Fatalf("%q -> %q: got %q", c[0], c[1], got)
		}
	}
	lines, ok := unified(strings.Fields("1 2 3 4 5 6 7 8 9"), strings.Fields("1 2 3 4 X 6 7 8 9"), 1)
	if !ok || len(lines) != 5 || lines[0].Kind != '@' { // gap before line 4
		t.Fatalf("unified %+v", lines)
	}
	lines, _ = unified([]string{"a"}, []string{"b"}, 3)
	if lines[0].Kind == '@' {
		t.Fatal("no gap marker expected at the very top")
	}
	big := make([]string, 2000)
	other := make([]string, 2000)
	for i := range big {
		big[i] = "a" + strings.Repeat("x", i%7) + string(rune('0'+i%10))
		other[i] = "b" + big[i]
	}
	if _, ok := myers(big, other); ok {
		t.Fatal("expected bail-out on huge rewrite")
	}
}
