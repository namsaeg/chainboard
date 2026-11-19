package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func interactive() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}

func echoOff() func() {
	h := windows.Handle(os.Stdin.Fd())
	var old uint32
	if windows.GetConsoleMode(h, &old) != nil {
		return func() {}
	}
	quiet := old&^windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT
	if windows.SetConsoleMode(h, quiet) != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(h, old) }
}
