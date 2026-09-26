package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/policy"
	"github.com/richardwooding/forge/internal/toolkit"
)

// capabilityRequestID names the input request a capability prompt asks for.
// It is distinct from forge_add_tool's, so a client can never confuse an
// approval to install with an approval to grant.
const capabilityRequestID = "capability"

// invokeWithApproval runs a tool, putting a capability question to the person
// behind the client when one comes up.
//
// MCP will not let a server raise an elicitation while it is serving a request
// -- it answers "cannot be sent while serving a request ... return an
// InputRequests map instead (SEP-2322)" -- so the question cannot simply be
// asked from inside the invocation. It is handed back as part of the reply
// instead, and the client calls again with the answer attached. This function
// is therefore entered twice per approval: once to discover the question, and
// once to act on the answer.
//
// Before this existed, the only prompter forge had was the terminal one, and
// `forge mcp` has no terminal -- its stdin is the JSON-RPC pipe. Every
// capability request over MCP was refused on the spot, with no prompt shown to
// anyone, and the refusal was written down, so one call from an agent
// permanently disabled the tool on every surface.
func (m *Manager) invokeWithApproval(ctx context.Context, req *mcp.CallToolRequest, tool, op string) (*mcp.CallToolResult, error) {
	if resp, answered := req.Params.InputResponses[capabilityRequestID]; answered {
		// Replay the answer through the ordinary policy path, so persistence,
		// merging and the floor behave exactly as they do on a terminal rather
		// than being reimplemented here.
		ctx = toolkit.WithPrompter(ctx, policy.Answered(answerFrom(resp)))
	}

	res, err := m.opts.Toolkit.Invoke(ctx, toolkit.Call{Tool: tool, Op: op, Input: req.Params.Arguments})
	if err != nil {
		if needs, ok := errors.AsType[*policy.NeedsApproval](err); ok {
			return askForCapability(needs), nil
		}
		// Anything else is forge failing or refusing, which is a protocol
		// error: the model cannot fix a denied capability by rewording its
		// arguments.
		return nil, err
	}

	if res.Rendition.ToolError {
		// A tool that ran and failed is NOT a protocol error. MCP carries it as
		// a result with IsError so the model can read what went wrong and try
		// something else, which is exactly what should happen.
		return toolErrorResult(res.Rendition.Message), nil
	}
	return renderResult(tool, op, res), nil
}

// askForCapability turns an unanswered prompt into the elicitation the client
// must fulfil before calling again.
func askForCapability(needs *policy.NeedsApproval) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{
			capabilityRequestID: &mcp.ElicitParams{
				Message: capabilityMessage(needs),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"allow": {
							Type:        "boolean",
							Description: "true to let this tool do what it is asking for",
						},
						"remember": {
							Type:        "boolean",
							Description: "true to allow it from now on, instead of only for this one call",
						},
					},
					Required: []string{"allow"},
				},
			},
		},
	}
}

// answerFrom maps an elicitation result onto a policy answer.
//
// Three client actions, and only one of them is a refusal. Declining answers
// the question; cancelling is closing a dialog, which must leave no permanent
// record, or a dismissed prompt would disable the tool for good.
func answerFrom(resp mcp.InputResponse) policy.Answer {
	er, ok := resp.(*mcp.ElicitResult)
	if !ok {
		return policy.Unavailable
	}
	switch er.Action {
	case "accept":
		allow, _ := er.Content["allow"].(bool)
		if !allow {
			return policy.Deny
		}
		if remember, _ := er.Content["remember"].(bool); remember {
			return policy.AllowAlways
		}
		return policy.AllowOnce
	case "decline":
		return policy.Deny
	default:
		return policy.Unavailable
	}
}

// capabilityMessage is what the person is shown.
//
// It names the combination as well as the parts, because the combination is
// what carries the risk: asked separately, reading secrets and reaching the
// network are two easy yeses.
func capabilityMessage(needs *policy.NeedsApproval) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s wants to:\n", needs.Tool)
	for _, r := range needs.Requests {
		fmt.Fprintf(&b, "  %s %s", r.Kind, strings.Join(r.Scope, ", "))
		if r.Reason != "" {
			fmt.Fprintf(&b, " -- %s", r.Reason)
		}
		b.WriteByte('\n')
	}
	if w := combinationWarning(needs.Requests); w != "" {
		fmt.Fprintf(&b, "\n! %s\n", w)
	}
	fmt.Fprintf(&b, "\nSet allow to true to permit this call, and remember to permit it from now on.")
	return b.String()
}

// combinationWarning mirrors the CLI's, so the same pairing reads the same way
// whichever surface is asking.
func combinationWarning(reqs []policy.Request) string {
	var secret, net, write bool
	for _, r := range reqs {
		switch r.Kind {
		case "secret":
			secret = true
		case "net.http":
			net = true
		case "fs.write":
			write = true
		}
	}
	switch {
	case secret && net:
		return "this tool could read your secrets and send them anywhere it is allowed to reach"
	case write && net:
		return "this tool could write files from whatever it downloads"
	}
	return ""
}

func toolErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
