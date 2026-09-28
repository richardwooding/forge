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
	}, &mcp.ServerOptions{Instructions: instructions(key)})

	e := &entry{server: srv, name: key, selector: sel, names: map[string]bool{}}
	m.servers[key] = e

	if err := m.syncLocked(e); err != nil {
		delete(m.servers, key)
		return nil, err
	}
	if m.opts.MetaTools {
		m.addMetaTools(srv, sel)
		// A meta-tool like the others, so it lives behind the same flag: a
		// server told to expose no meta-tools must expose none, or an unknown
		// view stops serving nothing and starts confirming that forge is
		// there.
		//
		// It is not behind AllowInstall, though, because nothing is compiled
		// or run and the content cannot be chosen by the caller -- only where
		// it lands, which is what the human is asked about.
		m.addSkillTool(srv)
	}

	if m.opts.AllowInstall {
		m.addInstallTool(srv, sel)
		// Uninstalling sits behind the same flag: it is the same class of act,
		// and an agent allowed to install should be able to undo it rather
		// than leaving the clearing up to someone at a terminal.
		m.addRemoveTool(srv)
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

// instructions are sent once, on initialize, and are the only place forge can
// say something about its tools as a whole rather than one at a time.
//
// It says when NOT to reach for a tool as well as when to, and that is not
// padding. Measured over a day's work, a model with these tools available and
// a note telling it to prefer them used one about eight times in a thousand
// calls -- and the ones it did use were all things it could not do itself.
// Exhortation is not the missing piece; naming the moments is, and so is
// admitting that a tool which only reshapes data already in front of the
// caller is competing with the caller and losing.
func instructions(view string) string {
	var b strings.Builder
	b.WriteString("forge runs these tools in a WebAssembly sandbox, each declaring what it may reach.\n\n")
	b.WriteString("Prefer one over a shell pipeline when the work:\n")
	b.WriteString("  - must be exact -- a hash, a checksum, a canonical form. Do not hand-roll or eyeball these.\n")
	b.WriteString("  - reaches out -- fetching a URL, calling an API.\n")
	b.WriteString("  - must be remembered -- anything that has to survive between runs or sessions.\n\n")
	b.WriteString("Do not reach for one to reshape data you can already see; reading it yourself is cheaper.\n\n")
	b.WriteString("Writing the same one-liner a second time is the signal to make a tool: forge_add_tool " +
		"takes a single Go file and it is live on every surface at once.\n")
	if view != "" {
		fmt.Fprintf(&b, "\nThis connection serves the %q view. forge_search_tools finds installed tools it hides.\n", view)
	} else {
		b.WriteString("\nforge_search_tools finds installed tools the current view hides.\n")
	}
	return b.String()
}

// description is what a model reads when choosing between this tool and
// something else -- including, usually, a shell pipeline. It is the most
// valuable text in the system and it used to throw most of itself away.
//
// Three things it gets right that the first version did not:
//
// A tool's own summary and the operation's answer different questions ("what
// is this thing" and "what does this one do"), so both appear. Taking the
// first non-empty of the two meant Spec.Summary was never shown at all for any
// tool with more than one operation.
//
// The long Description is included only for a single-operation tool. It used
// to be repeated verbatim on every operation, so a five-op tool put five
// identical copies in the caller's context. The full text is still one
// forge_describe_tool call away, and moving it there costs nothing but makes
// room for text that actually helps a choice.
//
// Labels are here because they are the cheapest trigger words a tool has --
// "hash", "json", "watch" -- and they were reaching a model only through a
// meta-tool it had to think to call.
func description(rec store.Record, op core.OpSpec) string {
	parts := []string{}

	if op.Summary != "" {
		parts = append(parts, op.Summary)
	}
	if s := rec.Spec.Summary; s != "" && s != op.Summary {
		parts = append(parts, s)
	}

	// UseWhen is the only field that speaks to the decision rather than the
	// behaviour, so it goes near the top where a skim will reach it.
	if rec.Spec.UseWhen != "" {
		parts = append(parts, "Use when: "+rec.Spec.UseWhen)
	}

	if len(rec.Spec.Ops) == 1 && rec.Spec.Description != "" {
		parts = append(parts, rec.Spec.Description)
	}

	if labels := rec.Labels(); len(labels) > 0 {
		parts = append(parts, "Labels: "+strings.Join(labels, ", ")+".")
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
		if len(r.JSON) > 0 && json.Valid(r.JSON) {
			// The raw bytes, not a decoded value. Unmarshalling into an `any`
			// turns every number into a float64, which silently rounded
			// anything past 2^53 on its way back out -- the call succeeded and
			// the number was wrong. json.RawMessage marshals verbatim, so the
			// digits the tool produced are the digits the client receives.
			out.StructuredContent = r.JSON
		}
		// A text copy as well, because clients that ignore structuredContent
		// would otherwise show the call as having returned nothing.
		out.Content = []mcp.Content{&mcp.TextContent{Text: string(r.JSON)}}
	}

	if res.Truncated {
		// Said plainly, because a model reasoning over a cut-off answer as
		// though it were complete is exactly the failure to avoid.
		out.Content = append(out.Content, &mcp.TextContent{
			Text: "note: this output was truncated because it exceeded forge's size limit.",
		})
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
