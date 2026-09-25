package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/store"
)

// addMetaTools gives an agent a way to discover tools outside its view.
//
// Both are read-only and neither changes what the session exposes. That
// restraint is the point. A set_view tool would be more convenient and is
// deliberately absent: it makes the session stateful, which breaks a stateless
// HTTP deployment; it leaves the model reasoning from a tool list already in
// its context that no longer matches reality; and, decisively, the view is the
// only access boundary on a localhost server, so a tool that widens it is a
// privilege-escalation primitive reachable by prompt injection in any other
// tool's output.
//
// Search returns names, summaries and labels but never schemas. The schemas are
// the expensive part, and leaving them out is what makes discovery cheap enough
// to be worth offering at all.
func (m *Manager) addMetaTools(srv *mcp.Server, sel labels.Selector) {
	srv.AddTool(&mcp.Tool{
		Name: "forge_search_tools",
		Description: "Find forge tools that are installed but not in the current view. " +
			"Returns names, summaries and labels only. Use forge_describe_tool to get " +
			"one tool's parameters, and ask the user to widen the view if you need to " +
			"call it.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"query": {Type: "string", Description: "match against tool names, summaries and labels"},
			},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return m.searchTools(sel, args.Query)
	})

	srv.AddTool(&mcp.Tool{
		Name:        "forge_describe_tool",
		Description: "Show one forge tool's operations and input schema, including tools outside the current view.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": {Type: "string", Description: "the tool's name"},
			},
			Required: []string{"name"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil || args.Name == "" {
			// A result with IsError, not a protocol error: the model can fix
			// this by supplying a name, and should see it as the tool talking
			// back rather than as the call having broken.
			return errorResult("forge_describe_tool needs a tool name"), nil //nolint:nilerr // MCP carries tool errors as results
		}
		return m.describeTool(sel, args.Name)
	})
}

func (m *Manager) searchTools(inView labels.Selector, query string) (*mcp.CallToolResult, error) {
	all, err := m.opts.Toolkit.List(labels.All)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(query)

	type row struct {
		Name    string   `json:"name"`
		Summary string   `json:"summary,omitempty"`
		Labels  []string `json:"labels,omitempty"`
		InView  bool     `json:"inView"`
	}
	var rows []row
	for _, rec := range all {
		if query != "" && !matches(rec, query) {
			continue
		}
		rows = append(rows, row{
			Name:    rec.Spec.Name,
			Summary: rec.Spec.Summary,
			Labels:  rec.Labels(),
			InView:  inView.Matches(rec.Labels()),
		})
	}
	if len(rows) == 0 {
		return textResult("no tools match that search"), nil
	}

	var b strings.Builder
	for _, r := range rows {
		mark := ""
		if !r.InView {
			mark = "  (not in the current view)"
		}
		fmt.Fprintf(&b, "%s — %s [%s]%s\n", r.Name, r.Summary, strings.Join(r.Labels, ", "), mark)
	}
	res := textResult(strings.TrimRight(b.String(), "\n"))
	res.StructuredContent = map[string]any{"tools": rows}
	return res, nil
}

func matches(rec store.Record, query string) bool {
	if strings.Contains(strings.ToLower(rec.Spec.Name), query) ||
		strings.Contains(strings.ToLower(rec.Spec.Summary), query) {
		return true
	}
	for _, l := range rec.Labels() {
		if strings.Contains(strings.ToLower(l), query) {
			return true
		}
	}
	return false
}

func (m *Manager) describeTool(inView labels.Selector, name string) (*mcp.CallToolResult, error) {
	rec, err := m.opts.Toolkit.Get(name)
	if err != nil {
		// Likewise a result: the model asked about a tool that is not there,
		// which it can recover from by searching instead.
		return errorResult(fmt.Sprintf("no tool named %q is installed", name)), nil //nolint:nilerr // MCP carries tool errors as results
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s", rec.Spec.Name)
	if rec.Spec.Version != "" {
		fmt.Fprintf(&b, " %s", rec.Spec.Version)
	}
	fmt.Fprintf(&b, "\n%s\n\nlabels: %s\n", rec.Spec.Summary, strings.Join(rec.Labels(), ", "))
	if !inView.Matches(rec.Labels()) {
		// Say so plainly rather than letting a model discover it by calling and
		// failing.
		fmt.Fprintf(&b, "\nThis tool is NOT in the current view, so it cannot be called from this\nsession. Ask the user to add it to the view.\n")
	}
	if len(rec.Spec.Requires) > 0 {
		fmt.Fprintf(&b, "\nrequires:\n")
		for _, r := range rec.Spec.Requires {
			fmt.Fprintf(&b, "  %s %s", r.Kind, strings.Join(r.Scope, ", "))
			if r.Reason != "" {
				fmt.Fprintf(&b, " — %s", r.Reason)
			}
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "\noperations:\n")
	for _, op := range rec.Spec.Ops {
		schema, _ := json.Marshal(op.Input)
		fmt.Fprintf(&b, "  %s — %s (%s)\n    input: %s\n", op.Name, op.Summary, op.OutputKind, schema)
	}
	return textResult(b.String()), nil
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
