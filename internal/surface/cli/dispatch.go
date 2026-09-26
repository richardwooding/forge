package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/labels"
)

// Dispatcher runs command lines against a freshly built command tree.
//
// This is what makes the REPL a front end rather than a second implementation.
// A line typed at the prompt becomes argv and goes through the same cobra tree
// the CLI builds, with the same flag parsing, the same schema binding and the
// same error rendering -- so there is nothing for the two to disagree about.
type Dispatcher struct {
	opts Options
}

// NewDispatcher returns a dispatcher over the same options the CLI uses.
func NewDispatcher(opts Options) *Dispatcher { return &Dispatcher{opts: opts} }

// Dispatch runs one line and returns what it wrote.
//
// The tree is rebuilt per line rather than reused. It is only struct
// construction, and it means a tool installed while the shell is open is
// callable on the next line -- which is the same property the MCP surface has
// and would be odd to lack here.
func (d *Dispatcher) Dispatch(ctx context.Context, args []string) (stdout, stderr string, err error) {
	return d.DispatchInput(ctx, args, nil)
}

// DispatchInput is Dispatch with something on standard input.
//
// It exists for callers that hand a tool a whole document rather than flags --
// `forge run <tool> --input-json -` -- which is the same path a shell pipeline
// takes, and the one the conformance harness uses so that the CLI is compared
// on the shared code rather than on its own flag encoder.
func (d *Dispatcher) DispatchInput(ctx context.Context, args []string, stdin []byte) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer

	root, err := New(Options{
		Toolkit:  d.opts.Toolkit,
		Stdout:   &out,
		Stderr:   &errBuf,
		Selector: d.opts.Selector,
		ViewName: d.opts.ViewName,
	})
	if err != nil {
		return "", "", err
	}
	if stdin != nil {
		root.SetIn(bytes.NewReader(stdin))
		args = append(append([]string(nil), args...), "--input-json", "-")
	}
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	// The shell prints its own errors, and cobra's usage dump on every typo
	// would bury the line that matters.
	root.SilenceUsage = true
	root.SilenceErrors = true

	runErr := root.ExecuteContext(ctx)
	return out.String(), errBuf.String(), renderError(runErr)
}

// ErrToolFailed reports that the tool ran and reported failure, as opposed to
// forge being unable to run it.
//
// The distinction has to survive this far. A caller that cannot tell the two
// apart -- the REPL deciding how to render a line, a harness deciding whether
// the surfaces agree -- would treat a tool saying no as a success, which is
// the one row of the parity table that is easiest to get wrong because it
// looks like an error and is not one.
var ErrToolFailed = errors.New("the tool reported a failure")

// renderError turns a command error into the one line the shell should show,
// through the same fault rendering the CLI uses.
func renderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errToolFailed) {
		// The tool has already written its own message to stderr; what is
		// added here is the classification, not the words.
		return ErrToolFailed
	}
	var b bytes.Buffer
	Render(&b, err)
	msg := strings.TrimRight(b.String(), "\n")
	if msg == "" {
		return nil
	}
	return errorString(msg)
}

type errorString string

func (e errorString) Error() string { return string(e) }

// Complete offers completions for a partial word, using cobra's own
// completion machinery so the shell has no second list of tools to keep
// current.
func (d *Dispatcher) Complete(ctx context.Context, args []string, partial string) []string {
	root, err := New(Options{
		Toolkit:  d.opts.Toolkit,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
		Selector: d.opts.Selector,
		ViewName: d.opts.ViewName,
	})
	if err != nil {
		return nil
	}

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	// __complete is how a shell asks a cobra program what comes next; asking
	// the same way means flag names, enum values and tool names all arrive
	// without forge computing any of them twice.
	root.SetArgs(append([]string{cobra.ShellCompRequestCmd}, append(args, partial)...))
	if err := root.ExecuteContext(ctx); err != nil {
		return nil
	}

	var options []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		line = strings.TrimSpace(line)
		// cobra ends its output with a :<directive> line, and annotates each
		// candidate with a tab and a description.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			line = line[:i]
		}
		options = append(options, line)
	}
	return options
}

// Rebuild changes the view the next line will be dispatched against.
func (d *Dispatcher) Rebuild(sel labels.Selector, viewName string) error {
	d.opts.Selector, d.opts.ViewName = sel, viewName
	return nil
}
