package index

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/movenial/gitlab-backup-browser/internal/pgdump"
)

// parseStructure reads the tables and columns of GitLab's db/structure.sql.
func parseStructure(r io.Reader) (map[string][]string, error) {
	tables := map[string][]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur string
	for sc.Scan() {
		line := sc.Text()
		if cur == "" {
			// CREATE [UNLOGGED] TABLE [schema.]name ( ... or ... PARTITION OF ...;
			f := strings.Fields(line)
			if len(f) < 3 || f[0] != "CREATE" {
				continue
			}
			i := 1
			if f[i] == "UNLOGGED" {
				i++
			}
			if i+1 >= len(f) || f[i] != "TABLE" {
				continue
			}
			name := f[i+1]
			if j := strings.LastIndexByte(name, '.'); j >= 0 { // schema-qualified
				name = name[j+1:]
			}
			cur = strings.Trim(name, `"`)
			if _, ok := tables[cur]; !ok {
				tables[cur] = nil
			}
			if strings.HasSuffix(strings.TrimSpace(line), ";") { // CREATE TABLE x PARTITION OF y ...;
				cur = ""
			}
			continue
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, ")") {
			cur = ""
			continue
		}
		f := strings.Fields(t)
		if len(f) == 0 {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "EXCLUDE":
			continue
		}
		tables[cur] = append(tables[cur], strings.Trim(f[0], `"`))
	}
	return tables, sc.Err()
}

// checkSchema compares Schema with a GitLab schema: errors for what gbb
// needs (data would be wrong or missing), warnings for the rest.
func checkSchema(tables map[string][]string) (errs, warns []string) {
	report := func(n Need, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if n == Needed {
			errs = append(errs, msg)
		} else {
			warns = append(warns, msg)
		}
	}
	for _, s := range Schema {
		var candidates []*pgdump.Table
		for name, cols := range tables {
			t := &pgdump.Table{Name: name, Columns: cols}
			if s.Match != nil && s.Match(t) || s.Match == nil && name == s.Name {
				candidates = append(candidates, t)
			}
		}
		if len(candidates) == 0 {
			report(s.Need, "table %s missing: %s", s.Name, s.Effect)
			continue
		}
		for _, c := range s.Columns {
			found := false
			for _, t := range candidates {
				for _, n := range append([]string{c.Name}, c.Alts...) {
					if t.Col(n) >= 0 {
						found = true
					}
				}
			}
			if !found {
				report(c.Need, "column %s.%s missing: %s", s.Name, c.Name, c.Effect)
			}
		}
	}
	return errs, warns
}

const sampleStructure = `SET search_path=public;

CREATE TABLE issues (
    id bigint NOT NULL,
    title character varying,
    "state_id" smallint DEFAULT 1 NOT NULL,
    iid integer,
    project_id bigint,
    CONSTRAINT check_x CHECK ((char_length(title) <= 255))
);

CREATE TABLE gitlab_partitions_static.project_uploads (
    id bigint NOT NULL,
    path text NOT NULL,
    uploader text NOT NULL,
    store integer DEFAULT 1 NOT NULL,
    model_id bigint NOT NULL
);

CREATE TABLE p_ci_builds_1 PARTITION OF p_ci_builds FOR VALUES IN ('1');

CREATE UNLOGGED TABLE cache_thing (
    key text
);
`

func TestParseStructure(t *testing.T) {
	tables, err := parseStructure(strings.NewReader(sampleStructure))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tables["issues"], ","); got != "id,title,state_id,iid,project_id" {
		t.Errorf("issues: %s", got)
	}
	if len(tables["project_uploads"]) != 5 || tables["cache_thing"] == nil {
		t.Errorf("%v", tables)
	}
	if _, ok := tables["p_ci_builds_1"]; !ok {
		t.Error("partition")
	}
	errs, _ := checkSchema(tables)
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "table namespaces missing") || strings.Contains(joined, "issues.state_id") ||
		strings.Contains(joined, "*_uploads.path") {
		t.Errorf("errors:\n%s", joined)
	}
}

// TestSchemaAgainstGitLab checks Schema against real GitLab schemas:
// GBB_GITLAB_STRUCTURE lists db/structure.sql files (comma-separated),
// downloaded by the gitlab-schema CI workflow for several GitLab versions.
func TestSchemaAgainstGitLab(t *testing.T) {
	files := os.Getenv("GBB_GITLAB_STRUCTURE")
	if files == "" {
		t.Skip("GBB_GITLAB_STRUCTURE not set")
	}
	for _, p := range strings.Split(files, ",") {
		t.Run(p, func(t *testing.T) {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			tables, err := parseStructure(f)
			if err != nil {
				t.Fatal(err)
			}
			if len(tables) < 100 {
				t.Fatalf("only %d tables: not a GitLab structure.sql?", len(tables))
			}
			errs, warns := checkSchema(tables)
			for _, w := range warns {
				// Printed (not t.Log) so it shows in the CI job log even when the test passes.
				fmt.Printf("WARNING internal/index/schema.go: %s: %s\n", p, w)
			}
			for _, e := range errs {
				t.Errorf("%s", e)
			}
		})
	}
}
