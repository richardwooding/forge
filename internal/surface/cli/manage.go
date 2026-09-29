package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/build"
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
		Long: "Uninstalls a tool and drops the capabilities it had been granted, so that\n" +
			"anything installed under the same name later is asked about afresh.\n\n" +
			"To upgrade a tool instead, use `forge tool add` on its source: replacing\n" +
			"keeps the answers you have already given, which removing deliberately\n" +
			"does not.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// Read the grants before removing, so the message can name what
			// was actually dropped rather than claiming something generic.
			held := a.tk.Policy().Granted(args[0]).Kinds()

			if err := a.tk.Remove(args[0]); err != nil {
				return err
			}

			th := ui.ForWriter(c.OutOrStdout())
			fmt.Fprintf(c.OutOrStdout(), "removed %s\n", th.Name.Render(args[0]))
			if len(held) > 0 {
				kinds := make([]string, 0, len(held))
				for _, k := range held {
					kinds = append(kinds, k.Badge()+" "+string(k))
				}
				fmt.Fprintf(c.OutOrStdout(), "  dropped %s\n", strings.Join(kinds, "  "))
				fmt.Fprintf(c.OutOrStdout(), "  %s\n", th.Subtle.Render(
					"a tool installed under this name later will be asked about again"))
			}
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
				if len(a.tk.Policy().Granted(rec.Spec.Name).Kinds()) == 0 {
					fmt.Fprintf(out, "    (nothing granted yet; you will be asked on first use)\n")
				}
			}

			// What was actually granted, from the policy -- which is where
			// grants live. Reading the copy on the tool record made `forge
			// info` report "nothing granted yet" for a tool that `forge grant
			// ls` listed as granted, and the record was the wrong one: nothing
			// ever writes a grant back to it.
			//
			// Printed outside the "wants" block on purpose. A grant can
			// outlive the declaration that prompted it, when an upgrade drops
			// a capability the previous version asked for, and a lingering
			// grant that nothing displays is the harder kind to notice.
			if granted := a.tk.Policy().Granted(rec.Spec.Name); len(granted.Kinds()) > 0 {
				fmt.Fprintf(out, "\n  granted:\n")
				for _, k := range granted.Kinds() {
					fmt.Fprintf(out, "    %s %-12s %s\n",
						k.Badge(), k, strings.Join(granted.Scopes(k), ", "))
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
			reportBuildEnv(out, th, a.tk.BuildEnvInfo())
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

// reportBuildEnv prints where tool builds fetch their modules from.
//
// Worth the four lines because the configuration it describes is otherwise
// invisible until a build fails, and the failure -- a proxy returning
// Forbidden, several frames deep in `go mod tidy` -- does not say which proxy
// was tried or why that one. It also answers the fair objection to adopting the
// machine's settings: the checksum database can now be off, so forge says so
// out loud rather than leaving it to be assumed.
func reportBuildEnv(out io.Writer, th ui.Theme, info build.EnvInfo) {
	fmt.Fprintf(out, "  %s\n", th.OK("module proxy    %s", info.GoProxy))

	if info.GoSumDB == "off" {
		// Not a failure: an air-gapped mirror cannot reach sum.golang.org, and
		// refusing to build there was the bug. But it is the one setting a
		// person should be told is off rather than left to discover.
		fmt.Fprintf(out, "  %s\n", th.Warn("checksum db     off — module checksums are not verified against a database"))
	} else {
		fmt.Fprintf(out, "  %s\n", th.OK("checksum db     %s", info.GoSumDB))
	}

	if info.Private != "" {
		fmt.Fprintf(out, "  %s\n", th.OK("private modules %s", info.Private))
	}
	if info.HTTPProxy != "" {
		fmt.Fprintf(out, "  %s\n", th.OK("http proxy      %s", info.HTTPProxy))
	}

	switch {
	case info.Hermetic:
		fmt.Fprintf(out, "  %s\n", th.OK("build mode      hermetic (FORGE_HERMETIC) — this machine's Go settings are ignored"))
	case !info.HostResolved:
		// The machine was meant to be consulted and could not be, so the values
		// above are forge's defaults wearing the machine's clothes. Saying so
		// is the difference between "my proxy is not being used" taking a
		// minute or an afternoon.
		fmt.Fprintf(out, "  %s\n", th.Warn("build mode      defaults — could not read this machine's Go configuration"))
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
