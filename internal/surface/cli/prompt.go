package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/richardwooding/forge/internal/policy"
)

// TerminalPrompter asks about capabilities on a terminal.
//
// It refuses rather than asking when there is no terminal to ask on. A prompt
// written to a pipe is not a question, it is a hang: the reader is waiting for
// output and forge is waiting for an answer, and neither will move.
type TerminalPrompter struct {
	In  io.Reader
	Out io.Writer
}

// Ask implements policy.Prompter.
func (p TerminalPrompter) Ask(ctx context.Context, prompt policy.Prompt) (policy.Answer, error) {
	in, out := p.In, p.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stderr
	}
	if !isTerminal(out) || !readableTerminal(in) {
		return policy.Deny, nil
	}

	fmt.Fprintf(out, "\n%s wants to:\n", prompt.Tool)
	for _, req := range prompt.Requests {
		fmt.Fprintf(out, "  %s %s  %s\n", req.Kind.Badge(), req.Kind, strings.Join(req.Scope, ", "))
		if req.Reason != "" {
			fmt.Fprintf(out, "      %s\n", req.Reason)
		}
	}
	// The combination is the risk, so it is named rather than left to be
	// noticed: a tool holding both a secret and the network can send one to
	// the other, and that is not obvious from two separate lines.
	if warning := combinationWarning(prompt); warning != "" {
		fmt.Fprintf(out, "\n  ! %s\n", warning)
	}

	fmt.Fprintf(out, "\n  [a] allow always   [o] allow once   [d] deny (default)\n  > ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return policy.Deny, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "a", "allow", "always":
		return policy.AllowAlways, nil
	case "o", "once":
		return policy.AllowOnce, nil
	default:
		return policy.Deny, nil
	}
}

// combinationWarning names a pairing that is more dangerous than its parts.
func combinationWarning(prompt policy.Prompt) string {
	var hasSecret, hasNet, hasWrite bool
	for _, r := range prompt.Requests {
		switch r.Kind {
		case "secret":
			hasSecret = true
		case "net.http":
			hasNet = true
		case "fs.write":
			hasWrite = true
		}
	}
	switch {
	case hasSecret && hasNet:
		return "this tool could read your secrets and send them anywhere it is allowed to reach"
	case hasWrite && hasNet:
		return "this tool could write files from whatever it downloads"
	}
	return ""
}

// readableTerminal reports whether r is a terminal forge can read an answer
// from. Reading a piped stdin would consume input meant for the tool.
func readableTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
