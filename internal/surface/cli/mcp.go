package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/surface/mcpsrv"
)

func (a *App) cmdMCP() *cobra.Command {
	var (
		addr         string
		metaTools    bool
		allowInstall bool
		allowOrigin  []string
	)

	cmd := &cobra.Command{
		Use:     "mcp",
		Short:   "Serve the tools over the Model Context Protocol",
		GroupID: "manage",
		Long: "Runs an MCP server so an agent can call your tools.\n\n" +
			"With no --http address this speaks the stdio protocol, which is how an\n" +
			"agent normally launches forge. The active view decides which tools are\n" +
			"exposed, and that is the point: every tool in the list costs the agent\n" +
			"context on every request it makes.\n\n" +
			"Over HTTP the view comes from the path, so /mcp/dev serves the dev view\n" +
			"and each client config can pin its own.\n\n" +
			"--allow-install adds forge_add_tool, so an agent can compile and install a\n" +
			"tool without leaving MCP. It asks the connected client to approve through\n" +
			"MCP elicitation before installing anything, and refuses outright when the\n" +
			"client cannot be asked -- but approval does not undo the risk `forge tool\n" +
			"add` already carries: building runs the Go toolchain, unsandboxed, over\n" +
			"whatever source it is given. Off by default.",
		RunE: func(c *cobra.Command, args []string) error {
			mgr := mcpsrv.New(mcpsrv.Options{
				Toolkit:      a.tk,
				Views:        a.tk.Views(),
				MetaTools:    metaTools,
				AllowInstall: allowInstall,
			})
			// A tool installed, or a view edited, from a terminal has to reach
			// this server without it being restarted.
			mgr.StartWatch(c.Context())

			if addr == "" {
				return a.serveStdio(c, mgr)
			}
			return a.serveHTTP(c, mgr, addr, allowOrigin)
		},
	}

	cmd.Flags().StringVar(&addr, "http", "", "serve over HTTP on this address instead of stdio (e.g. 127.0.0.1:7777)")
	cmd.Flags().BoolVar(&metaTools, "meta-tools", true, "offer forge_search_tools and forge_describe_tool so an agent can discover tools outside its view")
	cmd.Flags().BoolVar(&allowInstall, "allow-install", false, "offer forge_add_tool, gated by MCP elicitation, so an agent can install a tool live")
	cmd.Flags().StringArrayVar(&allowOrigin, "allow-origin", nil, "accept browser requests from this origin")
	return cmd
}

func (a *App) serveStdio(cmd *cobra.Command, mgr *mcpsrv.Manager) error {
	// stdout belongs to the protocol from here on. Anything forge wants to say
	// goes to stderr, or it corrupts the stream and the client reports a parse
	// error that explains nothing.
	fmt.Fprintf(os.Stderr, "forge mcp: serving %s over stdio\n", a.describeView())

	err := mgr.ServeStdio(cmd.Context(), a.viewName, a.selector)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (a *App) serveHTTP(cmd *cobra.Command, mgr *mcpsrv.Manager, addr string, origins []string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--http %q: %w", addr, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		// Binding beyond the loopback interface exposes every tool to the
		// network with no authentication at all. Refusing is the right default;
		// the day forge grows a token it can grow a flag as well.
		return fmt.Errorf("--http %q binds beyond localhost, which would expose your tools to the network without authentication; use 127.0.0.1", addr)
	}

	srv := &http.Server{
		Addr: addr,
		Handler: mgr.Handler(mcpsrv.HTTPOptions{
			Views:          a.tk.Views(),
			Default:        a.selector,
			AllowedOrigins: origins,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "forge mcp: http://%s/mcp  (%s)\n", addr, a.describeView())
	fmt.Fprintf(cmd.ErrOrStderr(), "  a named view is served at /mcp/<view>\n")

	go func() {
		<-cmd.Context().Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *App) describeView() string {
	if a.viewName != "" {
		return fmt.Sprintf("view %q", a.viewName)
	}
	if a.selector != nil && a.selector.String() != "*" {
		return fmt.Sprintf("tools matching %s", a.selector)
	}
	return "all tools"
}
