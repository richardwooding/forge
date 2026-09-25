package wasmrt_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/manifest"
	"github.com/richardwooding/forge/internal/wasmrt"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

func newEngine(t *testing.T) (*wasmrt.Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	e, err := wasmrt.NewEngine(ctx, wasmrt.Config{CacheDir: cacheDir(t)})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e, ctx
}

func compileGreeter(t *testing.T) (*wasmrt.Engine, context.Context, *wasmrt.Compiled) {
	t.Helper()
	wasm := buildFixture(t, "greeter")
	e, ctx := newEngine(t)
	c, err := e.Compile(ctx, wasm)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return e, ctx, c
}

// TestDescribeProducesAManifestForgeAccepts is the contract test between the
// SDK and forge. The SDK is a separate module that cannot import forge's types,
// so the only thing holding the two halves together is the JSON -- and this is
// what asserts they still agree.
func TestDescribeProducesAManifestForgeAccepts(t *testing.T) {
	e, ctx, c := compileGreeter(t)

	if c.Tier != wasmrt.TierReactor {
		t.Fatalf("tier = %s, want %s", c.Tier, wasmrt.TierReactor)
	}

	raw, err := e.Describe(ctx, c)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	t.Logf("manifest: %s", raw)

	l, err := manifest.Parse(raw)
	if err != nil {
		t.Fatalf("forge rejected the SDK's own manifest: %v", err)
	}
	if l.Spec.Name != "greeter" || l.Spec.Version != "1.0.0" {
		t.Errorf("spec = %+v", l.Spec)
	}
	if len(l.Spec.Ops) != 6 {
		t.Errorf("got %d operations, want 6", len(l.Spec.Ops))
	}
	greet, ok := l.Spec.Op("greet")
	if !ok {
		t.Fatal("no greet operation")
	}
	if !greet.Annotations.ReadOnly {
		t.Error("the ReadOnly annotation did not survive")
	}
	if greet.Input == nil || greet.Input.Type != "object" {
		t.Errorf("greet input schema = %+v, want an object", greet.Input)
	}
	if _, ok := greet.Input.Properties["name"]; !ok {
		t.Errorf("greet input has no name property: %+v", greet.Input.Properties)
	}
	echo, _ := l.Spec.Op("echo")
	if echo.OutputKind != "text" {
		t.Errorf("echo outputKind = %q, want text", echo.OutputKind)
	}
	// A text operation has no output schema, and the manifest must omit the
	// key rather than carry "output": null. A nil *jsonschema.Schema held in an
	// `any` is not an empty interface, so omitempty does not drop it -- and the
	// stray null would travel into the canonical bytes every surface is
	// compared against.
	if echo.Output != nil {
		t.Errorf("echo carries an output schema: %+v", echo.Output)
	}
	if strings.Contains(string(raw), `"output":null`) {
		t.Error(`manifest contains "output":null`)
	}
}

// TestDescribeRunsWithNoCapabilities pins the security property: the first time
// a stranger's code runs on this machine, it gets nothing.
func TestDescribeRunsWithNoCapabilities(t *testing.T) {
	e, ctx, c := compileGreeter(t)
	if _, err := e.Describe(ctx, c); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	// The fixture has no filesystem or network grant and describes fine. The
	// stronger statement -- that a hostile describe cannot reach out -- is
	// covered by rootfs's own hostile-guest test; what matters here is that
	// Describe never assembles a grant set at all.
}

func TestInvokeJSONOperation(t *testing.T) {
	e, ctx, c := compileGreeter(t)

	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "greet",
		Input: json.RawMessage(`{"name":"world","times":2,"loud":true}`),
		Opts:  wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.ToolError {
		t.Fatalf("tool reported an error: %s", resp.Error)
	}
	var got struct {
		Greeting string `json:"greeting"`
		Length   int    `json:"length"`
	}
	if err := json.Unmarshal(resp.Output, &got); err != nil {
		t.Fatal(err)
	}
	if got.Greeting != "HELLO, WORLD! HELLO, WORLD!" {
		t.Errorf("greeting = %q", got.Greeting)
	}
	if got.Length != len(got.Greeting) {
		t.Errorf("length = %d, want %d", got.Length, len(got.Greeting))
	}
}

func TestInvokeTextOperation(t *testing.T) {
	e, ctx, c := compileGreeter(t)
	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "echo",
		Input: json.RawMessage(`{"text":"a,b,c 🌍"}`),
		Opts:  wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Text != "a,b,c 🌍" {
		t.Errorf("text = %q", resp.Text)
	}
}

// TestLargeIntegerSurvivesTheGuestBoundary completes the 2^53 story: binding
// keeps precision through the flag layer, and this shows it survives the wasm
// round trip too, so the guarantee holds end to end rather than in one package.
func TestLargeIntegerSurvivesTheGuestBoundary(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1
	e, ctx, c := compileGreeter(t)

	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "big",
		Input: json.RawMessage(`{"n":` + big + `}`),
		Opts:  wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(string(resp.Output), big) {
		t.Errorf("output = %s, want it to contain %s exactly", resp.Output, big)
	}
}

// TestHandlerFailureIsAResultNotAnError is the distinction the whole parity
// table turns on. A tool that ran and failed must come back as a Response, not
// as an error from Invoke.
func TestHandlerFailureIsAResultNotAnError(t *testing.T) {
	e, ctx, c := compileGreeter(t)
	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "fail",
		Input: json.RawMessage(`{"text":"because"}`),
		Opts:  wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("a failing handler must not fail the invocation: %v", err)
	}
	if !resp.ToolError {
		t.Error("ToolError not set")
	}
	if !strings.Contains(resp.Error, "because") {
		t.Errorf("error = %q, want the handler's own message", resp.Error)
	}
}

// TestPanicInAHandlerBecomesAToolError matters because the alternative is a
// wasm trap: the user would be told the module died rather than what the panic
// said.
func TestPanicInAHandlerBecomesAToolError(t *testing.T) {
	e, ctx, c := compileGreeter(t)
	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "boom",
		Input: json.RawMessage(`{"text":"kaboom"}`),
		Opts:  wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("a panicking handler should still return a result: %v", err)
	}
	if !resp.ToolError {
		t.Error("ToolError not set")
	}
	if !strings.Contains(resp.Error, "kaboom") {
		t.Errorf("error = %q, want the panic message", resp.Error)
	}
}

func TestLogsAndProgressReachTheHost(t *testing.T) {
	e, ctx, c := compileGreeter(t)

	var mu sync.Mutex
	var logs []string
	var progress []string
	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "chatty",
		Input: json.RawMessage(`{"text":"work"}`),
		Opts: wasmrt.Options{
			Tool:    "greeter",
			Timeout: 10 * time.Second,
			OnLog: func(level hostabi.Level, tool, msg string) {
				mu.Lock()
				defer mu.Unlock()
				logs = append(logs, level.String()+":"+msg)
			},
			OnProgress: func(done, total int64, msg string) {
				mu.Lock()
				defer mu.Unlock()
				progress = append(progress, msg)
			},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Text != "done" {
		t.Errorf("text = %q", resp.Text)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], `info:starting work on "work"`) {
		t.Errorf("logs = %v", logs)
	}
	if len(progress) != 3 || progress[2] != "step 3" {
		t.Errorf("progress = %v", progress)
	}
	// A tool's own stdout must be captured, never inherited: on the MCP stdio
	// surface anything the tool prints would otherwise corrupt the JSON-RPC
	// stream.
	if !strings.Contains(string(resp.Stdout), "this went to stdout") {
		t.Errorf("stdout = %q, want the tool's print captured", resp.Stdout)
	}
	if resp.HostCalls < 5 {
		t.Errorf("HostCalls = %d, want at least the 5 log and progress calls", resp.HostCalls)
	}
}

func TestEachInvocationStartsWithFreshState(t *testing.T) {
	// Instances are not pooled by default, so one call cannot observe another's
	// leftovers. This is what makes the default safe for tools that were never
	// written with reuse in mind.
	e, ctx, c := compileGreeter(t)
	for i := range 3 {
		resp, err := e.Invoke(ctx, c, wasmrt.Request{
			Op:    "greet",
			Input: json.RawMessage(`{"name":"x"}`),
			Opts:  wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
		})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !strings.Contains(string(resp.Output), "Hello, x!") {
			t.Fatalf("call %d: %s", i, resp.Output)
		}
	}
}

func TestConcurrentInvocationsDoNotCollide(t *testing.T) {
	// The default module name comes from the binary's name section, so without
	// WithName("") two simultaneous instances of one tool fail with "module has
	// already been instantiated".
	e, ctx, c := compileGreeter(t)

	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := e.Invoke(ctx, c, wasmrt.Request{
				Op:    "greet",
				Input: json.RawMessage(`{"name":"x"}`),
				Opts:  wasmrt.Options{Tool: "greeter", Timeout: 20 * time.Second},
			})
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent invocation failed: %v", err)
		}
	}
}

func TestUnknownOperationIsReportedByTheGuest(t *testing.T) {
	e, ctx, c := compileGreeter(t)
	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:   "nosuchop",
		Opts: wasmrt.Options{Tool: "greeter", Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.ToolError || !strings.Contains(resp.Error, "nosuchop") {
		t.Errorf("resp = %+v", resp)
	}
}

func TestCompileIsCachedByDigest(t *testing.T) {
	wasm := buildFixture(t, "greeter")
	e, ctx := newEngine(t)

	a, err := e.Compile(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Compile(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("the same bytes compiled twice; the digest cache is not working")
	}
	if len(a.Digest) != 64 {
		t.Errorf("digest = %q, want a sha256 hex string", a.Digest)
	}
}

func TestGrantsAreNotAssembledWithoutCapabilities(t *testing.T) {
	// A zero Options grants nothing: no env, no mounts, no wall clock.
	e, ctx, c := compileGreeter(t)
	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "echo",
		Input: json.RawMessage(`{"text":"x"}`),
		Opts:  wasmrt.Options{Tool: "greeter", Grants: capability.Set{}, Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("a tool needing nothing should run with nothing granted: %v", err)
	}
	if resp.Text != "x" {
		t.Errorf("text = %q", resp.Text)
	}
}
