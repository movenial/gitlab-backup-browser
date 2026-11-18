package backup

import (
	"bufio"
	"fmt"
	"io"

	"github.com/movenial/gitlab-backup-browser/internal/gitstore"
)

// ChainStart returns the index of the step to start applying from: the
// last step whose bundle has no prerequisites (a full backup). Earlier
// steps are superseded. Only bundle headers are read.
func ChainStart(src Source, rb *RepoBackup) (int, error) {
	start := 0
	for i, s := range rb.Steps {
		h, err := ReadHeader(src, s.Bundle)
		if err != nil {
			return 0, err
		}
		if len(h.Prereqs) == 0 {
			start = i
		}
	}
	return start, nil
}

// ReadHeader reads just the header of a bundle member.
func ReadHeader(src Source, member string) (*gitstore.BundleHeader, error) {
	rc, err := src.Open(member)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	h, err := gitstore.ReadBundleHeader(bufio.NewReaderSize(rc, 4096))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", member, err)
	}
	return h, nil
}

// BuildRepo materialises rb as a bare repository at dst.
func BuildRepo(src Source, rb *RepoBackup, dst string) (*gitstore.BuildStats, error) {
	return BuildRepoProgress(src, rb, dst, nil)
}

// BuildRepoProgress is BuildRepo with a progress callback.
func BuildRepoProgress(src Source, rb *RepoBackup, dst string, prog func(gitstore.Progress)) (*gitstore.BuildStats, error) {
	if len(rb.Steps) == 0 {
		return nil, fmt.Errorf("backup: %s has no bundles", rb.Key)
	}
	start, err := ChainStart(src, rb)
	if err != nil {
		return nil, err
	}
	steps := rb.Steps[start:]
	var inputs []gitstore.BundleInput
	for _, s := range steps {
		member := s.Bundle
		inputs = append(inputs, gitstore.BundleInput{
			Name: member,
			Open: func() (io.ReadCloser, error) { return src.Open(member) },
		})
	}
	return gitstore.BuildFromBundlesProgress(dst, inputs, finalRefs(src, steps[len(steps)-1]), prog)
}

// PlannedRefs returns the refs a repository will have once restored, and
// HEAD's branch, read from bundle headers and .refs files only (no objects
// are unpacked). Refs whose objects turn out to be missing are dropped by
// the build, so the restored repository may have fewer.
func PlannedRefs(src Source, rb *RepoBackup) ([]gitstore.Ref, string, error) {
	if len(rb.Steps) == 0 {
		return nil, "", fmt.Errorf("backup: %s has no bundles", rb.Key)
	}
	start, err := ChainStart(src, rb)
	if err != nil {
		return nil, "", err
	}
	steps := rb.Steps[start:]
	var headers [][]gitstore.Ref
	for _, s := range steps {
		h, err := ReadHeader(src, s.Bundle)
		if err != nil {
			return nil, "", err
		}
		headers = append(headers, h.Refs)
	}
	refs, head := gitstore.PlanRefs(headers, finalRefs(src, steps[len(steps)-1]))
	return refs, head, nil
}

// finalRefs reads a Gitaly .refs file, if the step has a readable one.
func finalRefs(src Source, s Step) *gitstore.RefSet {
	if s.Refs == "" {
		return nil
	}
	rc, err := src.Open(s.Refs)
	if err != nil {
		return nil
	}
	defer rc.Close()
	rs, err := gitstore.ParseRefsFile(rc)
	if err != nil {
		return nil
	}
	return rs
}

// Refs is PlannedRefs as a name -> hex id map.
func Refs(src Source, rb *RepoBackup) (map[string]string, error) {
	list, _, err := PlannedRefs(src, rb)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string, len(list))
	for _, r := range list {
		refs[r.Name] = r.Hash.String()
	}
	return refs, nil
}
