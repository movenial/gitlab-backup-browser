package backup

import "testing"

func TestLFSOIDOf(t *testing.T) {
	oid := "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	rel, ok := KeepLFS("lfs-objects/4d/7a/214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393")
	if !ok || LFSOIDOf(rel) != oid {
		t.Fatalf("%q %v -> %q", rel, ok, LFSOIDOf(rel))
	}
	if LFSOIDOf("x") != "" {
		t.Fatal("short path")
	}
	p := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 12\n"
	if got, size, ok := LFSPointer([]byte(p)); !ok || got != oid || size != 12 {
		t.Fatalf("pointer: %q %d %v", got, size, ok)
	}
	if _, _, ok := LFSPointer([]byte("version https://git-lfs.github.com/spec/v1\noid sha256:ABC\n")); ok {
		t.Fatal("bad oid accepted")
	}
}
