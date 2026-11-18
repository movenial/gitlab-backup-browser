//go:build !linux

package main

// returnMemoryPromptly: elsewhere the runtime already returns memory
// promptly enough (see godebug_linux.go).
func returnMemoryPromptly() {}
