package highlight

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

func lines(t *testing.T, name, src string) []string {
	t.Helper()
	hl := Lines(name, []byte(src))
	if hl == nil {
		t.Fatalf("%s: not highlighted", name)
	}
	out := make([]string, len(hl))
	for i, l := range hl {
		out[i] = string(l)
	}
	return out
}

var tags = regexp.MustCompile(`<[^>]*>`)

// The text of the highlighted lines must be exactly the source, line by line.
func checkText(t *testing.T, name, src string) {
	t.Helper()
	hl := Lines(name, []byte(src))
	if hl == nil {
		return
	}
	want := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	if len(hl) != len(want) {
		t.Fatalf("%s: %d lines, want %d", name, len(hl), len(want))
	}
	for i, l := range hl {
		got := html.UnescapeString(tags.ReplaceAllString(string(l), ""))
		if got != want[i] {
			t.Fatalf("%s line %d: %q, want %q", name, i+1, got, want[i])
		}
		if strings.Count(string(l), "<span") != strings.Count(string(l), "</span>") {
			t.Fatalf("%s line %d: unbalanced spans: %s", name, i+1, l)
		}
	}
}

func TestGo(t *testing.T) {
	src := "package main\n\n// Hello says hi.\nfunc Hello(n int) string {\n\treturn fmt.Sprintf(\"hi %d <b>\", n) /* multi\nline */ + `raw\nstring`\n}\n"
	got := lines(t, "main.go", src)
	checkText(t, "main.go", src)
	for _, want := range []string{
		`<span class="hk">package</span> main`,
		`<span class="hc">// Hello says hi.</span>`,
		`<span class="hk">func</span> <span class="hf">Hello</span>(n <span class="ht">int</span>) <span class="ht">string</span> {`,
		`<span class="hs">&#34;hi %d &lt;b&gt;&#34;</span>`,
		`<span class="hc">/* multi</span>`,
		`<span class="hc">line */</span>`,
		`<span class="hs">string`,
	} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("missing %q in:\n%s", want, strings.Join(got, "\n"))
		}
	}
}

func TestYAMLAndShell(t *testing.T) {
	y := lines(t, ".gitlab-ci.yml", "stages:\n  - build # first\njob:\n  script:\n    - echo \"$CI_COMMIT_SHA\"\n  when: manual\n")
	if y[0] != `<span class="ha">stages</span>:` || !strings.HasPrefix(y[1], `  - <span class="ha">`) && !strings.Contains(y[1], `  - build`) || !strings.Contains(y[1], `<span class="hc"># first</span>`) || !strings.Contains(y[5], `<span class="ha">when</span>`) {
		t.Errorf("yaml: %q", y)
	}
	sh := lines(t, "deploy", "#!/bin/sh\nfor f in $FILES; do echo ${f} $# # done\ndone\n")
	if !strings.Contains(sh[1], `<span class="hk">for</span>`) || !strings.Contains(sh[1], `<span class="hv">$FILES</span>`) ||
		!strings.Contains(sh[1], `<span class="hv">${f}</span>`) || !strings.Contains(sh[1], `<span class="hv">$#</span> <span class="hc"># done</span>`) {
		t.Errorf("shell: %q", sh)
	}
}

func TestJSONAndMarkup(t *testing.T) {
	j := lines(t, "package.json", `{"name": "x", "private": true, "n": 12}`)
	if !strings.Contains(j[0], `<span class="ha">&#34;name&#34;</span>: <span class="hs">&#34;x&#34;</span>`) ||
		!strings.Contains(j[0], `<span class="hb">true</span>`) || !strings.Contains(j[0], `<span class="hn">12</span>`) {
		t.Errorf("json: %q", j)
	}
	x := lines(t, "index.html", "<!-- c -->\n<a href=\"/x\" data-y='1'>T &amp; U</a>\n")
	if x[0] != `<span class="hc">&lt;!-- c --&gt;</span>` || !strings.Contains(x[1], `<span class="hk">a</span> <span class="ha">href</span>=<span class="hs">&#34;/x&#34;</span>`) ||
		!strings.Contains(x[1], `<span class="hb">&amp;amp;</span>`) {
		t.Errorf("html: %q", x)
	}
}

func TestDetectAndLimits(t *testing.T) {
	for name, want := range map[string]string{
		"Dockerfile": "Dockerfile", "Dockerfile.prod": "Dockerfile", "Makefile": "Makefile", "a/b/c.tsx": "JavaScript",
		"x.PY": "Python", "README.txt": "", "Cargo.toml": "TOML", "fix.patch": "Diff", ".env.local": "Config",
	} {
		if got := Name(name, nil); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if Name("run", []byte("#!/usr/bin/env python3\nprint(1)\n")) != "Python" {
		t.Error("shebang")
	}
	if Lines("min.js", []byte(strings.Repeat("a", MaxLineSize+1))) != nil {
		t.Error("minified file highlighted")
	}
	if Lines("big.go", make([]byte, MaxBytes+1)) != nil {
		t.Error("big file highlighted")
	}
}

func TestTextPreserved(t *testing.T) {
	for name, src := range map[string]string{
		"a.go":       "x := 'a' + \"unterminated\nnext\" /* open",
		"a.py":       "s = '''one\ntwo''' # c\n@deco\ndef f(): pass\r\n",
		"a.sh":       "echo 'it''s' \"a\\\"b\" `cmd`\n",
		"a.sql":      "SELECT * FROM t WHERE x = 'a''b' -- c\n",
		"a.xml":      "<a b=\"1\"><<c/></a\n<!-- open",
		"a.css":      "a:hover { color: #fff; } /* x */ @media (x) {}\n",
		"Dockerfile": "FROM alpine AS b\nRUN echo $HOME \\\n  && true\n",
		"a.diff":     "--- a\n+++ b\n@@ -1 +1 @@\n-x\n+y\n ctx",
		"a.toml":     "[server]\nport = 8080\nname = \"x\" # c\n",
		"a.rs":       "fn f<'a>(x: &'a str) -> Option<u8> { None }\n",
		"a.php":      "<?php $x = \"a\"; # c\n",
		"a.json":     "{\"a\": [1, 2.5e3, -3]}",
		"a.c":        "#include <stdio.h>\nint main() { char c = '\\''; }\n",
		"a.yml":      "- \"quoted key\": 1\n- plain\nk: v: w\n- name: x\n",
		"a.mk":       "all: build\n\t$(CC) -o $@ $^\n",
		"a.ts":       "const t = `a ${b}\nc`; // é ünïcode\n",
	} {
		checkText(t, name, src)
	}
}

func FuzzLines(f *testing.F) {
	f.Add("a.go", "func f() { return \"x\" }")
	f.Add("a.html", "<a b='c'>&amp;</a>")
	f.Add("a.yml", "k: 'v'\n- x: y")
	f.Add("a.sh", "echo ${x} $1 # c")
	f.Fuzz(func(t *testing.T, name, src string) {
		checkText(t, name, src)
	})
}

func TestYAMLListKey(t *testing.T) {
	y := lines(t, "a.yml", "steps:\n  - uses: x\n")
	if y[1] != `  - <span class="ha">uses</span>: x` {
		t.Errorf("%q", y[1])
	}
}

func BenchmarkGo512K(b *testing.B) {
	src := []byte(strings.Repeat("func f(x int) string {\n\t// comment\n\treturn fmt.Sprintf(\"%d\", x+42) /* c */\n}\n", 512<<10/80))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Lines("a.go", src)
	}
}
