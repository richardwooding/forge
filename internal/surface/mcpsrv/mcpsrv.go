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

	// MetaTools adds forge_search_tools and forge_describe_tool, which let an
	// agent discover tools outside its view without those tools' schemas being
	// in its context all along.
	MetaTools bool
}

// Manager builds and caches one server per view.
type Manager struct {
	opts Options

	mu      sync.Mutex
	servers map[string]*entry
}

type entry struct {
	server   *mcp.Server
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

	e := &entry{server: srv, selector: sel, names: map[string]bool{}}
	m.servers[key] = e

	if err := m.syncLocked(e); err != nil {
		delete(m.servers, key)
		return nil, err
	}
	if m.opts.MetaTools {
		m.addMetaTools(srv, sel)
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
	records, err := m.opts.Toolkit.List(e.selector)
	if err != nil {
		return err
	}

	want := map[string]bool{}
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			name := toolName(rec.Spec.Name, op.Name, len(rec.Spec.Ops) == 1)
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

// toolName is how a forge operation is named to MCP.
//
// A single-operation tool keeps its own name, because "jsonfmt" reads better
// than "jsonfmt_format" and most tools have one operation. Several operations
// are distinguished by suffix.
func toolName(tool, op string, single bool) string {
	if single {
		return tool
	}
	return tool + "_" + op
}

// buildTool turns one operation into an MCP tool and its handler.
func (m *Manager) buildTool(rec store.Record, op core.OpSpec, name string) (*mcp.Tool, mcp.ToolHandler, error) {
	b, err := m.opts.Toolkit.Bound(rec.Spec.Name, op.Name)
	if err != nil {
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
		return m.invoke(ctx, toolName, opName, b, req.Params.Arguments)
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

// invoke runs a tool and shapes the reply.
func (m *Manager) invoke(ctx context.Context, tool, op string, b *binding.Bound, args json.RawMessage) (*mcp.CallToolResult, error) {
	res, err := m.opts.Toolkit.Invoke(ctx, toolkit.Call{Tool: tool, Op: op, Input: args})
	if err != nil {
		// A fault is forge refusing or failing, which is a protocol-level
		// error: the model cannot fix a denied capability by rewording its
		// arguments.
		return nil, err
	}

	if res.Rendition.ToolError {
		// A tool that ran and failed is NOT a protocol error. MCP carries it as
		// a result with IsError so the model can read what went wrong and try
		// something else, which is exactly what should happen.
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: res.Rendition.Message}},
		}, nil
	}
	return renderResult(tool, op, res, b), nil
}

// renderResult maps a rendition onto MCP content blocks.
func renderResult(tool, op string, res *toolkit.Result, b *binding.Bound) *mcp.CallToolResult {
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
