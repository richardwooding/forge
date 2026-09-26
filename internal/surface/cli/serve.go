package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/richardwooding/forge/internal/surface/grpcsvc"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/surface/rest"

	forgev1 "github.com/richardwooding/forge/grpcapi/forge/v1"
)

func (a *App) cmdServe() *cobra.Command {
	var (
		addr        string
		socket      string
		allowOrigin []string
		metaTools   bool
	)

	cmd := &cobra.Command{
		Use:     "serve",
		Short:   "Serve the tools over HTTP: REST, OpenAPI and MCP together",
		GroupID: "manage",
		Long: "Runs every HTTP surface on one address.\n\n" +
			"  /                     a plain list of the tools in view\n" +
			"  /v1/tools             the same, as JSON\n" +
			"  /v1/openapi.json      an OpenAPI 3.1 document for this view\n" +
			"  /mcp                  the Model Context Protocol\n\n" +
			"A named view is served under /v1/views/<view>/... and /mcp/<view>, so a\n" +
			"client configured with one URL always sees the same set of tools.\n\n" +
			"Defaults to a unix socket, which no browser can reach and which needs no\n" +
			"port. A TCP address is refused beyond localhost: there is no\n" +
			"authentication yet, so binding wider would publish every tool you have.",
		RunE: func(c *cobra.Command, args []string) error {
			mgr := mcpsrv.New(mcpsrv.Options{
				Toolkit:   a.tk,
				Views:     a.tk.Views(),
				MetaTools: metaTools,
			})
			mgr.StartWatch(c.Context())

			api := rest.New(rest.Options{
				Toolkit: a.tk,
				Views:   a.tk.Views(),
				Default: a.selector,
			})

			grpcSrv := grpc.NewServer()
			forgev1.RegisterToolServiceServer(grpcSrv, grpcsvc.New(grpcsvc.Options{
				Toolkit: a.tk, Views: a.tk.Views(), Default: a.selector,
			}))
			// Reflection, so grpcurl and grpcui work without a .proto file to
			// hand. The service is fixed, so this describes forge's API, not
			// the tools -- those are discovered through ListTools.
			reflection.Register(grpcSrv)

			mux := http.NewServeMux()
			mux.Handle("/mcp", mgr.Handler(mcpsrv.HTTPOptions{
				Views: a.tk.Views(), Default: a.selector, AllowedOrigins: allowOrigin,
			}))
			mux.Handle("/mcp/", mgr.Handler(mcpsrv.HTTPOptions{
				Views: a.tk.Views(), Default: a.selector, AllowedOrigins: allowOrigin,
			}))
			mux.Handle("/", originGuard(allowOrigin, api.Handler()))

			// One handler for everything. gRPC speaks prior-knowledge HTTP/2,
			// which without TLS means no ALPN, so the server has to accept
			// unencrypted HTTP/2 for a gRPC client and a curl to share an
			// address.
			//
			// Through http.Server.Protocols rather than x/net/http2/h2c: that
			// package is deprecated in favour of exactly this, and using it
			// would mean an extra dependency to do something the standard
			// library now does itself.
			//
			// grpc.Server.ServeHTTP is documented as experimental and slightly
			// lower fidelity than serving a raw listener. For a localhost
			// multitool that is a fair trade for one address, and the fallback
			// if it ever matters is separate ports rather than cmux and its
			// byte-peeking at the HTTP/2 preface.
			root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
					grpcSrv.ServeHTTP(w, r)
					return
				}
				mux.ServeHTTP(w, r)
			})

			ln, describe, err := listen(addr, socket)
			if err != nil {
				return err
			}
			defer func() { _ = ln.Close() }()

			protocols := new(http.Protocols)
			protocols.SetHTTP1(true)
			protocols.SetUnencryptedHTTP2(true)

			srv := &http.Server{
				Handler:           root,
				Protocols:         protocols,
				ReadHeaderTimeout: 10 * time.Second,
			}

			fmt.Fprintf(c.ErrOrStderr(), "forge serve: %s  (%s)\n", describe, a.describeView())
			fmt.Fprintf(c.ErrOrStderr(), "  REST %s/v1/tools   OpenAPI %s/v1/openapi.json   MCP %s/mcp\n",
				describe, describe, describe)
			fmt.Fprintf(c.ErrOrStderr(), "  gRPC forge.v1.ToolService on the same address (h2c)\n")

			go func() {
				<-c.Context().Done()
				shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdown)
				// GracefulStop blocks for as long as any server-stream is
				// open, so it gets the same deadline rather than the power to
				// hang shutdown indefinitely on one long InvokeStream.
				done := make(chan struct{})
				go func() { grpcSrv.GracefulStop(); close(done) }()
				select {
				case <-done:
				case <-shutdown.Done():
					grpcSrv.Stop()
				}
			}()

			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "http", "", "listen on this TCP address instead of a unix socket (e.g. 127.0.0.1:7777)")
	cmd.Flags().StringVar(&socket, "socket", "", "unix socket path (default: $XDG_RUNTIME_DIR/forge/forge.sock)")
	cmd.Flags().StringArrayVar(&allowOrigin, "allow-origin", nil, "accept browser requests from this origin")
	cmd.Flags().BoolVar(&metaTools, "meta-tools", true, "offer the MCP discovery meta-tools")
	return cmd
}

// listen opens the socket forge will serve on.
//
// A unix socket is the default because it is the safest thing that still
// works: no browser can reach it whatever a page tries, there is no port to
// collide with anything, and the filesystem permissions are the access
// control. TCP is opt-in and refused beyond loopback, since forge has no
// authentication yet and binding wider would publish every installed tool to
// the network.
func listen(addr, socket string) (net.Listener, string, error) {
	if addr != "" && socket != "" {
		return nil, "", errors.New("give --http or --socket, not both")
	}

	if addr != "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, "", fmt.Errorf("--http %q: %w", addr, err)
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return nil, "", fmt.Errorf("--http %q binds beyond localhost, which would expose every tool you have to the network without authentication; use 127.0.0.1", addr)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", err
		}
		return ln, "http://" + addr, nil
	}

	if socket == "" {
		socket = defaultSocket()
	}
	if err := os.MkdirAll(filepathDir(socket), 0o700); err != nil {
		return nil, "", err
	}
	// A socket left behind by a process that did not shut down cleanly would
	// make every later start fail with "address already in use".
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, "", err
	}
	// Only this user. The socket is the access control, so it has to actually
	// control access.
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, "", err
	}
	return ln, "unix:" + socket, nil
}

func defaultSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return dir + "/forge/forge.sock"
}

func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// originGuard rejects cross-origin browser requests to the REST surface, for
// the same reason the MCP one does: a page the user is already viewing can
// POST to localhost, and DNS rebinding makes that same-origin from the
// browser's side. A request with no Origin is not from a browser.
func originGuard(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if slices.Contains(allowed, origin) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, fmt.Sprintf("origin %q is not allowed; start forge with --allow-origin if this is intended", origin),
			http.StatusForbidden)
	})
}
