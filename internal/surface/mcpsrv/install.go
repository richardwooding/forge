package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/manifest"
	"github.com/richardwooding/forge/internal/toolkit"
)

// approveRequestID names the one input request forge_add_tool ever asks for.
// It is echoed back in CallToolResult.InputRequests and read back out of
// CallToolParamsRaw.InputResponses under the same key.
const approveRequestID = "approve"

// errStopAfterDescribe aborts Toolkit.Add once the module has described
// itself, before anything is written to the store. It carries no data of its
// own; beginInstall reads the manifest through the closure that returns it.
var errStopAfterDescribe = errors.New("forge: stopping before install to ask for approval")

// addInstallTool gives an agent a way to compile and install a tool without
// leaving MCP.
//
// It is the one meta-tool that changes what forge exposes, which is exactly
// what addMetaTools' own doc comment says a set_view tool must never do
// unattended: the view is the only access boundary on a localhost server, and
// a tool that widens it is a privilege-escalation primitive reachable by
// prompt injection in any other tool's output. Installing a new tool is a
// stronger version of the same move, so it does not get the same restraint --
// it gets a human in the loop instead, through an MCP elicitation (SEP-2322)
// that must be answered before anything reaches the store.
//
// That still leaves the risk the CLI's own README names: building runs the Go
// toolchain, unsandboxed, over whatever source it is given, before forge or
// the human giving approval knows what the tool wants. AllowInstall's doc
// comment says so; this is the surface where the same trust decision as
// `forge tool add` is made on an agent's say-so instead of a person's typed
// command, which is why it defaults to off.
func (m *Manager) addInstallTool(srv *mcp.Server, sel labels.Selector) {
	srv.AddTool(&mcp.Tool{
		Name: "forge_add_tool",
		Description: "Compile a Go program and install it as a forge tool, live on every surface " +
			"immediately -- the MCP equivalent of `forge tool add`. The source must be a single Go " +
			"file that registers itself with the forge SDK (github.com/richardwooding/forge/sdk/tool), " +
			"the same shape forge_describe_tool shows for an installed tool. forge asks the connected " +
			"human to approve before installing anything, and refuses if there is no one to ask. " +
			"Approval does not undo the risk of compiling: building already runs the Go toolchain, " +
			"unsandboxed, over the given source, the same trust decision as running `go build` on it " +
			"directly.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"source": {
					Type:        "string",
					Description: "a single Go file's source, including `package main` and a tool.Register call",
				},
			},
			Required: []string{"source"},
		},
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return m.handleInstall(ctx, req, sel)
	})
}

// handleInstall is called twice per install: once with no InputResponses, to
// describe the source and ask for approval, and once more with the answer
// attached, to act on it. The SDK's multi-round-trip middleware (SEP-2322)
// is what turns beginInstall's InputRequests into that second call -- on the
// client side for a client that supports it, and transparently on forge's own
// side, via a synchronous elicitation, for one that does not. Either way this
// function never calls the client itself; it only ever reads what the
// framework already collected.
func (m *Manager) handleInstall(ctx context.Context, req *mcp.CallToolRequest, sel labels.Selector) (*mcp.CallToolResult, error) {
	var args struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil || strings.TrimSpace(args.Source) == "" {
		return errorResult(`forge_add_tool needs Go source in "source"`), nil //nolint:nilerr // MCP carries tool errors as results
	}

	path, cleanup, err := stageSource(args.Source)
	if err != nil {
		return nil, fmt.Errorf("staging source: %w", err)
	}
	defer cleanup()

	if resp, asked := req.Params.InputResponses[approveRequestID]; asked {
		return m.finishInstall(ctx, path, resp, sel)
	}
	return beginInstall(ctx, m.opts.Toolkit, path)
}

// beginInstall builds and describes src, then hands back an elicitation
// asking a human to approve it. Nothing is written to the store on this leg,
// whatever the eventual answer turns out to be -- the multi-round-trip
// protocol requires the ask and the commit to be separate requests, and
// stopping here is what keeps Toolkit.Add's own guarantee: a tool that does
// not end up approved leaves no more trace than one that fails to build.
func beginInstall(ctx context.Context, tk *toolkit.Toolkit, path string) (*mcp.CallToolResult, error) {
	var loaded *manifest.Loaded
	_, err := tk.Add(ctx, path, toolkit.WithApprove(func(_ context.Context, l *manifest.Loaded) error {
		loaded = l
		return errStopAfterDescribe
	}))
	if !errors.Is(err, errStopAfterDescribe) {
		// A real build, compile or manifest failure. It is recoverable by
		// editing the source, so it comes back as a result, not a protocol
		// error, and no approval is asked for source that does not even build.
		return errorResult(err.Error()), nil //nolint:nilerr // MCP carries tool errors as results
	}

	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{
			approveRequestID: &mcp.ElicitParams{
				Message: installMessage(loaded),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"approve": {
							Type:        "boolean",
							Description: "true to compile and install this tool on this machine now",
						},
					},
					Required: []string{"approve"},
				},
			},
		},
	}, nil
}

// finishInstall runs once the elicitation beginInstall asked for has been
// answered. It rebuilds src -- cheap next to asking a human, and it means
// nothing about the tool has to be cached between two independent JSON-RPC
// requests -- and commits only if the answer was yes.
func (m *Manager) finishInstall(ctx context.Context, path string, resp mcp.InputResponse, sel labels.Selector) (*mcp.CallToolResult, error) {
	if !approved(resp) {
		return errorResult("forge_add_tool: install declined, nothing was written to the store"), nil
	}

	res, err := m.opts.Toolkit.Add(ctx, path)
	if err != nil {
		return errorResult(err.Error()), nil //nolint:nilerr // MCP carries tool errors as results
	}

	if err := m.Sync(); err != nil {
		return nil, fmt.Errorf("installed but could not refresh the tool list: %w", err)
	}
	return textResult(installSummary(res, sel)), nil
}

func approved(resp mcp.InputResponse) bool {
	er, ok := resp.(*mcp.ElicitResult)
	if !ok || er.Action != "accept" {
		return false
	}
	approve, _ := er.Content["approve"].(bool)
	return approve
}

// stageSource writes source to a throwaway .go file so it can be handed to
// Toolkit.Add, which builds from a path.
func stageSource(source string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "forge-mcp-install-*.go")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }

	if _, err := f.WriteString(source); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

func installMessage(loaded *manifest.Loaded) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An agent wants to install %q on this machine.\n", loaded.Spec.Name)
	if loaded.Spec.Summary != "" {
		fmt.Fprintf(&b, "%s\n", loaded.Spec.Summary)
	}
	fmt.Fprintf(&b, "\nCompiling it to get this description already ran the Go toolchain, "+
		"unsandboxed, over the source -- the same trust decision as running `go build` on it "+
		"yourself. Approving here only decides whether it is kept and made callable.\n")
	if reqs := loaded.Spec.Requires; len(reqs) > 0 {
		fmt.Fprintf(&b, "\nOnce installed, it wants:\n")
		for _, r := range reqs {
			fmt.Fprintf(&b, "  %s %s", r.Kind, strings.Join(r.Scope, ", "))
			if r.Reason != "" {
				fmt.Fprintf(&b, " -- %s", r.Reason)
			}
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "You will be asked again before any of that is actually granted.\n")
	}
	return b.String()
}

// installSummary reports what Add produced, and whether the current view can
// see it -- installing does not widen the view, so a tool built here can still
// be invisible to the session that just built it.
func installSummary(res *toolkit.AddResult, sel labels.Selector) string {
	verb := "installed"
	if res.Replaced {
		verb = "updated"
	}
	rec := res.Record
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", verb, rec.Spec.Name)
	if v := rec.Spec.Version; v != "" {
		fmt.Fprintf(&b, " %s", v)
	}
	if len(rec.Labels()) > 0 {
		fmt.Fprintf(&b, "  [%s]", strings.Join(rec.Labels(), ", "))
	}
	b.WriteByte('\n')
	if !sel.Matches(rec.Labels()) {
		fmt.Fprintf(&b, "\nThis tool is NOT in the current view, so it cannot be called from this\nsession. Ask the user to add it to the view.\n")
	}
	return b.String()
}

func boolPtr(b bool) *bool { return &b }
