package mcpsrv

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/view"
)

// ServeStdio runs one view over stdio, which is how an agent normally starts
// forge.
//
// The caller must have arranged for nothing else to write to stdout. That is
// not a nicety: stdout is the JSON-RPC stream, so a single stray line of
// logging -- or a tool's own print, if it were inherited rather than captured
// -- corrupts the protocol and produces parse errors on the client that say
// nothing about where they came from.
func (m *Manager) ServeStdio(ctx context.Context, key string, sel labels.Selector) error {
	srv, err := m.Server(key, sel)
	if err != nil {
		return err
	}
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// HTTPOptions configure the streamable HTTP transport.
type HTTPOptions struct {
	// Views resolves a view name from the URL path. Nil means every request
	// gets the default view.
	Views *view.Store

	// Default is the selector for requests that name no view.
	Default labels.Selector

	// AllowedOrigins are the Origin header values accepted from a browser.
	// Empty means browser requests are refused outright.
	AllowedOrigins []string
}

// Handler serves MCP over HTTP, choosing the view from the path.
//
// Mounting one view per path is what makes this useful: each client config
// pins the set of tools it sees, which is the whole context-saving goal, and it
// stays stateless, so nothing has to be remembered between requests.
func (m *Manager) Handler(opts HTTPOptions) http.Handler {
	mux := http.NewServeMux()

	get := func(r *http.Request) *mcp.Server {
		key := strings.Trim(r.PathValue("view"), "/")
		sel := opts.Default
		if sel == nil {
			sel = labels.All
		}
		if key != "" && opts.Views != nil {
			resolved, _, err := opts.Views.Resolve(key, "")
			if err != nil {
				// An unknown view must not fall back to everything: on this
				// surface the view is the only boundary, so a typo would
				// silently widen what is exposed. Serving nothing is the safe
				// failure.
				sel = labels.None
			} else {
				sel = resolved
			}
		}
		srv, err := m.Server(key, sel)
		if err != nil {
			return nil
		}
		return srv
	}

	handler := mcp.NewStreamableHTTPHandler(get, nil)
	guarded := originGuard(opts.AllowedOrigins, handler)

	mux.Handle("/mcp/{view}", guarded)
	mux.Handle("/mcp/{view}/", guarded)
	mux.Handle("/mcp", guarded)
	mux.Handle("/mcp/", guarded)
	return mux
}

// originGuard rejects cross-origin browser requests.
//
// "It is only listening on localhost" is not a defence. A page the user is
// already viewing can POST to 127.0.0.1, and DNS rebinding turns that into a
// same-origin request from the browser's point of view; the MCP specification
// calls this out specifically for local servers. A request with no Origin at
// all is not from a browser and is allowed, which is what lets an ordinary
// client work.
func originGuard(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		for _, ok := range allowed {
			if origin == ok {
				next.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, fmt.Sprintf("origin %q is not allowed; start forge with --allow-origin if this is intended", origin),
			http.StatusForbidden)
	})
}
