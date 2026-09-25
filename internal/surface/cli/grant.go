package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/ui"
)

func (a *App) cmdGrant() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "grant",
		Short:   "See and change what tools are allowed to do",
		GroupID: "manage",
	}

	list := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Show what each tool has been granted",
		RunE: func(c *cobra.Command, args []string) error {
			tools := a.tk.Policy().Tools()
			if len(tools) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no tool has asked for anything yet")
				return nil
			}
			th := ui.ForWriter(c.OutOrStdout())
			tbl := th.NewTable("TOOL", "GRANTED")
			for _, t := range tools {
				set := a.tk.Policy().Granted(t)
				var parts []string
				for _, k := range set.Kinds() {
					parts = append(parts, fmt.Sprintf("%s %s(%s)", k.Badge(), k, strings.Join(set.Scopes(k), ",")))
				}
				if len(parts) == 0 {
					parts = append(parts, "nothing (refused)")
				}
				tbl.Row(th.Name.Render(t), strings.Join(parts, " "))
			}
			tbl.Render(c.OutOrStdout())
			return nil
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <tool>",
		Short: "Forget what was decided for a tool, so it asks again",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.tk.Policy().Revoke(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s will be asked about again on its next use\n", args[0])
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}

	allow := &cobra.Command{
		Use:   "allow <tool>",
		Short: "Grant everything a tool declares, without being prompted",
		Long: "Grants exactly what the tool's manifest asks for. The floor still\n" +
			"applies: a request forge never grants is refused here too.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			rec, err := a.tk.Get(args[0])
			if err != nil {
				return err
			}
			if len(rec.Spec.Requires) == 0 {
				fmt.Fprintf(c.OutOrStdout(), "%s does not ask for anything\n", args[0])
				return nil
			}
			floor := a.tk.Floor()
			var grants []capability.Grant
			for _, req := range rec.Spec.Requires {
				if why := floor.Check(req.Kind, req.Scope); why != "" {
					return fmt.Errorf("%s asks for %s covering %s, which forge never grants", args[0], req.Kind, why)
				}
				grants = append(grants, capability.Grant{Kind: req.Kind, Scope: req.Scope})
			}
			if err := a.tk.Policy().Grant(args[0], grants); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "granted %s: %s\n", args[0], describeRequests(rec.Spec.Requires))
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}

	cmd.AddCommand(list, revoke, allow)
	return cmd
}
