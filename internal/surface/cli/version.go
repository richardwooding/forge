package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/buildinfo"
)

func (a *App) cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Show which forge this is",
		GroupID: "manage",
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprintln(c.OutOrStdout(), buildinfo.String())
			return nil
		},
	}
}
