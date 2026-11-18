//go:build !unix

package app

func diskFree(path string) (uint64, bool) { return 0, false }
