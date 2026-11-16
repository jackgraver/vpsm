package cli

import (
	"os"
)

// isTerminal reports whether f is a character device, i.e. an interactive
// terminal rather than a pipe or file.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// useColor decides whether to emit ANSI styling. Colour is for humans at a
// terminal: piping to a file, a pager, or another program gets plain text,
// and NO_COLOR is honoured either way (see no-color.org).
func useColor(f *os.File) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(f)
}
