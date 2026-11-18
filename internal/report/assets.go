package report

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"sort"
	"strings"

	"github.com/movenial/gitlab-backup-browser/internal/backup"
)

// AssetCheck compares what the database lists with an archive of the
// backup (lfs.tar.gz or uploads.tar.gz).
type AssetCheck struct {
	Archive   string   `json:"archive"`
	Present   bool     `json:"present"`  // the archive is in the backup
	Skipped   bool     `json:"skipped"`  // backup_information.yml says it was skipped
	Expected  int      `json:"expected"` // stored on disk by GitLab, so expected in the backup
	Found     int      `json:"found"`
	Missing   int      `json:"missing"`
	Sample    []string `json:"missing_sample,omitempty"` // first missing items
	Remote    int      `json:"object_storage"`           // in object storage: never in backups
	Unchecked int      `json:"unchecked,omitempty"`      // attachments of unknown layout
	Error     string   `json:"error,omitempty"`
}

const missingSample = 20

// checkAssets reads the member names of lfs.tar.gz and uploads.tar.gz
// (decompressing them, without writing anything) and compares them with
// the database. Memory: one set of names at a time.
func (r *Report) checkAssets(cur *Loaded, progress func(name string)) {
	e := cur.Expected
	if e == nil || !e.Known {
		return
	}
	skipped := cur.Index.Info["skipped"]
	lfs := &AssetCheck{Archive: backup.LFSMember, Remote: e.LFSRemote, Expected: len(e.LFS)}
	uploads := &AssetCheck{Archive: backup.UploadsMember, Remote: e.UploadsRemote, Expected: len(e.Uploads), Unchecked: e.UploadsOther}
	r.Assets = []*AssetCheck{lfs, uploads}

	progress(lfs.Archive)
	r.scanArchive(cur.Src, lfs, strings.Contains(skipped, "lfs"), func(name string) string {
		if rel, ok := backup.KeepLFS(name); ok {
			return backup.LFSOIDOf(rel)
		}
		return ""
	}, func(have map[string]bool) []string {
		var miss []string
		for oid := range e.LFS {
			if !have[oid] {
				miss = append(miss, oid)
			}
		}
		return miss
	})
	progress(uploads.Archive)
	want := map[string]bool{}
	for _, u := range e.Uploads {
		want[u] = true
	}
	r.scanArchive(cur.Src, uploads, strings.Contains(skipped, "uploads"), func(name string) string {
		if want[name] {
			return name
		}
		return ""
	}, func(have map[string]bool) []string {
		var miss []string
		for u := range want {
			if !have[u] {
				miss = append(miss, u)
			}
		}
		return miss
	})
}

// scanArchive lists the archive, keeping the names key maps to something,
// and records what missing() reports.
func (r *Report) scanArchive(src backup.Source, c *AssetCheck, skipped bool, key func(string) string, missing func(map[string]bool) []string) {
	kind := strings.TrimSuffix(c.Archive, ".tar.gz") // lfs | uploads
	c.Skipped = skipped
	c.Present = src.Has(c.Archive)
	if !c.Present {
		switch {
		case c.Expected == 0:
		case skipped:
			r.add(Warning, kind+"_skipped", "", "%s was skipped when the backup was made (%d files on disk in GitLab are not in it)", c.Archive, c.Expected)
		default:
			r.add(Error, kind+"_archive_missing", "", "the backup has no %s but the database lists %d files stored on disk", c.Archive, c.Expected)
		}
		c.Missing = c.Expected
		return
	}
	have := map[string]bool{}
	files := 0
	if err := listArchive(src, c.Archive, func(name string) {
		files++
		if k := key(name); k != "" {
			have[k] = true
		}
	}); err != nil {
		c.Error = err.Error()
		r.add(Error, kind+"_archive_unreadable", "", "%s could not be read: %v", c.Archive, err)
		return
	}
	if c.Expected == 0 && files > 0 {
		// A check that compares with nothing is no check: say so.
		r.add(Warning, kind+"_unchecked", "", "%s holds %d files but the database lists none to compare with (unknown schema?)", c.Archive, files)
	}
	miss := missing(have)
	sort.Strings(miss)
	c.Found, c.Missing = c.Expected-len(miss), len(miss)
	if len(miss) > missingSample {
		c.Sample = miss[:missingSample]
	} else {
		c.Sample = miss
	}
	if c.Missing > 0 {
		r.add(Error, kind+"_missing", "", "%d of %d files listed in the database are not in %s (first: %s)",
			c.Missing, c.Expected, c.Archive, miss[0])
	}
}

// listArchive calls fn with the name of every regular file of a gzipped
// tar member of the backup.
func listArchive(src backup.Source, member string, fn func(name string)) error {
	rc, err := src.Open(member)
	if err != nil {
		return err
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			fn(strings.TrimPrefix(h.Name, "./"))
		}
	}
}
