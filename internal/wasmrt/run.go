package wasmrt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// Harvest limits. The describe call runs code the user has not yet agreed to
// run, so it gets the tightest budget forge applies anywhere.
const (
	describeTimeout   = 5 * time.Second
	describeMaxOutput = 256 << 10
)

// Describe runs the module's describe entry point and returns its manifest
// JSON.
//
// It runs with an empty capability set, no filesystem, no environment and a
// five second deadline. That is deliberate: this is the first time the tool's
// code has ever run on the machine, and the user has not yet been shown what it
// wants, let alone agreed to it. The describe call has to be safe on its own.
func (e *Engine) Describe(ctx context.Context, c *Compiled) ([]byte, error) {
	if c.Tier != TierReactor {
		return nil, fmt.Errorf("%w (built without -buildmode=c-shared?)", ErrNoDescribe)
	}

	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()

	// Describing runs guest code, so it gets the same treatment as an
	// invocation: no grants and no services at all. A tool cannot reach the
	// network while forge is still working out what it is.
	inv := &hostabi.Invocation{
		Tool:   "(describing)",
		Grants: capability.Set{},
		Limits: hostabi.DefaultLimits(),
	}
	inv.Prepare()
	defer inv.Release()
	ctx = hostabi.With(ctx, inv)

	stdout := NewCappedBuffer(describeMaxOutput)
	stderr := NewCappedBuffer(describeMaxOutput)
	cfg, err := e.buildModuleConfig(Options{Stdout: stdout, Stderr: stderr}, exportInit)
	if err != nil {
		return nil, err
	}

	mod, err := e.rt.InstantiateModule(ctx, c.cm, cfg)
	if err != nil {
		if code, ok := exitCode(err); ok {
			return nil, fmt.Errorf("module exited with status %d while initialising: %s", code, trim(stderr.Bytes()))
		}
		return nil, fmt.Errorf("initialising module: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	fn := mod.ExportedFunction(exportDescribe)
	if fn == nil {
		return nil, ErrNoDescribe
	}
	res, err := fn.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", exportDescribe, err)
	}
	raw, err := readResult(mod, res, describeMaxOutput)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("the module returned an empty manifest")
	}

	// The SDK reports a registration mistake through this channel rather than
	// returning nothing, so that forge can name the mistake instead of saying
	// only that the description was empty.
	var guestErr struct {
		Error string `json:"forgeError"`
	}
	if err := json.Unmarshal(raw, &guestErr); err == nil && guestErr.Error != "" {
		return nil, fmt.Errorf("the tool reported: %s", guestErr.Error)
	}
	return raw, nil
}

// Request is one call into a tool.
type Request struct {
	Op    string
	Input json.RawMessage
	Opts  Options
}

// Response is a tool's reply.
type Response struct {
	// Output, Text and Bytes carry the result according to the operation's
	// declared output kind; exactly one is set on success.
	Output json.RawMessage
	Text   string
	Bytes  []byte

	// ToolError is set when the tool ran and reported failure. That is not the
	// same as Invoke returning an error, which means forge could not run it.
	ToolError bool
	Error     string

	Stdout, Stderr []byte
	Truncated      bool

	HostCalls int64
	HostBytes int64
	Wall      time.Duration
}

const defaultOutputCap = 16 << 20

// Invoke runs one operation.
func (e *Engine) Invoke(ctx context.Context, c *Compiled, req Request) (Response, error) {
	if c.Tier != TierReactor {
		return Response{}, fmt.Errorf("%w", ErrNoDescribe)
	}

	// One budget across every surface: without this, five servers would each
	// think they were entitled to the whole machine.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}

	if req.Opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Opts.Timeout)
		defer cancel()
	}
	if req.Opts.Limits == (hostabi.Limits{}) {
		req.Opts.Limits = hostabi.DefaultLimits()
	}

	envelope, err := json.Marshal(struct {
		Op    string          `json:"op"`
		Input json.RawMessage `json:"input,omitempty"`
	}{req.Op, req.Input})
	if err != nil {
		return Response{}, fmt.Errorf("encoding the call: %w", err)
	}

	inv := &hostabi.Invocation{
		Call:       envelope,
		Tool:       req.Opts.Tool,
		Grants:     req.Opts.Grants,
		Limits:     req.Opts.Limits,
		OnLog:      req.Opts.OnLog,
		OnProgress: req.Opts.OnProgress,
		Services:   req.Opts.Services,
		CallPath:   req.Opts.CallPath,
		Budget:     req.Opts.Budget,
	}
	inv.Prepare()
	defer inv.Release()
	ctx = hostabi.With(ctx, inv)

	stdout, stderr := NewCappedBuffer(defaultOutputCap), NewCappedBuffer(defaultOutputCap)
	opts := req.Opts
	if opts.Stdout == nil {
		opts.Stdout = stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = stderr
	}
	cfg, err := e.buildModuleConfig(opts, exportInit)
	if err != nil {
		return Response{}, err
	}

	start := time.Now()
	mod, err := e.rt.InstantiateModule(ctx, c.cm, cfg)
	if err != nil {
		return Response{}, wrapRunError(ctx, err, stderr.Bytes())
	}
	defer func() { _ = mod.Close(ctx) }()

	fn := mod.ExportedFunction(exportInvoke)
	if fn == nil {
		return Response{}, fmt.Errorf("module does not export %s", exportInvoke)
	}
	raw, err := fn.Call(ctx)
	if err != nil {
		return Response{}, wrapRunError(ctx, err, stderr.Bytes())
	}
	payload, err := readResult(mod, raw, defaultOutputCap)
	if err != nil {
		return Response{}, err
	}

	var env struct {
		OK     bool            `json:"ok"`
		Output json.RawMessage `json:"output,omitempty"`
		Text   string          `json:"text,omitempty"`
		Bytes  string          `json:"bytes,omitempty"`
		Error  string          `json:"error,omitempty"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return Response{}, fmt.Errorf("the tool returned a result forge could not decode: %w", err)
	}

	resp := Response{
		Output:    env.Output,
		Text:      env.Text,
		ToolError: !env.OK,
		Error:     env.Error,
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		Truncated: stdout.Truncated() || stderr.Truncated() || inv.Truncated(),
		HostCalls: inv.Calls(),
		HostBytes: inv.Bytes(),
		Wall:      time.Since(start),
	}
	if env.Bytes != "" {
		b, err := base64.StdEncoding.DecodeString(env.Bytes)
		if err != nil {
			return Response{}, fmt.Errorf("the tool returned undecodable binary output: %w", err)
		}
		resp.Bytes = b
	}
	return resp, nil
}

// wrapRunError turns a wazero failure into something that names the cause.
//
// A closed module looks the same whether the deadline passed or the guest
// trapped, so the context is consulted rather than the error text.
func wrapRunError(ctx context.Context, err error, stderr []byte) error {
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("the tool exceeded its time limit: %w", ctxErr)
	} else if ctxErr != nil {
		return ctxErr
	}
	if code, ok := exitCode(err); ok {
		return fmt.Errorf("the tool exited with status %d: %s", code, trim(stderr))
	}
	return fmt.Errorf("the tool failed: %w: %s", err, trim(stderr))
}

// readResult unpacks the (pointer, length) an export returns and copies the
// bytes out of guest memory.
func readResult(mod api.Module, stack []uint64, limit int) ([]byte, error) {
	if len(stack) == 0 {
		return nil, errors.New("the module returned no result")
	}
	packed := stack[0]
	if packed == 0 {
		return nil, nil
	}
	ptr := uint32(packed >> 32)
	length := uint32(packed)
	if int(length) > limit {
		return nil, fmt.Errorf("the tool returned %d bytes, over the %d byte limit", length, limit)
	}
	raw, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil, fmt.Errorf("the tool returned a result outside its own memory (pointer %#x, length %d)", ptr, length)
	}
	// Copy before anything can grow linear memory and move the backing array.
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

func trim(b []byte) string {
	const max = 2 << 10
	if len(b) > max {
		b = b[:max]
	}
	return string(b)
}
