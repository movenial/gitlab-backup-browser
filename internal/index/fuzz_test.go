package index

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad reads a persisted index: a damaged file must be rejected (and
// rebuilt by the caller), never crash the lookups that follow.
func FuzzLoad(f *testing.F) {
	f.Add([]byte(`{"version":4,"fingerprint":"fp","namespaces":[{"id":1,"full_path":"acme"},{"id":-1,"full_path":"_unmatched"}],` +
		`"projects":[{"id":1,"full_path":"acme/web","repo":"@hashed/aa","topics":["go"]}],` +
		`"repos":{"@hashed/aa":{"key":"@hashed/aa","steps":[{"bundle":"b","size":3}],"size":3}}}`))
	f.Add([]byte(`{"version":4,"fingerprint":"fp","projects":[null],"namespaces":[null],"repos":{"x":null}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "index.json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		ix, err := Load(p, "fp")
		if err != nil || ix == nil {
			return
		}
		ix.Stats()
		ix.Topics()
		ix.Children("")
		ix.Children("acme")
		ix.Project("acme/web")
		ix.Namespace("acme")
		for _, pr := range ix.Projects {
			pr.VisibilityName()
		}
	})
}
