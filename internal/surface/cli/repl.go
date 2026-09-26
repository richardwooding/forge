package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/surface/repl"
)

func (a *App) cmdREPL() *cobra.Command {
	return &cobra.Command{
		Use:     "repl",
		Short:   "An interactive shell over your tools",
		GroupID: "manage",
		Long: "A line-oriented shell. Type a tool name and flags exactly as you would\n" +
			"on the command line; colon commands do the rest.\n\n" +
			"Every line goes through the same command tree `forge` itself uses, so\n" +
			"nothing here can behave differently from the CLI.",
		RunE: func(c *cobra.Command, args []string) error {
			if !isTerminal(c.OutOrStdout()) {
				return errors.New("forge repl needs a terminal; pipe into `forge run` instead")
			}
			return repl.Run(c.Context(), repl.Options{
				Toolkit: a.tk,
				Dispatcher: NewDispatcher(Options{
					Toolkit:  a.tk,
					Selector: a.selector,
					ViewName: a.viewName,
				}),
				Selector: a.selector,
				ViewName: a.viewName,
			})
		},
	}
}
