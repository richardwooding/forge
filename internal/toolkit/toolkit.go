// Package toolkit is the single façade the surfaces use.
//
// CLI, REPL, MCP, REST and gRPC all talk to this and to nothing below it. That
// is what keeps the five surfaces in step: there is one place where a tool is
// installed, one place where input is normalised, and one place where a result
// is produced, so a surface cannot accidentally do any of it differently.
package toolkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/build"
	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/manifest"
	"github.com/richardwooding/forge/internal/policy"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/view"
	"github.com/richardwooding/forge/internal/wasmrt"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// Config sets up a toolkit.
type Config struct {
	Paths Paths

	// SDKReplace points tool builds at a local SDK checkout. Used when working
	// on forge itself.
	SDKReplace string

	// Offline builds tools without network access.
	Offline bool

	// InvokeTimeout bounds one tool call.
	InvokeTimeout time.Duration

	// Prompter asks the user about capabilities. A nil Prompter denies
	// everything, which is the right default for a server or a script: running
	// forge non-interactively must not silently grant what it would otherwise
	// have asked about.
	Prompter policy.Prompter

	// AllowPrivateNetwork lets tools reach loopback and private addresses.
	// Off by default: the whole point of the SSRF screening is that a tool
	// granted "example.com" cannot be talked into fetching 169.254.169.254.
	// Tests that stand up a local server turn it on deliberately.
	AllowPrivateNetwork bool
}

// Toolkit installs, lists and runs tools.
type Toolkit struct {
	cfg     Config
	store   *store.Store
	builder *build.Builder
	engine  *wasmrt.Engine
	views   *view.Store
	policy  *policy.Policy
	floor   policy.Floor

	// services back the capability host functions. Built once, shared by every
	// invocation, because the rate limiter and the KV lock are only useful if
	// every call goes through the same one.
	services hostabi.Services

	mu    sync.RWMutex
	bound map[string]map[string]*binding.Bound // tool -> op -> bound
}

// New opens the store and prepares the runtime.
func New(ctx context.Context, cfg Config) (*Toolkit, error) {
	if cfg.Paths == (Paths{}) {
		cfg.Paths = DefaultPaths()
	}
	if cfg.InvokeTimeout == 0 {
		cfg.InvokeTimeout = 2 * time.Minute
	}

	st, err := store.Open(cfg.Paths.Data)
	if err != nil {
		return nil, err
	}

	proxy := ""
	if cfg.Offline {
		proxy = "off"
	}
	b := build.New(build.Config{
		StateDir:   filepath.Join(cfg.Paths.Cache, "build"),
		SDKReplace: cfg.SDKReplace,
		Proxy:      proxy,
	})

	e, err := wasmrt.NewEngine(ctx, wasmrt.Config{
		CacheDir: filepath.Join(cfg.Paths.Cache, "wasm"),
	})
	if err != nil {
		return nil, err
	}

	vs, err := view.Open(cfg.Paths.Config)
	if err != nil {
		_ = e.Close(ctx)
		return nil, err
	}
	pol, err := policy.Open(cfg.Paths.Config)
	if err != nil {
		_ = e.Close(ctx)
		return nil, err
	}

	tk := &Toolkit{
		cfg:     cfg,
		store:   st,
		builder: b,
		engine:  e,
		views:   vs,
		policy:  pol,
		// forge's own directories are on the floor: a tool that could write
		// them could grant itself anything on the next run.
		floor: policy.DefaultFloor(cfg.Paths.Data, cfg.Paths.Config, cfg.Paths.Cache),
		bound: map[string]map[string]*binding.Bound{},
	}
	// The invoker closes the loop: it is one of tk's services and it calls
	// back into tk, so it can only be built once tk exists.
	tk.services = buildServices(cfg, invoker{tk: tk})
	return tk, nil
}

// prompterKey carries a per-invocation Prompter.
type prompterKey struct{}

// WithPrompter attaches a Prompter to a context for the duration of one call.
//
// The toolkit's configured Prompter is fixed at construction and shared by
// every surface, which is wrong for any surface where the person to ask is
// reachable only through the connection the request arrived on. An MCP
// session is exactly that: whom to ask is a property of the request, not of
// the process.
func WithPrompter(ctx context.Context, p policy.Prompter) context.Context {
	return context.WithValue(ctx, prompterKey{}, p)
}

// prompterFor returns the prompter to use for this call, preferring one
// carried on the context.
func (tk *Toolkit) prompterFor(ctx context.Context) policy.Prompter {
	if p, ok := ctx.Value(prompterKey{}).(policy.Prompter); ok && p != nil {
		return p
	}
	return tk.cfg.Prompter
}

// Paths reports where this toolkit keeps its data, cache and config.
func (tk *Toolkit) Paths() Paths { return tk.cfg.Paths }

// Views exposes the view store.
func (tk *Toolkit) Views() *view.Store { return tk.views }

// Policy exposes the recorded capability decisions.
func (tk *Toolkit) Policy() *policy.Policy { return tk.policy }

// Floor is the set of things no tool may be granted.
func (tk *Toolkit) Floor() policy.Floor { return tk.floor }

// Close releases the runtime.
func (tk *Toolkit) Close(ctx context.Context) error { return tk.engine.Close(ctx) }

// Store exposes the underlying store for commands that manage it directly.
func (tk *Toolkit) Store() *store.Store { return tk.store }

// GoAvailable reports the Go toolchain version, and whether there is one.
func (tk *Toolkit) GoAvailable(ctx context.Context) (string, bool) {
	return tk.builder.Available(ctx)
}

// AddResult reports what installing a tool produced.
type AddResult struct {
	Record   store.Record
	Replaced bool
	Timings  AddTimings
}

// AddTimings are the per-step durations, shown in the CLI so that a slow
// install is legible rather than mysterious.
type AddTimings struct {
	Build    time.Duration
	Compile  time.Duration
	Describe time.Duration
	Store    time.Duration
}

// AddOption adjusts one call to Add.
type AddOption func(*addOptions)

type addOptions struct {
	approve func(ctx context.Context, loaded *manifest.Loaded) error
}

// WithApprove asks approve once the module has described itself but before
// anything is written to the store. An error aborts the install and leaves no
// trace, the same guarantee Add already gives when a module describes itself
// badly.
//
// CLI's `tool add` passes none: a person who typed the command has already
// approved running it. A surface that installs on someone else's say-so --
// an MCP client acting for a model -- is expected to use this to put a human
// in the loop before anything is committed.
func WithApprove(approve func(ctx context.Context, loaded *manifest.Loaded) error) AddOption {
	return func(o *addOptions) { o.approve = approve }
}

// Add builds a tool from source and installs it.
//
// The steps are: compile the source, compile the wasm, ask the module to
// describe itself with no capabilities at all, validate what it said, and only
// then write anything to the store. Nothing is installed until every step has
// succeeded, so a tool that describes itself badly leaves no trace.
func (tk *Toolkit) Add(ctx context.Context, src string, opts ...AddOption) (*AddResult, error) {
	var o addOptions
	for _, opt := range opts {
		opt(&o)
	}

	var t AddTimings

	start := time.Now()
	art, err := tk.builder.Build(ctx, src)
	if err != nil {
		return nil, err
	}
	t.Build = time.Since(start)

	start = time.Now()
	compiled, err := tk.engine.Compile(ctx, art.Wasm)
	if err != nil {
		return nil, err
	}
	t.Compile = time.Since(start)
	if compiled.Tier != wasmrt.TierReactor {
		return nil, fmt.Errorf("the module does not export forge's entry points; it was probably built without -buildmode=c-shared, or it does not import the forge SDK")
	}

	start = time.Now()
	raw, err := tk.engine.Describe(ctx, compiled)
	if err != nil {
		return nil, err
	}
	loaded, err := manifest.Parse(raw)
	if err != nil {
		return nil, err
	}
	t.Describe = time.Since(start)

	if o.approve != nil {
		if err := o.approve(ctx, loaded); err != nil {
			return nil, err
		}
	}

	start = time.Now()
	existing, getErr := tk.store.Get(loaded.Spec.Name)
	replaced := getErr == nil

	digest, err := tk.store.PutBlob(art.Wasm)
	if err != nil {
		return nil, err
	}
	rec := store.Record{
		Spec:       loaded.Spec,
		WasmDigest: digest,
		Build:      art.Prov,
		Source:     src,
	}
	if replaced {
		// Reinstalling keeps what the user added: their labels and the grants
		// they already agreed to. Discarding either would mean every upgrade
		// re-asked questions they had answered.
		rec.ExtraLabels = existing.ExtraLabels
		rec.Grants = existing.Grants
		rec.Added = existing.Added
	}
	if err := tk.store.Put(rec); err != nil {
		return nil, err
	}
	t.Store = time.Since(start)

	tk.invalidate(rec.Spec.Name)
	return &AddResult{Record: rec, Replaced: replaced, Timings: t}, nil
}

// Remove uninstalls a tool.
func (tk *Toolkit) Remove(name string) error {
	if err := tk.store.Remove(name); err != nil {
		return err
	}
	tk.invalidate(name)
	return nil
}

// List returns the installed tools matching a selector, ordered by name.
func (tk *Toolkit) List(sel labels.Selector) ([]store.Record, error) {
	all, err := tk.store.List()
	if err != nil {
		return nil, err
	}
	if sel == nil {
		sel = labels.All
	}
	var out []store.Record
	for _, r := range all {
		if sel.Matches(r.Labels()) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}

// Get returns one tool's record.
func (tk *Toolkit) Get(name string) (store.Record, error) { return tk.store.Get(name) }

// Bound returns the derived binding for one operation, deriving it on first use
// and caching it.
func (tk *Toolkit) Bound(name, op string) (*binding.Bound, error) {
	tk.mu.RLock()
	if ops, ok := tk.bound[name]; ok {
		if b, ok := ops[op]; ok {
			tk.mu.RUnlock()
			return b, nil
		}
	}
	tk.mu.RUnlock()

	rec, err := tk.store.Get(name)
	if err != nil {
		return nil, err
	}
	loaded, err := manifest.Validate(rec.Spec)
	if err != nil {
		return nil, err
	}
	all, err := binding.BindAll(loaded)
	if err != nil {
		return nil, err
	}

	tk.mu.Lock()
	tk.bound[name] = all
	tk.mu.Unlock()

	b, ok := all[op]
	if !ok {
		return nil, fmt.Errorf("tool %q has no operation %q", name, op)
	}
	return b, nil
}

func (tk *Toolkit) invalidate(name string) {
	tk.mu.Lock()
	delete(tk.bound, name)
	tk.mu.Unlock()
}

// Call is a request to run an operation.
type Call struct {
	Tool string
	Op   string

	// Input is whatever the surface produced; it is normalised here, once, so
	// that every surface validates identically.
	Input json.RawMessage

	OnLog      func(level hostabi.Level, tool, msg string)
	OnProgress func(done, total int64, msg string)

	// attenuate, when set, caps the callee's grants at the caller's. It is set
	// only by the tool-to-tool invoker; a call arriving from a surface has no
	// caller to be attenuated against.
	attenuate *capability.Set

	// callPath is the chain of tools that led here.
	callPath []string

	// budget is the allowance of the call tree this call belongs to. Nil for
	// a call from a surface, which starts a tree of its own.
	budget *hostabi.Budget
}

// Result is what a call produced.
type Result struct {
	Rendition binding.Rendition
	Usage     core.Usage
	Stdout    []byte
	Stderr    []byte

	// Truncated reports that output hit a cap and was cut.
	//
	// Every surface has to pass this on. Presenting the first megabyte of a
	// larger answer as though it were the whole answer is worse than refusing
	// it, because nothing downstream can tell the difference.
	Truncated bool
}

// Invoke runs one operation.
//
// The error it returns is always a *binding.Fault, so every surface can map it
// through the one parity table rather than inventing its own classification.
// A tool that ran and reported failure is not an error here: it comes back in
// the Rendition with ToolError set.
func (tk *Toolkit) Invoke(ctx context.Context, c Call) (*Result, error) {
	rec, err := tk.store.Get(c.Tool)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &binding.Fault{Code: binding.FaultNotFound, Tool: c.Tool,
				Message: fmt.Sprintf("no tool named %q is installed", c.Tool)}
		}
		return nil, internalFault(c.Tool, err)
	}

	if c.Op == "" {
		op, ok := rec.Spec.DefaultOp()
		if !ok {
			var names []string
			for _, o := range rec.Spec.Ops {
				names = append(names, o.Name)
			}
			return nil, &binding.Fault{Code: binding.FaultInvalidInput, Tool: c.Tool,
				Message: fmt.Sprintf("tool %q has several operations; choose one of %v", c.Tool, names)}
		}
		c.Op = op.Name
	}

	b, err := tk.Bound(c.Tool, c.Op)
	if err != nil {
		return nil, &binding.Fault{Code: binding.FaultNotFound, Tool: c.Tool, Op: c.Op, Message: err.Error()}
	}

	input, fault := binding.Normalize(b, c.Input)
	if fault != nil {
		return nil, fault
	}

	grants, err := (&policy.Resolver{
		Policy:   tk.policy,
		Floor:    tk.floor,
		Prompter: tk.prompterFor(ctx),
	}).Resolve(ctx, rec.Spec.Name, rec.Spec.Summary, rec.Spec.Requires)
	if err == nil && c.attenuate != nil {
		grants = capability.Intersect(*c.attenuate, grants)
	}
	if err != nil {
		// A refusal is the user's decision, not a malfunction, so it is
		// reported as an input-level fault rather than an internal one.
		return nil, &binding.Fault{Code: binding.FaultInvalidInput, Tool: c.Tool, Op: c.Op,
			Message: err.Error(), Cause: err}
	}

	wasm, err := tk.store.Blob(rec.WasmDigest)
	if err != nil {
		return nil, internalFault(c.Tool, err)
	}
	compiled, err := tk.engine.Compile(ctx, wasm)
	if err != nil {
		return nil, internalFault(c.Tool, err)
	}

	resp, err := tk.engine.Invoke(ctx, compiled, wasmrt.Request{
		Op:    c.Op,
		Input: input,
		Opts: wasmrt.Options{
			Tool:       c.Tool,
			Grants:     grants,
			Timeout:    tk.cfg.InvokeTimeout,
			OnLog:      c.OnLog,
			OnProgress: c.OnProgress,
			Services:   tk.services,
			CallPath:   c.callPath,
			Budget:     c.budget,
		},
	})
	if err != nil {
		code := binding.FaultInternal
		if errors.Is(err, context.DeadlineExceeded) {
			code = binding.FaultDeadline
		}
		return nil, &binding.Fault{Code: code, Tool: c.Tool, Op: c.Op, Message: err.Error(), Cause: err}
	}

	return &Result{
		Rendition: binding.Rendition{
			Kind:      b.Op.OutputKind,
			JSON:      resp.Output,
			Text:      resp.Text,
			Bytes:     resp.Bytes,
			MediaType: b.Op.OutputMediaType,
			ToolError: resp.ToolError,
			Message:   resp.Error,
		},
		Usage: core.Usage{
			Wall:      int64(resp.Wall),
			HostCalls: int(resp.HostCalls),
			BytesOut:  resp.HostBytes,
		},
		Stdout:    resp.Stdout,
		Stderr:    resp.Stderr,
		Truncated: resp.Truncated,
	}, nil
}

func internalFault(tool string, err error) *binding.Fault {
	return &binding.Fault{Code: binding.FaultInternal, Tool: tool, Message: err.Error(), Cause: err}
}
