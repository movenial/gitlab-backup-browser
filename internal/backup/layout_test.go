package backup

import (
	"strings"
	"testing"
)

func TestBackupInfo(t *testing.T) {
	gitlab19 := `---
:db_version: '20251020000000'
:backup_created_at: !ruby/object:ActiveSupport::TimeWithZone
  utc: 2026-11-03 02:00:30.123456789 Z
  zone: !ruby/object:ActiveSupport::TimeZone
    name: Etc/UTC
  time: 2026-11-03 02:00:30.123456789 Z
:gitlab_version: 19.5.3
:tar_version: tar (GNU tar) 1.35
:installation_type: omnibus-gitlab
:skipped: ''
:repositories_storages: ''
`
	got := BackupInfo(strings.NewReader(gitlab19))
	if got["backup_created_at"] != "2026-11-03 02:00:30 UTC" {
		t.Fatalf("date %q", got["backup_created_at"])
	}
	if got["gitlab_version"] != "19.5.3" || got["db_version"] != "20251020000000" {
		t.Fatalf("info %v", got)
	}
	if _, ok := got["skipped"]; ok {
		t.Fatal("empty values should be dropped")
	}
	old := BackupInfo(strings.NewReader(":backup_created_at: 2024-10-27 10:00:00 UTC\n:gitlab_version: 17.5.1\n"))
	if old["backup_created_at"] != "2024-10-27 10:00:00 UTC" {
		t.Fatalf("old format %v", old)
	}
}

func TestLayout(t *testing.T) {
	es := []Entry{
		{Name: "repositories/default/@hashed/aa/bb/h.git/2_b/002.bundle", Size: 1},
		{Name: "repositories/default/@hashed/aa/bb/h.git/1_a/001.bundle", Size: 2},
		{Name: "repositories/default/@hashed/aa/bb/h.git/2_b/001.bundle", Size: 3},
		{Name: "repositories/default/@hashed/aa/bb/h.git/2_b/001.refs", Size: 3},
		{Name: "repositories/default/@hashed/aa/bb/h.git/LATEST", Size: 3},
		{Name: "repositories/@hashed/cc/dd/x.wiki.bundle", Size: 4},
		{Name: "repositories/@snippets/aa/bb/s.bundle", Size: 4},
		{Name: "repositories/group/project.bundle", Size: 4},
	}
	l := Layout(es)
	h := l["@hashed/aa/bb/h"]
	if h == nil || len(h.Steps) != 3 || h.Steps[0].BackupID != "1_a" || h.Steps[2].Seq != 2 || h.Steps[1].Refs == "" {
		t.Fatalf("gitaly layout %+v", h)
	}
	if l["@hashed/cc/dd/x.wiki"] == nil || l["@hashed/cc/dd/x.wiki"].Kind() != "wiki" {
		t.Fatal("classic wiki")
	}
	if l["group/project"] == nil {
		t.Fatal("legacy")
	}
	found := false
	for k := range l {
		if strings.HasPrefix(k, "@snippets/") {
			found = true
		}
	}
	if !found {
		t.Fatal("snippet repositories are browsable now and must be listed")
	}
}
