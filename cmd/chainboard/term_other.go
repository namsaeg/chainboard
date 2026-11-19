//go:build !linux && !windows

package main

// Elsewhere the password is read with echo on; prefer -password-file.
func interactive() bool { return true }

func echoOff() func() { return func() {} }
