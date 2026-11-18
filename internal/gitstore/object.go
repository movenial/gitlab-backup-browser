// Package gitstore is a small, dependency-free, read-mostly git object
// store. It can turn git bundles into indexed packfiles (like
// `git index-pack --fix-thin`), read objects back, parse commits, trees
// and tags, and write the files needed to serve a repository over git's
// "dumb" HTTP protocol.
//
// Only SHA-1 repositories are supported.
package gitstore

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Hash is a SHA-1 object id.
type Hash [20]byte

// ZeroHash is the all-zero id.
var ZeroHash Hash

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// Short returns the first 8 hex digits.
func (h Hash) Short() string { return h.String()[:8] }

// IsZero reports whether h is the zero id.
func (h Hash) IsZero() bool { return h == ZeroHash }

// ParseHash parses a 40-character hex id.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 40 {
		return h, fmt.Errorf("gitstore: bad object id %q", s)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("gitstore: bad object id %q", s)
	}
	return h, nil
}

// ObjType is a git object type as encoded in packfiles.
type ObjType int8

const (
	ObjCommit   ObjType = 1
	ObjTree     ObjType = 2
	ObjBlob     ObjType = 3
	ObjTag      ObjType = 4
	ObjOfsDelta ObjType = 6
	ObjRefDelta ObjType = 7
)

func (t ObjType) String() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	case ObjOfsDelta:
		return "ofs-delta"
	case ObjRefDelta:
		return "ref-delta"
	}
	return "unknown"
}

// hashObject computes the git object id of data with the given type.
func hashObject(t ObjType, data []byte) Hash {
	h := sha1.New()
	h.Write(objHeader(t, int64(len(data))))
	h.Write(data)
	var out Hash
	h.Sum(out[:0])
	return out
}

func objHeader(t ObjType, size int64) []byte {
	b := make([]byte, 0, 32)
	b = append(b, t.String()...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, size, 10)
	b = append(b, 0)
	return b
}
