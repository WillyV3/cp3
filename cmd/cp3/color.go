package main

import "os"

// 16-color ANSI so output adapts to the terminal's theme instead of fighting
// it. NO_COLOR (https://no-color.org) disables everywhere. Tables color only
// on a TTY; the statusline forces color because its consumer (Claude's
// statusline renderer) reads ANSI from a pipe.
const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
)

func noColor() bool {
	_, set := os.LookupEnv("NO_COLOR")
	return set
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// paint wraps s in an ANSI code when on is true.
func paint(on bool, code, s string) string {
	if !on || noColor() {
		return s
	}
	return code + s + cReset
}
