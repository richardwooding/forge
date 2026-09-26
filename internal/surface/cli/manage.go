package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/buildinfo"
	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/ui"
)

func (a *App) addManageCommands(root *cobra.Command) {
	root.AddCommand(
		a.cmdTool(),
		a.cmdLs(),
		a.cmdInfo(),
		a.cmdRun(),
		a.cmdView(),
		a.cmdLabel(),
		a.cmdGrant(),
		a.cmdSecret(),
		a.cmdSkill(),
		a.cmdMCP(),
		a.cmdServe(),
		a.cmdREPL(),
		a.cmdDoctor(),
		a.cmdVersion(),
		a.cmdExport(),
		a.cmdImport(),
		a.cmdPush(),
		a.cmdPull(),
	)
}

func (a *App) cmdTool() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tool",
		Short:   "Install and remove tools",
		GroupID: "manage",
	}

	add := &cobra.Command{
		Use:   "add <path>",
		Short: "Build a Go program and install it as a tool",
		Long: "Compiles the Go source at <path> to WebAssembly and installs it.\n\n" +
			"The source is copied before it is built, so your go.mod and go.sum are\n" +
			"never modified. Note that compiling runs the Go toolchain over that\n" +
			"source on this machine: forge's sandbox protects a tool when it runs,\n" +
			"not when it is built, so add tools whose source you would have been\n" +
			"willing to build anyway.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			res, err := a.tk.Add(c.Context(), args[0])
			if err != nil {
				return err
			}
			th := ui.ForWriter(c.OutOrStdout())
			out := c.OutOrStdout()
			t := res.Timings
			rec := res.Record

			fmt.Fprintln(out, th.Step("compile", "Go → wasip1 reactor", round(t.Build)))
			fmt.Fprintln(out, th.Step("load", "wasm module", round(t.Compile)))
			fmt.Fprintln(out, th.Step("describe", "with no capabilities", round(t.Describe)))
			fmt.Fprintln(out, th.Step("store", "content addressed", round(t.Store)))

			verb := "installed"
			if res.Replaced {
				verb = "updated"
			}
			name := th.Name.Render(rec.Spec.Name)
			if v := rec.Spec.Version; v != "" {
				name += " " + th.Subtle.Render(v)
			}
			fmt.Fprintf(out, "\n%s %s  %s\n", verb, name, th.Chips(rec.Labels()))

			if reqs := rec.Spec.Requires; len(reqs) > 0 {
				fmt.Fprintf(out, "\n%s\n", th.Warn("%s wants:", rec.Spec.Name))
				for _, req := range reqs {
					fmt.Fprintf(out, "    %s %s  %s\n", req.Kind.Badge(),
						th.Bold.Render(string(req.Kind)), th.Subtle.Render(strings.Join(req.Scope, ", ")))
					if req.Reason != "" {
						fmt.Fprintf(out, "      %s\n", th.Subtle.Render(req.Reason))
					}
				}
				fmt.Fprintf(out, "  %s\n", th.Subtle.Render("you will be asked before it uses these"))
			}
			return nil
		},
	}

	remove := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Uninstall a tool",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.tk.Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %s\n", args[0])
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}

	gc := &cobra.Command{
		Use:   "gc",
		Short: "Delete stored modules no tool refers to",
		RunE: func(c *cobra.Command, args []string) error {
			n, freed, err := a.tk.Store().GC()
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %d module(s), freed %s\n", n, bytesHuman(freed))
			return nil
		},
	}

	cmd.AddCommand(add, remove, gc)
	return cmd
}

func (a *App) cmdLs() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List installed tools",
		GroupID: "manage",
		RunE: func(c *cobra.Command, args []string) error {
			records, err := a.tk.List(a.selector)
			if err != nil {
				return err
			}
			if asJSON {
				return jsonOut(c.OutOrStdout(), records)
			}
			if len(records) == 0 {
				if a.selector.String() != "*" {
					fmt.Fprintf(c.OutOrStdout(), "no tools match %s\n", a.selector)
					return nil
				}
				fmt.Fprintln(c.OutOrStdout(), "no tools installed yet — try: forge tool add ./mytool")
				return nil
			}

			th := ui.ForWriter(c.OutOrStdout())
			tbl := th.NewTable("NAME", "LABELS", "WANTS", "SUMMARY")
			for _, r := range records {
				tbl.Row(
					th.Name.Render(r.Spec.Name),
					th.Chips(r.Labels()),
					badges(r),
					th.Subtle.Render(r.Spec.Summary))
			}
			tbl.Render(c.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func (a *App) cmdInfo() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "info <name>",
		Short:   "Show everything about one tool",
		GroupID: "manage",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			rec, err := a.tk.Get(args[0])
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			th := ui.ForWriter(out)
			fmt.Fprintf(out, "%s", th.Name.Render(rec.Spec.Name))
			if rec.Spec.Version != "" {
				fmt.Fprintf(out, " %s", th.Subtle.Render(rec.Spec.Version))
			}
			fmt.Fprintln(out)
			if rec.Spec.Summary != "" {
				fmt.Fprintf(out, "  %s\n", rec.Spec.Summary)
			}
			fmt.Fprintf(out, "\n  labels    %s\n", th.Chips(rec.Labels()))
			fmt.Fprintf(out, "  module    %s\n", rec.WasmDigest[:16])
			fmt.Fprintf(out, "  added     %s\n", rec.Added.Local().Format("2006-01-02 15:04"))
			if rec.Source != "" {
				fmt.Fprintf(out, "  source    %s\n", rec.Source)
			}
			if rec.Build.GoVersion != "" {
				fmt.Fprintf(out, "  built by  %s\n", rec.Build.GoVersion)
			}

			if len(rec.Spec.Requires) > 0 {
				fmt.Fprintf(out, "\n  wants:\n")
				for _, req := range rec.Spec.Requires {
					fmt.Fprintf(out, "    %s %-12s %s\n", req.Kind.Badge(), req.Kind, strings.Join(req.Scope, ", "))
					if req.Reason != "" {
						fmt.Fprintf(out, "      %s\n", req.Reason)
					}
				}
				granted := rec.GrantSet()
				if len(granted.Kinds()) == 0 {
					fmt.Fprintf(out, "    (nothing granted yet; you will be asked on first use)\n")
				}
			}

			fmt.Fprintf(out, "\n  operations:\n")
			for _, op := range rec.Spec.Ops {
				fmt.Fprintf(out, "    %s", op.Name)
				if op.Summary != "" {
					fmt.Fprintf(out, " — %s", op.Summary)
				}
				fmt.Fprintf(out, "  (%s)\n", op.OutputKind)

				b, err := a.tk.Bound(rec.Spec.Name, op.Name)
				if err != nil {
					continue
				}
				for _, f := range b.Flags.Flags {
					req := ""
					if f.Required {
						req = " (required)"
					}
					fmt.Fprintf(out, "      --%-20s %s%s\n", f.Name+" "+TypeName(f.Kind, f.Repeatable), f.Usage, req)
				}
				for _, ne := range b.Flags.NotExpressible {
					fmt.Fprintf(out, "      %s needs --set-json: %s\n", ne.Pointer, ne.Reason)
				}
			}
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}
	return cmd
}

func (a *App) cmdRun() *cobra.Command {
	var inputJSON string
	var op string
	cmd := &cobra.Command{
		Use:     "run <tool> [op]",
		Short:   "Run a tool, including one outside the current view",
		GroupID: "manage",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			if len(args) > 1 {
				op = args[1]
			}
			var input []byte
			if inputJSON != "" {
				raw, err := readDocument(c, inputJSON)
				if err != nil {
					return err
				}
				input = raw
			}
			return a.runTool(c, name, op, input)
		},
		ValidArgsFunction: a.completeToolNames,
	}
	cmd.Flags().StringVar(&inputJSON, "input-json", "", "read the input document from a file, or - for stdin")
	return cmd
}

func (a *App) cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:     "doctor",
		Short:   "Check that forge can build and run tools",
		GroupID: "manage",
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			th := ui.ForWriter(out)
			fmt.Fprintf(out, "  %s\n", th.OK("forge            %s", buildinfo.Version()))
			if v, ok := a.tk.GoAvailable(c.Context()); ok {
				fmt.Fprintf(out, "  %s\n", th.OK("go toolchain    %s", v))
			} else {
				fmt.Fprintf(out, "  %s\n", th.Warn("go toolchain    not found — forge can still run installed tools, but not build new ones"))
			}
			records, err := a.tk.List(labels.All)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  %s\n", th.OK("tools installed %d", len(records)))

			var broken int
			for _, r := range records {
				if _, err := a.tk.Store().Blob(r.WasmDigest); err != nil {
					fmt.Fprintf(out, "  %s\n", th.Fail("%s: %v", r.Spec.Name, err))
					broken++
				}
			}
			if broken == 0 && len(records) > 0 {
				fmt.Fprintf(out, "  %s\n", th.OK("modules         all present and verified"))
			}
			return nil
		},
	}
}

func (a *App) completeToolNames(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	records, err := a.tk.List(labels.All)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, r := range records {
		if strings.HasPrefix(r.Spec.Name, prefix) {
			names = append(names, r.Spec.Name+"\t"+r.Spec.Summary)
		}
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

// badges renders a tool's requested capabilities compactly.
func badges(r store.Record) string {
	var b strings.Builder
	for _, req := range r.Spec.Requires {
		b.WriteString(req.Kind.Badge())
	}
	return b.String()
}

func describeRequests(reqs []capability.Request) string {
	var parts []string
	for _, r := range reqs {
		parts = append(parts, fmt.Sprintf("%s %s(%s)", r.Kind.Badge(), r.Kind, strings.Join(r.Scope, ",")))
	}
	return strings.Join(parts, " ")
}

// round shortens a duration for display: a build that took 1.234567s should
// read as 1.2s.
func round(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func bytesHuman(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
