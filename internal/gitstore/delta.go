package gitstore

import (
	"crypto/sha1"
	"errors"
)

var errBadDelta = errors.New("gitstore: corrupt delta")

// applyDelta applies a git binary delta to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	dstSize, ops, err := deltaHeader(base, delta)
	if err != nil {
		return nil, err
	}
	// Check the ops really produce dstSize bytes before allocating them:
	// the size is read from the delta and cannot be trusted.
	if err := deltaOps(base, ops, dstSize, func([]byte) {}); err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	deltaOps(base, ops, dstSize, func(b []byte) { out = append(out, b...) })
	return out, nil
}

// deltaHeader reads the source and result sizes at the start of delta,
// checks the source size against base and returns the ops that follow.
func deltaHeader(base, delta []byte) (dstSize uint64, ops []byte, err error) {
	srcSize, n := deltaVarint(delta)
	if n <= 0 || srcSize != uint64(len(base)) {
		return 0, nil, errBadDelta
	}
	delta = delta[n:]
	dstSize, n = deltaVarint(delta)
	if n <= 0 || dstSize > maxObjectSize {
		return 0, nil, errBadDelta
	}
	return dstSize, delta[n:], nil
}

// maxObjectSize is the largest object a slice can hold.
const maxObjectSize = uint64(^uint(0) >> 1)

// deltaOps runs the copy and insert ops of a delta, passing each chunk of
// the result to emit, and checks they produce exactly dstSize bytes.
func deltaOps(base, ops []byte, dstSize uint64, emit func([]byte)) error {
	var written uint64
	for len(ops) > 0 {
		op := ops[0]
		ops = ops[1:]
		var chunk []byte
		switch {
		case op&0x80 != 0:
			// Bits 0-3 say which offset bytes follow, bits 4-6 which size bytes.
			off, ok1 := sparseLE(&ops, op, 4)
			size, ok2 := sparseLE(&ops, op>>4, 3)
			if !ok1 || !ok2 {
				return errBadDelta
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > uint64(len(base)) {
				return errBadDelta
			}
			chunk = base[off : off+size]
		case op != 0:
			if int(op) > len(ops) {
				return errBadDelta
			}
			chunk = ops[:op]
			ops = ops[op:]
		default:
			return errBadDelta
		}
		written += uint64(len(chunk))
		if written > dstSize {
			return errBadDelta
		}
		emit(chunk)
	}
	if written != dstSize {
		return errBadDelta
	}
	return nil
}

// deltaVarint reads the little-endian base-128 size used in delta headers.
func deltaVarint(b []byte) (uint64, int) {
	var v uint64
	var shift uint
	for i, c := range b {
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
		if shift > 63 {
			return 0, -1
		}
	}
	return 0, -1
}

// hashDelta returns the object id of the result of applying delta to base,
// without materialising the result: copied and inserted bytes go straight
// into SHA-1.
func hashDelta(t ObjType, base, delta []byte) (Hash, error) {
	dstSize, ops, err := deltaHeader(base, delta)
	if err != nil {
		return ZeroHash, err
	}
	h := sha1.New()
	h.Write(objHeader(t, int64(dstSize)))
	if err := deltaOps(base, ops, dstSize, func(b []byte) { h.Write(b) }); err != nil {
		return ZeroHash, err
	}
	var out Hash
	h.Sum(out[:0])
	return out, nil
}

// sparseLE reads the little-endian number of a delta copy op: for each of
// the n low bits set in mask, one byte follows (missing bytes are 0).
func sparseLE(ops *[]byte, mask byte, n uint) (uint64, bool) {
	var v uint64
	for i := uint(0); i < n; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		if len(*ops) == 0 {
			return 0, false
		}
		v |= uint64((*ops)[0]) << (8 * i)
		*ops = (*ops)[1:]
	}
	return v, true
}
