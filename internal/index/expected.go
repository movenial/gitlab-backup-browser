package index

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// Expected lists the LFS objects and attachments the database knows about,
// so a check can tell whether lfs.tar.gz and uploads.tar.gz hold them all.
// Objects GitLab keeps in object storage are not in backups: they are only
// counted.
type Expected struct {
	Known         bool             `json:"known"` // the dump had the tables
	LFS           map[string]int64 `json:"lfs"`   // oid -> size, stored locally
	LFSRemote     int              `json:"lfs_remote"`
	Uploads       []string         `json:"uploads"` // paths inside uploads.tar.gz
	UploadsRemote int              `json:"uploads_remote"`
	UploadsOther  int              `json:"uploads_unchecked"` // uploaders whose layout is not known
	UploadsBytes  int64            `json:"uploads_bytes"`     // their total size on disk
}

// LFSBytes is the total size of the LFS objects stored on disk.
func (e *Expected) LFSBytes() int64 {
	var n int64
	for _, s := range e.LFS {
		n += s
	}
	return n
}

// WriteExpected stores e as JSON at p.
func WriteExpected(p string, e *Expected) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// ReadExpected loads what WriteExpected stored.
func ReadExpected(p string) (*Expected, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var e Expected
	return &e, json.Unmarshal(b, &e)
}

type uploadRow struct {
	path, uploader, modelType string
	modelID                   int64
}

// local reports whether a GitLab store/file_store value means "on disk"
// (1 or unset); 2 is object storage.
func local(store int64, ok bool) bool { return !ok || store != 2 }

// uploadMember is where an upload lives inside uploads.tar.gz, or "" when
// the uploader's layout is not known.
func uploadMember(u uploadRow, diskPaths map[int64]string) string {
	switch {
	case u.uploader == "FileUploader" && u.modelType == "Project":
		if dp := diskPaths[u.modelID]; dp != "" {
			return dp + "/" + u.path
		}
		return ""
	case u.uploader == "NamespaceFileUploader":
		return "-/system/namespace/" + itoa(u.modelID) + "/" + u.path
	case strings.HasPrefix(u.path, "uploads/"):
		return strings.TrimPrefix(u.path, "uploads/")
	}
	return ""
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
