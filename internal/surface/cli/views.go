package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/labels"
)

func (a *App) cmdView() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "view",
		Short:   "Save and switch between sets of tools",
		GroupID: "manage",
		Long: "A view is a saved label selector: the set of tools you are working with.\n\n" +
			"It narrows every surface at once, which matters most for MCP, where each\n" +
			"tool in the list costs an agent context on every request.",
	}

	list := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List saved views",
		RunE: func(c *cobra.Command, args []string) error {
			views := a.tk.Views().List()
			if len(views) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no views saved — try: forge view set dev 'git || json'")
				return nil
			}
			active := a.tk.Views().ActiveName()
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "\tNAME\tSELECTOR\tTOOLS")
			for _, v := range views {
				marker := " "
				if v.Name == active {
					marker = "*"
				}
				n := "?"
				if sel, err := labels.Parse(v.Selector); err == nil {
					if recs, err := a.tk.List(sel); err == nil {
						n = fmt.Sprint(len(recs))
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, v.Name, v.Selector, n)
			}
			return tw.Flush()
		},
	}

	set := &cobra.Command{
		Use:   "set <name> <selector>",
		Short: "Create or replace a view",
		Long: "Selectors are label expressions:\n\n" +
			"  git                  tools labelled git\n" +
			"  git && !slow         git tools that are not slow\n" +
			"  (git || vcs) && json\n" +
			"  *                    everything",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.tk.Views().Set(args[0], args[1]); err != nil {
				return err
			}
			sel, err := labels.Parse(args[1])
			if err != nil {
				return err
			}
			recs, err := a.tk.List(sel)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "view %s = %s (%d tool(s))\n", args[0], sel, len(recs))
			return nil
		},
	}

	use := &cobra.Command{
		Use:   "use [name]",
		Short: "Make a view the default, or clear it with no argument",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(c *cobra.Command, args []string) error {
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			if err := a.tk.Views().SetActive(name); err != nil {
				return err
			}
			if name == "" {
				fmt.Fprintln(c.OutOrStdout(), "showing all tools")
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "now using view %s\n", name)
			return nil
		},
		ValidArgsFunction: a.completeViewNames,
	}

	rm := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a view",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.tk.Views().Delete(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed view %s\n", args[0])
			return nil
		},
		ValidArgsFunction: a.completeViewNames,
	}

	cmd.AddCommand(list, set, use, rm)
	return cmd
}

func (a *App) completeViewNames(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	var out []string
	for _, v := range a.tk.Views().List() {
		if strings.HasPrefix(v.Name, prefix) {
			out = append(out, v.Name+"\t"+v.Selector)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func (a *App) cmdLabel() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "label",
		Short:   "Add and remove your own labels on a tool",
		GroupID: "manage",
		Long: "Labels you add are kept separately from the ones a tool declares, so\n" +
			"reinstalling the tool does not discard them.",
	}

	add := &cobra.Command{
		Use:   "add <tool> <label>...",
		Short: "Add labels to a tool",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			rec, err := a.tk.Get(args[0])
			if err != nil {
				return err
			}
			for _, l := range args[1:] {
				if !containsStr(rec.ExtraLabels, l) && !rec.Spec.HasLabel(l) {
					rec.ExtraLabels = append(rec.ExtraLabels, l)
				}
			}
			if err := a.tk.Store().Put(rec); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s: %s\n", rec.Spec.Name, strings.Join(rec.Labels(), ", "))
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}

	rm := &cobra.Command{
		Use:     "remove <tool> <label>...",
		Aliases: []string{"rm"},
		Short:   "Remove labels you added",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			rec, err := a.tk.Get(args[0])
			if err != nil {
				return err
			}
			for _, l := range args[1:] {
				if rec.Spec.HasLabel(l) {
					// A tool's own labels come from its manifest, so removing
					// one here would be undone by the next reinstall. Say so
					// rather than appearing to work.
					return fmt.Errorf("%q is one of %s's own labels and cannot be removed; use a view to filter it out instead", l, rec.Spec.Name)
				}
				rec.ExtraLabels = removeStr(rec.ExtraLabels, l)
			}
			if err := a.tk.Store().Put(rec); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s: %s\n", rec.Spec.Name, strings.Join(rec.Labels(), ", "))
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}

	cmd.AddCommand(add, rm)
	return cmd
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func removeStr(hay []string, needle string) []string {
	out := hay[:0]
	for _, s := range hay {
		if s != needle {
			out = append(out, s)
		}
	}
	return out
}
