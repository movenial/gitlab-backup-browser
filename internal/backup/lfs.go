package backup

import (
	"bytes"
	"strconv"
)

// LFSPointer recognises a Git LFS pointer file and returns its object id
// and the size of the real content.
func LFSPointer(b []byte) (oid string, size int64, ok bool) {
	if len(b) > 1024 || !bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1\n")) {
		return "", 0, false
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		k, v, _ := bytes.Cut(line, []byte(" "))
		switch string(k) {
		case "oid":
			if h, found := bytes.CutPrefix(v, []byte("sha256:")); found && isOID(h) {
				oid = string(h)
			}
		case "size":
			size, _ = strconv.ParseInt(string(v), 10, 64)
		}
	}
	return oid, size, oid != ""
}

// isOID: 64 lowercase hex digits.
func isOID(b []byte) bool {
	if len(b) != 64 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// LFSOIDOf returns the object id of an LFS storage path as KeepLFS
// stores it (ab/cd/<rest>).
func LFSOIDOf(rel string) string {
	if len(rel) != 66 || rel[2] != '/' || rel[5] != '/' {
		return ""
	}
	return rel[:2] + rel[3:5] + rel[6:]
}
