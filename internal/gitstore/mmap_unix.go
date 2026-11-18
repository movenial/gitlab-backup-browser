//go:build unix

package gitstore

import (
	"os"
	"syscall"
)

// mapFile maps a file read-only. Mapped pages live in the OS page cache:
// they are not Go heap and the kernel can drop them under memory pressure.
func mapFile(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.Size() == 0 {
		return nil, func() error { return nil }, nil
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		// Some filesystems (e.g. Android FUSE storage) refuse mmap.
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, nil, rerr
		}
		return data, func() error { return nil }, nil
	}
	return b, func() error { return syscall.Munmap(b) }, nil
}
