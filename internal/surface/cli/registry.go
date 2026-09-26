package cli

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/bundle"
	"github.com/richardwooding/forge/internal/ocidist"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/ui"
)

func (a *App) cmdPush() *cobra.Command {
	var insecure bool

	cmd := &cobra.Command{
		Use:     "push <reference>",
		Short:   "Publish the tools in view to an OCI registry",
		GroupID: "manage",
		Args:    cobra.ExactArgs(1),
		Long: "Publishes the tools matching the current view as an OCI artifact, so any\n" +
			"registry can store them: ghcr, a company's own, one running on a laptop.\n\n" +
			"Credentials come from `docker login` or `podman login`.\n\n" +
			"A pushed artifact carries the same thing a bundle does, and just as\n" +
			"deliberately does not carry what the tools are allowed to do.",
		RunE: func(c *cobra.Command, args []string) error {
			var buf bytes.Buffer
			n, err := a.tk.Export(&buf, a.selector)
			if err != nil {
				return err
			}
			br, err := bundle.Read(bytes.NewReader(buf.Bytes()))
			if err != nil {
				return err
			}
			img, err := ocidist.Build(br)
			if err != nil {
				return err
			}

			th := ui.ForWriter(c.ErrOrStderr())
			fmt.Fprintf(c.ErrOrStderr(), "pushing %d tool(s) to %s…\n", n, args[0])

			digest, err := ocidist.Push(args[0], img, ocidist.Options{Insecure: insecure})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s\n", th.OK("pushed %s", digest))
			return nil
		},
	}
	cmd.Flags().BoolVar(&insecure, "insecure", false, "allow plain HTTP, for a registry running locally")
	return cmd
}

func (a *App) cmdPull() *cobra.Command {
	var (
		insecure bool
		conflict string
	)

	cmd := &cobra.Command{
		Use:     "pull <reference>",
		Short:   "Install tools from an OCI registry",
		GroupID: "manage",
		Args:    cobra.ExactArgs(1),
		Long: "Fetches a forge artifact and installs the tools in it, with the same\n" +
			"checks `forge import` makes: every manifest revalidated, every module\n" +
			"loaded, nothing granted.",
		RunE: func(c *cobra.Command, args []string) error {
			policy := toolkit.OnConflict(conflict)
			if !policy.Valid() {
				return fmt.Errorf("--on-conflict %q: choose skip, replace, rename or fail", conflict)
			}

			fmt.Fprintf(c.ErrOrStderr(), "pulling %s…\n", args[0])
			img, err := ocidist.Pull(args[0], ocidist.Options{Insecure: insecure})
			if err != nil {
				return err
			}

			// Back through the bundle path rather than installing from the
			// artifact directly, so a registry and a file take exactly the
			// same route into the store and cannot diverge in what they check.
			raw, err := ocidist.ToBundle(img)
			if err != nil {
				return err
			}
			res, err := a.tk.Import(c.Context(), bytes.NewReader(raw), policy)
			if err != nil {
				return err
			}
			report(c, res)
			return nil
		},
	}
	cmd.Flags().BoolVar(&insecure, "insecure", false, "allow plain HTTP, for a registry running locally")
	cmd.Flags().StringVar(&conflict, "on-conflict", string(toolkit.ConflictSkip),
		"what to do about a name already installed: skip, replace, rename or fail")
	return cmd
}
