package toolkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/hostsvc"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// buildServices assembles what the capability host functions call through.
//
// Every service is constructed whether or not any installed tool wants it:
// they are cheap, and a service that exists but is never granted is much
// easier to reason about than one conjured on demand. What decides whether a
// tool can use one is its grant set, in one place.
func buildServices(cfg Config, invoker hostabi.ToolInvoker) hostabi.Services {
	return hostabi.Services{
		HTTP: hostsvc.NewHTTP(hostsvc.HTTPConfig{
			AllowPrivate: cfg.AllowPrivateNetwork,
		}),
		KV: hostsvc.NewKV(hostsvc.KVConfig{
			Dir: filepath.Join(cfg.Paths.Data, "kv"),
		}),
		Secrets: hostsvc.NewSecrets(hostsvc.SecretsConfig{
			Dir: filepath.Join(cfg.Paths.Config, "secrets"),
		}),
		Invoker: invoker,
	}
}

// Secrets exposes the secret store for the `forge secret` commands.
func (tk *Toolkit) Secrets() *hostsvc.Secrets {
	s, _ := tk.services.Secrets.(*hostsvc.Secrets)
	return s
}

// invoker adapts the toolkit to hostabi.ToolInvoker, so a tool can call
// another tool.
//
// It is a distinct type rather than methods on Toolkit because the wiring is
// circular -- the services a toolkit holds include one that calls back into
// it -- and a small indirection makes that legible instead of surprising.
type invoker struct{ tk *Toolkit }

// Invoke implements hostabi.ToolInvoker.
//
// hostabi has already checked the caller's tool.invoke grant, the depth and
// count budgets, and the call path for a cycle. What is left here is the part
// that needs the registry: resolve the callee, attenuate its grants against
// the caller's, and run it.
func (in invoker) Invoke(ctx context.Context, caller string, callerGrants capability.Set,
	tool, op string, input json.RawMessage) (json.RawMessage, error) {

	res, err := in.tk.Invoke(ctx, Call{
		Tool:  tool,
		Op:    op,
		Input: input,

		// The callee is attenuated against the caller: it can never reach
		// anything the caller could not, whatever its own manifest asks for
		// and whatever the user granted it directly. A tool that can read
		// secrets must not become a way for one that cannot to read them.
		attenuate: &callerGrants,

		// The path grows by the caller, so a cycle is refused on the way down
		// rather than discovered by running out of depth.
		callPath: append(append([]string{}, in.pathOf(ctx)...), caller),

		// The callee draws on the caller's allowance. A fresh budget here
		// would make every limit per-node, so a tree could spend the whole
		// allowance again at each level and fanout would go unbounded.
		budget: in.budgetOf(ctx),
	})
	if err != nil {
		// Hand the callee's own message across, so the calling tool can report
		// something better than "it failed". The fault code is deliberately
		// not propagated: to the caller this is one failed call, not a fault
		// in its own invocation, and letting a callee's NotFound surface as
		// the caller's would make the caller look broken.
		if fault, ok := errors.AsType[*binding.Fault](err); ok {
			return nil, fmt.Errorf("%s: %s", tool, fault.Message)
		}
		return nil, err
	}
	if res.Rendition.ToolError {
		return nil, fmt.Errorf("%s reported a failure", tool)
	}
	return res.Rendition.JSON, nil
}

// pathOf reads the call path of the invocation this call is nested inside.
func (in invoker) pathOf(ctx context.Context) []string {
	if inv := hostabi.From(ctx); inv != nil {
		return inv.CallPath
	}
	return nil
}

// budgetOf reads the allowance of the invocation this call is nested inside.
func (in invoker) budgetOf(ctx context.Context) *hostabi.Budget {
	if inv := hostabi.From(ctx); inv != nil {
		return inv.Budget
	}
	return nil
}
