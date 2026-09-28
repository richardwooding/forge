package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	var scopes []string
	allow := &cobra.Command{
		Use:   "allow <tool> [--scope kind=value]...",
		Short: "Grant what a tool declares, optionally narrowed, without being prompted",
		Long: "Grants what the tool's manifest asks for. The floor still applies: a\n" +
			"request forge never grants is refused here too.\n\n" +
			"--scope narrows one capability to something smaller than the manifest\n" +
			"asks for, and is the only way to give a tool a usable filesystem grant.\n" +
			"A tool cannot name your directories in its own source, so it declares\n" +
			"fs.read as * and you say which directory you meant:\n\n" +
			"    forge grant allow hashsum --scope fs.read=~/Downloads\n" +
			"    forge grant allow fetch --scope net.http=api.github.com\n\n" +
			"Repeat it per capability. It only ever narrows: a kind the tool never\n" +
			"asked for is refused, and so is a value outside what it declared.",
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
			narrowed, err := parseScopes(scopes)
			if err != nil {
				return err
			}
			reqs, err := narrowRequests(args[0], rec.Spec.Requires, narrowed)
			if err != nil {
				return err
			}
			floor := a.tk.Floor()
			var grants []capability.Grant
			for _, req := range reqs {
				if why := floor.Check(req.Kind, req.Scope); why != "" {
					return fmt.Errorf("%s asks for %s covering %s, which forge never grants", args[0], req.Kind, why)
				}
				grants = append(grants, capability.Grant{Kind: req.Kind, Scope: req.Scope})
			}
			if err := a.tk.Policy().Grant(args[0], grants); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "granted %s: %s\n", args[0], describeRequests(reqs))
			return nil
		},
		ValidArgsFunction: a.completeToolNames,
	}
	allow.Flags().StringArrayVar(&scopes, "scope", nil,
		"narrow one capability, as kind=value (repeatable), e.g. fs.read=/home/you/Downloads")

	cmd.AddCommand(list, revoke, allow)
	return cmd
}

// parseScopes turns repeated --scope kind=value flags into scopes per kind.
//
// Values for a filesystem kind are made absolute, and a leading ~ is expanded,
// because a person narrowing a grant from a shell will type a relative path or
// a tilde and forge mounts only absolute ones. Getting that wrong produces a
// grant that looks right in `forge grant ls` and fails at invoke time, which is
// the exact failure this whole flag exists to end.
func parseScopes(flags []string) (map[capability.Kind][]string, error) {
	out := map[capability.Kind][]string{}
	for _, f := range flags {
		name, value, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("--scope %q is not kind=value, e.g. --scope fs.read=/home/you/Downloads", f)
		}
		kind := capability.Kind(strings.TrimSpace(name))
		if kind == "" {
			return nil, fmt.Errorf("--scope %q names no capability; write it as kind=value, e.g. fs.read=/home/you/Downloads", f)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("--scope %s= has no value; to grant nothing for a capability, leave it out and it will be refused on use", name)
		}
		if kind == capability.FSRead || kind == capability.FSWrite {
			expanded, err := absPath(value)
			if err != nil {
				return nil, fmt.Errorf("--scope %s=%s: %w", kind, value, err)
			}
			value = expanded
		}
		out[kind] = append(out[kind], value)
	}
	return out, nil
}

// absPath expands a leading ~ and makes the result absolute.
func absPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return filepath.Abs(p)
}

// narrowRequests applies --scope to what the tool declared.
//
// It only ever narrows. A kind the tool never asked for is refused rather than
// granted quietly: --scope is for saying which directory you meant, not for
// handing a tool something it did not ask for and cannot be expecting. And
// where the tool named concrete scopes, an override must be one of them, so the
// flag can drop scopes but never invent one.
//
// A declared scope of "*" is the open case and anything the floor permits may
// replace it. That is the normal shape for fs.read: a tool cannot know your
// directories, so it declares * and asks, in its reason, to be narrowed.
func narrowRequests(name string, declared []capability.Request, narrowed map[capability.Kind][]string) ([]capability.Request, error) {
	byKind := map[capability.Kind]bool{}
	for _, r := range declared {
		byKind[r.Kind] = true
	}
	for kind := range narrowed {
		if !byKind[kind] {
			return nil, fmt.Errorf("%s does not ask for %s, so there is nothing to narrow; it declares %s",
				name, kind, declaredKinds(declared))
		}
	}

	out := make([]capability.Request, 0, len(declared))
	for _, req := range declared {
		scope, ok := narrowed[req.Kind]
		if !ok {
			out = append(out, req)
			continue
		}
		if !open(req.Scope) {
			for _, s := range scope {
				if !slices.Contains(req.Scope, s) {
					return nil, fmt.Errorf("%s asks for %s only over %s, so it cannot be narrowed to %q",
						name, req.Kind, strings.Join(req.Scope, ", "), s)
				}
			}
		}
		req.Scope = scope
		out = append(out, req)
	}
	return out, nil
}

// open reports whether a declared scope is the unrestricted "*".
func open(scope []string) bool {
	return slices.Contains(scope, "*")
}

func declaredKinds(reqs []capability.Request) string {
	var parts []string
	for _, r := range reqs {
		parts = append(parts, string(r.Kind))
	}
	return strings.Join(parts, ", ")
}
