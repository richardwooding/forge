// Package cli is forge's command-line surface.
//
// The command tree is built at startup from whatever is installed, so a tool
// added a moment ago is a subcommand now, with flags derived from its schema
// and completion derived from its enums. Nothing is generated and nothing is
// restarted.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// App holds what the commands need.
type App struct {
	tk  *toolkit.Toolkit
	out io.Writer
	err io.Writer

	// selector is the active view, resolved by the pre-scan before the tree
	// was built.
	selector labels.Selector
	viewName string

	quiet   bool
	verbose bool
}

// Options configure the surface.
type Options struct {
	Toolkit  *toolkit.Toolkit
	Stdout   io.Writer
	Stderr   io.Writer
	Selector labels.Selector
	ViewName string
}

// New builds the root command.
func New(opts Options) (*cobra.Command, error) {
	a := &App{
		tk:       opts.Toolkit,
		out:      or(opts.Stdout, os.Stdout),
		err:      or(opts.Stderr, os.Stderr),
		selector: opts.Selector,
		viewName: opts.ViewName,
	}
	if a.selector == nil {
		a.selector = labels.All
	}

	root := &cobra.Command{
		Use:   "forge",
		Short: "A multitool that builds itself",
		Long: "forge compiles Go programs to WebAssembly and exposes each one as a\n" +
			"command, sandboxed by the capabilities it declares and filtered by the\n" +
			"labels you give it.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without these two, cobra answers before forge can. ArbitraryArgs lets
		// an unmatched command reach RunE, and the whitelist stops the root
		// rejecting the tool's own flags -- `forge hello --name x` for a tool
		// that is merely out of view would otherwise fail with "unknown flag:
		// --name", which says nothing about the real problem.
		//
		// This is scoped to the root. A real subcommand still validates its own
		// flags, so a typo in `forge ls --jsn` is still caught.
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
	}
	root.SetOut(a.out)
	root.SetErr(a.err)

	root.PersistentFlags().String("view", "", "use a saved view")
	root.PersistentFlags().String("labels", "", "filter tools by a label selector, e.g. 'git && !slow'")
	root.PersistentFlags().BoolVar(&a.quiet, "quiet", false, "suppress tool logs and progress")
	root.PersistentFlags().BoolVarP(&a.verbose, "verbose", "v", false, "show tool logs")

	root.AddGroup(
		&cobra.Group{ID: "manage", Title: "Managing tools:"},
		&cobra.Group{ID: "tools", Title: "Installed tools:"},
	)

	a.addManageCommands(root)

	records, err := a.tk.List(a.selector)
	if err != nil {
		return nil, err
	}
	a.addToolCommands(root, records)

	// A tool that exists but is out of view would otherwise fail as an unknown
	// command, which reads as "it is not installed".
	root.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return c.Help()
		}
		return a.unknownCommand(args[0])
	}
	return root, nil
}

func or(w io.Writer, fallback io.Writer) io.Writer {
	if w == nil {
		return fallback
	}
	return w
}

// unknownCommand explains an out-of-view tool rather than letting it read as
// missing.
//
// The CLI is the one surface that says this. On MCP, REST and gRPC an
// out-of-view tool answers not-found precisely so that a narrow view does not
// leak what it hides; here the caller is a local human who owns the machine and
// benefits from being told how to widen it.
func (a *App) unknownCommand(name string) error {
	all, err := a.tk.List(labels.All)
	if err == nil {
		for _, rec := range all {
			if rec.Spec.Name != name {
				continue
			}
			where := "the current view"
			if a.viewName != "" {
				where = fmt.Sprintf("view %q", a.viewName)
			}
			return fmt.Errorf("tool %q is installed but not in %s (labels: %v)\n"+
				"  run it anyway:  forge run %s\n"+
				"  see everything: forge --labels '*' ls",
				name, where, rec.Labels(), name)
		}
	}
	return fmt.Errorf("unknown command %q; run 'forge ls' to see the installed tools", name)
}

// writeResult renders a result to the terminal or to a pipe.
func (a *App) writeResult(cmd *cobra.Command, res *toolkit.Result) error {
	out := cmd.OutOrStdout()

	if len(res.Stderr) > 0 && !a.quiet {
		fmt.Fprint(cmd.ErrOrStderr(), string(res.Stderr))
	}
	if len(res.Stdout) > 0 {
		fmt.Fprint(out, string(res.Stdout))
	}

	if res.Rendition.ToolError {
		fmt.Fprintln(cmd.ErrOrStderr(), res.Rendition.Message)
		// The tool ran and reported failure, which is exit 1 -- distinct from
		// forge being unable to run it at all.
		return errToolFailed
	}

	data, printable := res.Rendition.Raw()
	if len(data) == 0 {
		return nil
	}
	if !printable && isTerminal(out) {
		// Writing arbitrary bytes to a terminal can leave it needing a reset.
		fmt.Fprintln(out, res.Rendition.String())
		return nil
	}
	if printable && isTerminal(out) {
		fmt.Fprintln(out, res.Rendition.String())
		return nil
	}
	_, err := out.Write(data)
	return err
}

// errToolFailed marks a tool that ran and reported failure.
var errToolFailed = errors.New("tool reported failure")

func (a *App) logSink(cmd *cobra.Command) func(hostabi.Level, string, string) {
	if a.quiet {
		return nil
	}
	return func(level hostabi.Level, tool, msg string) {
		if level == hostabi.LevelDebug && !a.verbose {
			return
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%s %s: %s\n", level, tool, msg)
	}
}

func (a *App) progressSink(cmd *cobra.Command) func(int64, int64, string) {
	if a.quiet || !isTerminal(cmd.ErrOrStderr()) {
		return nil
	}
	return func(done, total int64, msg string) {
		if total > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "\r  %d/%d %s\033[K", done, total, msg)
			if done >= total {
				fmt.Fprintln(cmd.ErrOrStderr())
			}
			return
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\r  %s\033[K", msg)
	}
}

// ExitCode maps an error to a process status through binding's parity table, so
// the CLI cannot invent its own classification.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errToolFailed):
		return 1
	case errors.Is(err, context.Canceled):
		return 130
	}
	if f, ok := binding.AsFault(err); ok {
		return f.Code.ExitCode()
	}
	// An error forge did not classify is a usage or setup problem.
	return 2
}

// Render writes an error the way the CLI should, including violations when the
// fault carries them.
func Render(w io.Writer, err error) {
	if err == nil || errors.Is(err, errToolFailed) {
		return
	}
	f, ok := binding.AsFault(err)
	if !ok {
		fmt.Fprintln(w, "forge:", err)
		return
	}
	fmt.Fprintf(w, "forge: %s\n", f.Message)
	// With one violation the message is already built from it, so listing it
	// again just says the same thing twice. With several, the message only
	// counts them and the list is where the detail is.
	if len(f.Violations) > 1 {
		for _, v := range f.Violations {
			fmt.Fprintf(w, "  %s: %s\n", v.Pointer, v.Message)
		}
	}
}

// jsonOut writes a value as indented JSON, used by the management commands
// under --output json.
func jsonOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
