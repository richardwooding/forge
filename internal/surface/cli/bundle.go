package cli

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/ui"
)

func (a *App) cmdExport() *cobra.Command {
	var out string

	cmd := &cobra.Command{
		Use:     "export",
		Short:   "Write the tools in view to a bundle",
		GroupID: "manage",
		Long: "Writes the tools matching the current view to a single file another\n" +
			"machine can import.\n\n" +
			"A bundle carries the modules, their manifests and the labels you added.\n" +
			"It does not carry what those tools are allowed to do: that is decided on\n" +
			"the machine they run on, and a bundle that arrived pre-authorised would\n" +
			"let whoever built it decide for you.\n\n" +
			"Exports are reproducible, so the same tools always produce the same bytes\n" +
			"and two bundles can simply be compared.",
		RunE: func(c *cobra.Command, args []string) error {
			w := c.OutOrStdout()
			if out != "" && out != "-" {
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				w = f
			} else if isTerminal(w) {
				// A bundle is compressed binary; writing it to a terminal
				// leaves the terminal needing a reset and the user with
				// nothing they can use.
				return fmt.Errorf("refusing to write a bundle to the terminal; use -o <file> or pipe it")
			}

			n, err := a.tk.Export(w, a.selector)
			if err != nil {
				return err
			}
			th := ui.ForWriter(c.ErrOrStderr())
			fmt.Fprintf(c.ErrOrStderr(), "%s\n", th.OK("exported %d tool(s) from %s", n, a.describeView()))
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this file instead of standard output")
	return cmd
}

func (a *App) cmdImport() *cobra.Command {
	var conflict string

	cmd := &cobra.Command{
		Use:     "import <bundle>",
		Short:   "Install the tools from a bundle",
		GroupID: "manage",
		Args:    cobra.MaximumNArgs(1),
		Long: "Installs the tools in a bundle, revalidating every manifest and loading\n" +
			"every module first: a bundle is ordinary untrusted input, and one bad\n" +
			"tool must not leave half an import behind.\n\n" +
			"Nothing is granted by importing. Each tool asks on first use, exactly as\n" +
			"one you built yourself would — including a tool that replaces one you had\n" +
			"already approved, because a module from elsewhere is a different module\n" +
			"whatever it calls itself.",
		RunE: func(c *cobra.Command, args []string) error {
			policy := toolkit.OnConflict(conflict)
			if !policy.Valid() {
				return fmt.Errorf("--on-conflict %q: choose skip, replace, rename or fail", conflict)
			}

			r := c.InOrStdin()
			if len(args) == 1 && args[0] != "-" {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				r = f
			}

			res, err := a.tk.Import(c.Context(), r, policy)
			if err != nil {
				return err
			}
			report(c, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&conflict, "on-conflict", string(toolkit.ConflictSkip),
		"what to do about a name already installed: skip, replace, rename or fail")
	return cmd
}

func report(c *cobra.Command, res *toolkit.ImportResult) {
	out := c.OutOrStdout()
	th := ui.ForWriter(out)

	for _, n := range res.Installed {
		fmt.Fprintf(out, "%s\n", th.OK("installed %s", th.Name.Render(n)))
	}
	for _, n := range res.Replaced {
		fmt.Fprintf(out, "%s\n", th.OK("replaced  %s", th.Name.Render(n)))
	}
	for _, from := range sortedKeys(res.Renamed) {
		fmt.Fprintf(out, "%s\n", th.OK("installed %s %s",
			th.Name.Render(res.Renamed[from]), th.Subtle.Render("(was "+from+", which is taken)")))
	}
	for _, n := range res.Skipped {
		fmt.Fprintf(out, "%s\n", th.Warn("skipped   %s %s",
			n, th.Subtle.Render("(already installed; --on-conflict replace to overwrite)")))
	}

	total := len(res.Installed) + len(res.Replaced) + len(res.Renamed)
	if total > 0 {
		// Said every time, because it is the thing someone importing a
		// stranger's bundle most needs to know and least expects.
		fmt.Fprintf(out, "\n%s\n", th.Subtle.Render(
			"nothing is granted by importing; each tool will ask on first use"))
	}
	if total == 0 && len(res.Skipped) > 0 {
		fmt.Fprintf(out, "\n%s\n", th.Subtle.Render("nothing was installed"))
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
