// Package mcpsrv exposes forge's tools over the Model Context Protocol.
//
// This is the surface the label view exists for. Fifty installed tools mean
// fifty tool definitions in an agent's context on every single request, whether
// the task needs them or not; a view cuts that to the handful that matter.
//
// The implementation holds one *mcp.Server per view rather than filtering a
// shared one. That is not a stylistic choice: Server.listTools paginates over
// its whole tool map before returning, so a receiving middleware that dropped
// out-of-view tools would produce short or empty pages, and the opaque cursor
// is computed over the unfiltered list, so out-of-view names would leak through
// it. It would look correct until someone exceeded one page.
package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"
)

// Version is reported to clients.
const Version = "0.1.0"

// inlineLimit is the largest result forge will put straight into a reply.
//
// Beyond it, binary output is described rather than inlined. Pushing five
// megabytes of base64 into a model's context is a far worse failure than making
// the caller ask for it another way.
const inlineLimit = 256 << 10

// Options configure a manager.
type Options struct {
	Toolkit *toolkit.Toolkit

	// Views resolves a named view's selector. When set, a server rebuilt by
	// Sync picks up a selector the user has since edited; without it a view's
	// meaning is fixed at the moment its server was first built.
	Views *view.Store

	// MetaTools adds forge_search_tools and forge_describe_tool, which let an
	// agent discover tools outside its view without those tools' schemas being
	// in its context all along.
	MetaTools bool

	// AllowInstall adds forge_add_tool, which compiles Go source and installs
	// it live -- the MCP equivalent of `forge tool add`. It asks the connected
	// human to approve through MCP elicitation before anything is written to
	// the store, and refuses outright when there is no one to ask, the same
	// rule a nil Prompter applies to capability grants.
	//
	// Off by default. This is a bigger grant than any single capability: it
	// runs the Go toolchain, unsandboxed, over whatever source it is given.
	AllowInstall bool
}

// Manager builds and caches one server per view.
type Manager struct {
	opts Options

	mu      sync.Mutex
	servers map[string]*entry
}

type entry struct {
	server *mcp.Server
	// name is the view this server serves, empty for the default. Sync
	// re-resolves it, so editing a view reaches a server already running.
	name     string
	selector labels.Selector
	// names is what the server currently exposes, so a registry change can be
	// applied as a diff rather than by rebuilding.
	names map[string]bool
}

// New returns a manager.
func New(opts Options) *Manager {
	return &Manager{opts: opts, servers: map[string]*entry{}}
}

// Server returns the server for a view, building it on first use.
//
// key is what the caller uses to identify the view -- a name from the URL path,
// or "" for the default.
func (m *Manager) Server(key string, sel labels.Selector) (*mcp.Server, error) {
	if sel == nil {
		sel = labels.All
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.servers[key]; ok {
		return e.server, nil
	}

	title := "forge"
	if key != "" {
		title = "forge (" + key + ")"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "forge",
		Title:   title,
		Version: Version,
	}, nil)

	e := &entry{server: srv, name: key, selector: sel, names: map[string]bool{}}
	m.servers[key] = e

	if err := m.syncLocked(e); err != nil {
		delete(m.servers, key)
		return nil, err
	}
	if m.opts.MetaTools {
		m.addMetaTools(srv, sel)
	}
	if m.opts.AllowInstall {
		m.addInstallTool(srv, sel)
	}
	return srv, nil
}

// Sync brings every live server back in step with the store.
//
// AddTool and RemoveTools each notify clients through
// notifications/tools/list_changed behind a 10ms coalescing timer, so a burst
// of changes becomes one notification and nothing here needs to debounce.
func (m *Manager) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.servers {
		if err := m.syncLocked(e); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) syncLocked(e *entry) error {
	// Re-resolve the view first: `forge view set` in a terminal has to reach a
	// server that is already running, and a stale selector would keep serving
	// the old set however fresh the store is.
	if e.name != "" && m.opts.Views != nil {
		if sel, _, err := m.opts.Views.Resolve(e.name, ""); err == nil {
			e.selector = sel
		} else {
			// The view was deleted. Serving nothing is the safe reading: this
			// surface's view is its only access boundary, so falling back to
			// everything would widen exposure on a deletion.
			e.selector = labels.None
		}
	}

	records, err := m.opts.Toolkit.List(e.selector)
	if err != nil {
		return err
	}

	want := map[string]bool{}
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			name := binding.SurfaceName(rec.Spec.Name, op.Name, len(rec.Spec.Ops) == 1)
			want[name] = true
			if e.names[name] {
				continue
			}
			t, handler, err := m.buildTool(rec, op, name)
			if err != nil {
				// One malformed tool must not stop the others being served.
				continue
			}
			e.server.AddTool(t, handler)
		}
	}

	var gone []string
	for name := range e.names {
		if !want[name] {
			gone = append(gone, name)
		}
	}
	if len(gone) > 0 {
		e.server.RemoveTools(gone...)
	}
	e.names = want
	return nil
}

// buildTool turns one operation into an MCP tool and its handler.
func (m *Manager) buildTool(rec store.Record, op core.OpSpec, name string) (*mcp.Tool, mcp.ToolHandler, error) {
	// Bind now, and discard the result: the value is not needed here, but a
	// tool that cannot be bound must not be advertised and then fail on the
	// first call. Failing at registration keeps it out of the list entirely.
	if _, err := m.opts.Toolkit.Bound(rec.Spec.Name, op.Name); err != nil {
		return nil, nil, err
	}
	// AddTool panics on a nil or non-object input schema, which would take the
	// whole server down for one bad tool. The manifest already rejects that at
	// install time; this is the second line.
	if op.Input == nil || op.Input.Type != "object" {
		return nil, nil, fmt.Errorf("tool %q operation %q has no object input schema", rec.Spec.Name, op.Name)
	}

	t := &mcp.Tool{
		Name:        name,
		Description: description(rec, op),
		// The schema goes in verbatim. forge and the MCP SDK use the same
		// jsonschema package, so there is no conversion here to get wrong --
		// which is the main reason forge standardised on it.
		InputSchema: op.Input,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   op.Annotations.ReadOnly,
			IdempotentHint: op.Annotations.Idempotent,
		},
	}
	if op.Output != nil {
		t.OutputSchema = op.Output
	}
	if op.Annotations.Destructive {
		d := true
		t.Annotations.DestructiveHint = &d
	}
	if op.Annotations.OpenWorld {
		o := true
		t.Annotations.OpenWorldHint = &o
	}

	toolName, opName := rec.Spec.Name, op.Name
	handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return m.invokeWithApproval(ctx, req, toolName, opName)
	}
	return t, handler, nil
}

func description(rec store.Record, op core.OpSpec) string {
	parts := []string{}
	for _, s := range []string{op.Summary, rec.Spec.Summary} {
		if s != "" {
			parts = append(parts, s)
			break
		}
	}
	if rec.Spec.Description != "" {
		parts = append(parts, rec.Spec.Description)
	}
	// Capabilities belong in the description: a model choosing between tools
	// should be able to see that one of them reaches the network.
	if len(rec.Spec.Requires) > 0 {
		var kinds []string
		for _, r := range rec.Spec.Requires {
			kinds = append(kinds, string(r.Kind))
		}
		parts = append(parts, "Requires: "+strings.Join(kinds, ", ")+".")
	}
	return strings.Join(parts, "\n\n")
}

// renderResult maps a rendition onto MCP content blocks.
func renderResult(tool, op string, res *toolkit.Result) *mcp.CallToolResult {
	out := &mcp.CallToolResult{}
	r := res.Rendition

	switch r.Kind {
	case core.OutputText:
		out.Content = []mcp.Content{&mcp.TextContent{Text: r.Text}}

	case core.OutputBytes:
		media := r.MediaType
		if len(r.Bytes) > inlineLimit {
			out.Content = []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("%s returned %d bytes of %s, which is too large to include here.",
					tool, len(r.Bytes), media),
			}}
			break
		}
		switch {
		case strings.HasPrefix(media, "image/"):
			out.Content = []mcp.Content{&mcp.ImageContent{Data: r.Bytes, MIMEType: media}}
		case strings.HasPrefix(media, "audio/"):
			out.Content = []mcp.Content{&mcp.AudioContent{Data: r.Bytes, MIMEType: media}}
		default:
			// MCP has no generic binary block, so an embedded resource is the
			// correct carrier for anything that is not image or audio.
			out.Content = []mcp.Content{&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      fmt.Sprintf("forge://%s/%s/output", tool, op),
					MIMEType: media,
					Blob:     r.Bytes,
				},
			}}
		}

	default:
		var v any
		if len(r.JSON) > 0 {
			if err := json.Unmarshal(r.JSON, &v); err == nil {
				out.StructuredContent = v
			}
		}
		// A text copy as well, because clients that ignore structuredContent
		// would otherwise show the call as having returned nothing.
		out.Content = []mcp.Content{&mcp.TextContent{Text: string(r.JSON)}}
	}

	if len(res.Stderr) > 0 {
		out.Content = append(out.Content, &mcp.TextContent{
			Text: "stderr:\n" + string(res.Stderr),
		})
	}
	return out
}

// StartWatch keeps every live server in step with the store and the views
// until ctx is done.
//
// Without it, a server's tool set only ever changed when forge_add_tool ran
// inside the same process, so `forge tool add` from a terminal was invisible
// to an agent already connected -- with no notification either, because
// nothing called Sync for the SDK to notice. AddTool and RemoveTools fire
// tools/list_changed themselves, so all that was missing was something to
// trigger the diff.
func (m *Manager) StartWatch(ctx context.Context) {
	events := m.opts.Toolkit.Watch(ctx)
	go func() {
		for range events {
			// An error here is not worth ending the watch for: the next event
			// tries again, and a server that stops watching is exactly the
			// failure this exists to prevent.
			_ = m.Sync()
		}
	}()
}
