package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/store"
)

// removeRequestID names the one input request forge_remove_tool asks for.
const removeRequestID = "approve_remove"

// addRemoveTool lets an agent uninstall a tool.
//
// Behind the same flag as forge_add_tool, because it is the same class of act:
// both change what forge exposes on every surface, and an agent that may
// install should be able to tidy up after itself rather than leaving a tool it
// regrets for someone to clear by hand.
//
// It asks a human first for the reason install does, and one more besides.
// Removing now drops the capabilities the tool had been granted, and those are
// the part that does not come back: reinstalling restores the tool, but every
// grant has to be given again. The elicitation says which ones are about to
// go, since that is what the person is really deciding about.
func (m *Manager) addRemoveTool(srv *mcp.Server) {
	srv.AddTool(&mcp.Tool{
		Name: "forge_remove_tool",
		Description: "Use to clean up after yourself -- a tool you installed that turned out wrong, " +
			"or one the user no longer wants. Removes it from every surface, the MCP equivalent of " +
			"`forge tool remove`.\n\n" +
			"It also drops the capabilities the tool held, and those are what does not come back: " +
			"the tool can be reinstalled from its source, every grant has to be given again. A " +
			"human approves first, and forge refuses if there is no one to ask.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name": {
					Type:        "string",
					Description: "the tool to uninstall, as `forge ls` names it",
				},
			},
			Required: []string{"name"},
		},
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true)},
	}, m.handleRemove)
}

// handleRemove runs twice: once to describe what would go and ask, and once
// more with the answer.
func (m *Manager) handleRemove(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil || strings.TrimSpace(args.Name) == "" {
		return errorResult(`forge_remove_tool needs a tool name in "name"`), nil //nolint:nilerr // MCP carries tool errors as results
	}
	name := strings.TrimSpace(args.Name)

	rec, err := m.opts.Toolkit.Get(name)
	if err != nil {
		// Not installed is an ordinary answer, not a protocol failure. The
		// store's own message already names the tool.
		return errorResult(err.Error()), nil //nolint:nilerr // MCP carries tool errors as results
	}

	if resp, asked := req.Params.InputResponses[removeRequestID]; asked {
		return m.finishRemove(name, resp)
	}

	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{
			removeRequestID: &mcp.ElicitParams{
				Message: removeMessage(name, rec.Spec.Summary, m.grantedKinds(name)),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"approve": {
							Type:        "boolean",
							Description: "true to uninstall this tool and drop its capabilities",
						},
					},
					Required: []string{"approve"},
				},
			},
		},
	}, nil
}

func (m *Manager) finishRemove(name string, resp mcp.InputResponse) (*mcp.CallToolResult, error) {
	if !approvedRemove(resp) {
		return errorResult(fmt.Sprintf("forge_remove_tool: declined, %s is still installed", name)), nil
	}

	dropped := m.grantedKinds(name)

	if err := m.opts.Toolkit.Remove(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errorResult(err.Error()), nil
		}
		return nil, fmt.Errorf("removing %s: %w", name, err)
	}

	// Every live view server has to drop it, and clients hear about it through
	// the list_changed notification Sync triggers -- the same path an install
	// takes, so a tool appearing and disappearing look alike from outside.
	if err := m.Sync(); err != nil {
		return nil, fmt.Errorf("removed %s but could not refresh the tool list: %w", name, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "removed %s", name)
	if len(dropped) > 0 {
		fmt.Fprintf(&b, ", and dropped %s", strings.Join(dropped, ", "))
		b.WriteString("\nInstalling a tool under this name later will ask about those again.")
	}
	return textResult(b.String()), nil
}

// grantedKinds names what a tool currently holds, for the message.
func (m *Manager) grantedKinds(name string) []string {
	set := m.opts.Toolkit.Policy().Granted(name)
	kinds := set.Kinds()
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		scopes := set.Scopes(k)
		if len(scopes) == 0 {
			out = append(out, string(k))
			continue
		}
		out = append(out, fmt.Sprintf("%s(%s)", k, strings.Join(scopes, ", ")))
	}
	return out
}

func approvedRemove(resp mcp.InputResponse) bool {
	er, ok := resp.(*mcp.ElicitResult)
	if !ok || er.Action != "accept" {
		return false
	}
	approve, _ := er.Content["approve"].(bool)
	return approve
}

// removeMessage is what the person reads. It leads with the capabilities,
// because the tool itself can be reinstalled from its source and the grants
// cannot be got back except by giving them again.
func removeMessage(name, summary string, granted []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An agent wants to uninstall %q from this machine.\n", name)
	if summary != "" {
		fmt.Fprintf(&b, "%s\n", summary)
	}
	if len(granted) > 0 {
		fmt.Fprintf(&b, "\nIt currently holds:\n")
		for _, g := range granted {
			fmt.Fprintf(&b, "  %s\n", g)
		}
		fmt.Fprintf(&b, "\nRemoving drops those. The tool can be installed again from its "+
			"source, but every capability above would have to be granted again.\n")
	} else {
		fmt.Fprintf(&b, "\nIt holds no capabilities, so nothing is lost but the tool itself, "+
			"which can be installed again from its source.\n")
	}
	return b.String()
}
