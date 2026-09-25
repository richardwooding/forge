package cli

import (
	"io"
	"os"
)

// isTerminal reports whether w is an interactive terminal.
//
// forge's surfaces get piped constantly -- into jq, into a file, into another
// tool -- so the difference decides whether output is decorated for a person or
// left exactly as the tool produced it.
func isTerminal(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
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
