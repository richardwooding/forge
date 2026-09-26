package cli

import (
	"io"
	"os"
)

// isTTY reports whether w is attached to a terminal device.
//
// Kept apart from isTerminal because the two answer different questions.
// Asking "is this a terminal" to decide whether to use colour has to honour
// NO_COLOR; asking it to decide whether an interactive shell can run must not,
// or `NO_COLOR=1 forge repl` refuses to start with "needs a terminal" -- which
// is untrue, and sends the reader looking for a problem with their terminal.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// isTerminal reports whether w is an interactive terminal that should be
// written to in colour.
//
// forge's surfaces get piped constantly -- into jq, into a file, into another
// tool -- so the difference decides whether output is decorated for a person or
// left exactly as the tool produced it.
func isTerminal(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTTY(w)
}
