// Package hostabi implements the "forge" module a guest imports.
//
// The module is built once per engine and shared by every instance; everything
// that varies per call travels in the context.Context wazero threads through to
// each host function. That is what lets one registered module serve many
// concurrent invocations with different grants.
//
// Two rules hold for every function here, and both exist because getting them
// wrong is silent rather than loud:
//
// Bound a length before trusting it, and copy immediately after reading guest
// memory. api.Memory.Read returns a view into the instance's linear memory, and
// any later growth of that memory -- including growth caused by a nested call
// into another instance -- can move the backing array underneath a slice the
// host is still holding.
//
// Never panic. A panic in a host function becomes a wasm trap that unwinds the
// guest with no chance to report anything, so the tool's user would see "the
// module died" in place of the real problem.
package hostabi

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/richardwooding/forge/internal/capability"
)

// ModuleName is the module name a guest imports.
const ModuleName = "forge"

// Level mirrors the SDK's log levels.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "?"
}

// Limits bound what one invocation may consume through the host.
type Limits struct {
	// MaxMessageBytes caps a single log or progress message.
	MaxMessageBytes int32
	// MaxHostCallBytes caps the total moved across the boundary.
	MaxHostCallBytes int64
	// MaxHostCalls caps the number of host calls.
	MaxHostCalls int64
}

// DefaultLimits are deliberately generous for logging and tight on totals: a
// tool should be able to say what it is doing without being able to drown the
// host in it.
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes:  8 << 10,
		MaxHostCallBytes: 64 << 20,
		MaxHostCalls:     100_000,
	}
}

// Invocation is the per-call state a host function needs. It is immutable
// except for the counters, which are atomic because one invocation may make
// host calls from more than one guest goroutine.
type Invocation struct {
	// Call is the envelope staged for the guest to read.
	Call []byte

	Tool   string
	Grants capability.Set
	Limits Limits

	// OnLog and OnProgress may be nil, in which case the report is discarded.
	OnLog      func(level Level, tool, msg string)
	OnProgress func(done, total int64, msg string)

	calls     atomic.Int64
	bytes     atomic.Int64
	callRead  atomic.Bool
	truncated atomic.Bool
}

// Calls and Bytes report what the invocation used, for core.Usage.
func (inv *Invocation) Calls() int64 { return inv.calls.Load() }
func (inv *Invocation) Bytes() int64 { return inv.bytes.Load() }

// Truncated reports whether any host output was dropped against a limit.
func (inv *Invocation) Truncated() bool { return inv.truncated.Load() }

// charge accounts for one host call, reporting whether it may proceed.
func (inv *Invocation) charge(n int64) bool {
	if inv.calls.Add(1) > inv.Limits.MaxHostCalls {
		inv.truncated.Store(true)
		return false
	}
	if inv.bytes.Add(n) > inv.Limits.MaxHostCallBytes {
		inv.truncated.Store(true)
		return false
	}
	return true
}

type invocationKey struct{}

// With attaches an invocation to a context for the duration of one guest call.
func With(ctx context.Context, inv *Invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, inv)
}

// From retrieves the invocation, or nil when a guest calls a host function
// outside one -- which happens if a module calls into the host from its
// package init, during the describe harvest.
func From(ctx context.Context) *Invocation {
	inv, _ := ctx.Value(invocationKey{}).(*Invocation)
	return inv
}

// Instantiate registers the forge module on a runtime. Call it once per engine.
//
// Every function is registered whether or not the current invocation holds the
// capability it needs. Omitting one would turn a policy decision into a link
// error at instantiation -- indistinguishable from an ABI mismatch, arriving
// before any guest code runs, and impossible for the tool to explain or work
// around. Denial belongs at the call, where it can be a value the guest can
// read.
func Instantiate(ctx context.Context, rt wazero.Runtime) error {
	b := rt.NewHostModuleBuilder(ModuleName)

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(callLen),
			nil, []api.ValueType{api.ValueTypeI32}).
		WithResultNames("len").
		Export("call_len")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(callRead),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32}).
		WithParameterNames("ptr", "cap").
		WithResultNames("n").
		Export("call_read")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(hostLog),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, nil).
		WithParameterNames("level", "ptr", "len").
		Export("log")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(hostProgress),
			[]api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32}, nil).
		WithParameterNames("done", "total", "ptr", "len").
		Export("progress")

	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("registering the %q host module: %w", ModuleName, err)
	}
	return nil
}

func callLen(ctx context.Context, _ api.Module, stack []uint64) {
	defer guard()
	inv := From(ctx)
	if inv == nil {
		stack[0] = api.EncodeI32(0)
		return
	}
	stack[0] = api.EncodeI32(int32(len(inv.Call)))
}

func callRead(ctx context.Context, mod api.Module, stack []uint64) {
	defer guard()
	stack[0] = api.EncodeI32(func() int32 {
		inv := From(ctx)
		if inv == nil {
			return 0
		}
		ptr := api.DecodeU32(stack[0])
		capacity := api.DecodeI32(stack[1])
		if capacity < 0 || int(capacity) < len(inv.Call) {
			return -1
		}
		if !mod.Memory().Write(ptr, inv.Call) {
			return -1
		}
		inv.callRead.Store(true)
		return int32(len(inv.Call))
	}())
}

func hostLog(ctx context.Context, mod api.Module, stack []uint64) {
	defer guard()
	inv := From(ctx)
	if inv == nil {
		return
	}
	level := Level(api.DecodeI32(stack[0]))
	msg, ok := readGuest(inv, mod, api.DecodeU32(stack[1]), api.DecodeI32(stack[2]))
	if !ok {
		return
	}
	if inv.OnLog != nil {
		inv.OnLog(level, inv.Tool, string(msg))
	}
}

func hostProgress(ctx context.Context, mod api.Module, stack []uint64) {
	defer guard()
	inv := From(ctx)
	if inv == nil {
		return
	}
	done, total := int64(stack[0]), int64(stack[1])
	msg, ok := readGuest(inv, mod, api.DecodeU32(stack[2]), api.DecodeI32(stack[3]))
	if !ok {
		return
	}
	if inv.OnProgress != nil {
		inv.OnProgress(done, total, string(msg))
	}
}

// readGuest is the only sanctioned way to read guest memory in this package.
//
// It bounds the length before allocating, checks the read succeeded, and copies
// out of linear memory before returning. The copy is not defensive
// over-engineering: the returned slice from Memory.Read aliases the instance's
// memory, and a later grow moves it.
func readGuest(inv *Invocation, mod api.Module, ptr uint32, length int32) ([]byte, bool) {
	if length < 0 {
		return nil, false
	}
	if length > inv.Limits.MaxMessageBytes {
		inv.truncated.Store(true)
		length = inv.Limits.MaxMessageBytes
	}
	if !inv.charge(int64(length)) {
		return nil, false
	}
	if length == 0 {
		return nil, true
	}
	raw, ok := mod.Memory().Read(ptr, uint32(length))
	if !ok {
		return nil, false
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, true
}

// guard converts a panic in a host function into a no-op.
//
// Without it a panic becomes a wasm trap, and the tool's user is told the
// module died rather than what actually went wrong. Recovering here loses the
// host call, which is the lesser harm.
func guard() { _ = recover() }
