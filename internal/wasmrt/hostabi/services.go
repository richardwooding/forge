package hostabi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/richardwooding/forge/internal/capability"
)

// HTTPRequest is what a guest asks for.
type HTTPRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// HTTPResponse is what it gets back.
type HTTPResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`

	// Truncated reports that the body hit forge's size limit and was cut, so
	// a tool does not parse half a document as though it were whole.
	Truncated bool `json:"truncated,omitempty"`
}

// HTTPService performs a request on the host's behalf.
//
// The implementation is responsible for everything a guest cannot be trusted
// to do for itself: resolving the name, checking every resolved address, and
// dialling the address it checked rather than the name.
type HTTPService interface {
	Do(ctx context.Context, tool string, grants capability.Set, req HTTPRequest) (HTTPResponse, error)
}

// KVStore is a tool's own small key/value space.
type KVStore interface {
	Get(ctx context.Context, tool, namespace, key string) ([]byte, bool, error)
	Set(ctx context.Context, tool, namespace, key string, value []byte) error
	Delete(ctx context.Context, tool, namespace, key string) error
	List(ctx context.Context, tool, namespace, prefix string) ([]string, error)
}

// SecretSource resolves a named secret.
type SecretSource interface {
	Secret(ctx context.Context, tool, name string) (string, bool, error)
}

// ToolInvoker runs another tool on a guest's behalf.
type ToolInvoker interface {
	Invoke(ctx context.Context, caller string, callerGrants capability.Set,
		tool, op string, input json.RawMessage) (json.RawMessage, error)
}

// deny builds a refusal, carrying the reason so a guest can act on it: a tool
// that can see it was refused the network can fall back rather than merely
// failing.
// deny reports a refusal as structured data rather than prose.
//
// The code is what lets a tool tell "you were never granted this" apart from
// "this is never allowed" and "you have used up your allowance" -- three
// refusals with three different things a user should do about them. Sending
// only a sentence forces the guest to guess, and a tool that guesses wrong
// tells the user to fix something that was never broken.
func deny(code capability.DenyCode, format string, args ...any) (Status, []byte) {
	body, err := json.Marshal(denial{Code: string(code), Detail: fmt.Sprintf(format, args...)})
	if err != nil {
		return StatusError, []byte("cannot encode the denial")
	}
	return StatusDenied, body
}

// denial is the wire form of a refusal. sdk/tool mirrors it.
type denial struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func failed(format string, args ...any) (Status, []byte) {
	return StatusError, []byte(fmt.Sprintf(format, args...))
}

func ok(v any) (Status, []byte) {
	raw, err := json.Marshal(v)
	if err != nil {
		return failed("cannot encode the result: %v", err)
	}
	return StatusOK, raw
}

// ---------------------------------------------------------------- http

func (m *hostModule) httpCall(ctx context.Context, inv *Invocation, raw []byte) (Status, []byte) {
	if inv.Services.HTTP == nil {
		// Not configured is not the same as refused, and saying so keeps a
		// tool from reporting to its user that they said no when they were
		// never asked.
		return failed("this forge has no HTTP service configured")
	}

	var req HTTPRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return failed("cannot read the request: %v", err)
	}
	if !spend(&inv.Budget.httpCalls, inv.Limits.MaxHTTPRequests) {
		return deny(capability.DenyBudget, "this request has made its %d allowed HTTP requests", inv.Limits.MaxHTTPRequests)
	}

	res, err := inv.Services.HTTP.Do(ctx, inv.Tool, inv.Grants, req)
	if err != nil {
		if d, isDenial := capability.AsDenial(err); isDenial {
			return deny(d.Code, "%s", d.Detail)
		}
		return failed("%v", err)
	}
	return ok(res)
}

// ---------------------------------------------------------------- kv

type kvRequest struct {
	Op        string `json:"op"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key,omitempty"`
	Value     []byte `json:"value,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
}

type kvResponse struct {
	Found bool     `json:"found,omitempty"`
	Value []byte   `json:"value,omitempty"`
	Keys  []string `json:"keys,omitempty"`
}

func (m *hostModule) kvCall(ctx context.Context, inv *Invocation, raw []byte) (Status, []byte) {
	if inv.Services.KV == nil {
		return failed("this forge has no key/value store configured")
	}
	var req kvRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return failed("cannot read the request: %v", err)
	}

	ns := req.Namespace
	if ns == "" {
		ns = "default"
	}
	if d := inv.Grants.Allow(capability.KV, ns); !d.OK {
		return deny(d.Code, "%s", d.Detail)
	}
	if req.Op != "get" && req.Op != "list" {
		if int64(len(req.Value)) > inv.Limits.MaxKVValueBytes {
			return deny(capability.DenyBudget, "value is %d bytes; the limit is %d", len(req.Value), inv.Limits.MaxKVValueBytes)
		}
	}

	switch req.Op {
	case "get":
		v, found, err := inv.Services.KV.Get(ctx, inv.Tool, ns, req.Key)
		if err != nil {
			return failed("%v", err)
		}
		return ok(kvResponse{Found: found, Value: v})
	case "set":
		if err := inv.Services.KV.Set(ctx, inv.Tool, ns, req.Key, req.Value); err != nil {
			return failed("%v", err)
		}
		return ok(kvResponse{})
	case "delete":
		if err := inv.Services.KV.Delete(ctx, inv.Tool, ns, req.Key); err != nil {
			return failed("%v", err)
		}
		return ok(kvResponse{})
	case "list":
		keys, err := inv.Services.KV.List(ctx, inv.Tool, ns, req.Prefix)
		if err != nil {
			return failed("%v", err)
		}
		return ok(kvResponse{Keys: keys})
	default:
		return failed("unknown kv operation %q", req.Op)
	}
}

// ---------------------------------------------------------------- secret

type secretRequest struct {
	Name string `json:"name"`
}

type secretResponse struct {
	Found bool   `json:"found"`
	Value string `json:"value,omitempty"`
}

func (m *hostModule) secretCall(ctx context.Context, inv *Invocation, raw []byte) (Status, []byte) {
	if inv.Services.Secrets == nil {
		return failed("this forge has no secret source configured")
	}
	var req secretRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return failed("cannot read the request: %v", err)
	}
	if d := inv.Grants.Allow(capability.Secret, req.Name); !d.OK {
		return deny(d.Code, "%s", d.Detail)
	}

	v, found, err := inv.Services.Secrets.Secret(ctx, inv.Tool, req.Name)
	if err != nil {
		return failed("%v", err)
	}
	// A missing secret is reported as missing rather than as empty: a tool
	// that silently used "" as an API key would fail somewhere far away from
	// the cause.
	return ok(secretResponse{Found: found, Value: v})
}

// ---------------------------------------------------------------- invoke

type invokeRequest struct {
	Tool  string          `json:"tool"`
	Op    string          `json:"op,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type invokeResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
}

func (m *hostModule) invokeCall(ctx context.Context, inv *Invocation, raw []byte) (Status, []byte) {
	if inv.Services.Invoker == nil {
		return failed("this forge has no tool invoker configured")
	}
	var req invokeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return failed("cannot read the request: %v", err)
	}
	if d := inv.Grants.Allow(capability.Invoke, req.Tool); !d.OK {
		return deny(d.Code, "%s", d.Detail)
	}

	// Depth and total are budgeted across the whole call tree rather than per
	// node, because fanout multiplies: eight tools each calling eight is
	// sixty-four whatever the depth limit says.
	if !spend(&inv.Budget.invokes, inv.Limits.MaxInvokes) {
		return deny(capability.DenyBudget, "this request has made its %d allowed tool calls", inv.Limits.MaxInvokes)
	}
	if len(inv.CallPath) >= inv.Limits.MaxInvokeDepth {
		return deny(capability.DenyBudget, "tool calls are nested %d deep, which is the limit", len(inv.CallPath))
	}

	// The chain includes this tool, not just the ones above it, so a tool
	// calling itself is refused here rather than one level further down.
	chain := append(append([]string{}, inv.CallPath...), inv.Tool)
	if slices.Contains(chain, req.Tool) {
		return deny(capability.DenyOutOfScope, "%s is already running in this chain: %s",
			req.Tool, strings.Join(append(chain, req.Tool), " → "))
	}

	out, err := inv.Services.Invoker.Invoke(ctx, inv.Tool, inv.Grants, req.Tool, req.Op, req.Input)
	if err != nil {
		if d, isDenial := capability.AsDenial(err); isDenial {
			return deny(d.Code, "%s", d.Detail)
		}
		return failed("%v", err)
	}
	return ok(invokeResponse{Output: out})
}
