package main

import (
	"os"
	"strings"
	"syscall"
)

// returnMemoryPromptly makes the Go runtime give freed memory back with
// MADV_DONTNEED instead of MADV_FREE. With MADV_FREE (the default on
// Linux and Android) the kernel takes the pages back only under memory
// pressure, so after a large download the process keeps showing hundreds
// of MB it no longer uses; on a phone that gets it killed.
//
// The setting is read when the runtime starts and cannot be set with
// //go:debug, so gbb re-executes itself once with it in GODEBUG (unless
// the user already chose a value).
func returnMemoryPromptly() {
	cur := os.Getenv("GODEBUG")
	if strings.Contains(cur, "madvdontneed=") {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	val := "madvdontneed=1"
	if cur != "" {
		val = cur + "," + val
	}
	env := []string{"GODEBUG=" + val}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GODEBUG=") {
			env = append(env, kv)
		}
	}
	syscall.Exec(exe, os.Args, env) // only returns on failure: carry on without it
}
