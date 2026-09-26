package mcpsrv_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/toolkit"
)

const pingSrc = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct{}

var _ = tool.Register(
	tool.Spec{Name: "ping", Summary: "Replies pong", Labels: []string{"demo"}},
	tool.Op("run", run, tool.Text(), tool.ReadOnly()),
)

func main() {}

func run(_ *tool.Context, _ Args) (string, error) { return "pong", nil }
`

const brokenSrc = `package main

import "github.com/richardwooding/forge/sdk/tool"

var _ = tool.Register(
	tool.Spec{Name: "broken"},
	tool.Op("run", doesNotExist, tool.Text()),
)

func main() {}
`

// freshToolkit is fixture without the pre-built greeter and counter: the
// install tests are about what forge_add_tool itself puts in the store, so
// starting from nothing makes an unexpected extra tool impossible to miss.
func freshToolkit(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm tools with the Go toolchain; skipped in -short")
	}
	sdk, err := filepath.Abs(filepath.Join("..", "..", "..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	ctx := context.Background()
	tk, err := toolkit.New(ctx, toolkit.Config{
		Paths: toolkit.Paths{
			Data:   filepath.Join(home, "data"),
			Cache:  filepath.Join(os.TempDir(), forgeTestCache),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })
	return tk
}

// connectWithElicitation is connect plus an elicitation handler, so a test can
// play the part of the human forge_add_tool asks for approval.
func connectWithElicitation(t *testing.T, srv *mcp.Server, handler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()

	go func() {
		if err := srv.Run(ctx, serverT); err != nil {
			t.Logf("server stopped: %v", err)
		}
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ElicitationHandler: handler,
	})
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func approve(ok bool) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": ok}}, nil
	}
}

// TestInstallToolIsOffByDefault is AllowInstall's whole point: a capability
// this size must be opted into, not discovered by an agent that happened to
// call forge_search_tools.
func TestInstallToolIsOffByDefault(t *testing.T) {
	tk := freshToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	for _, name := range listNames(t, sess) {
		if name == "forge_add_tool" {
			t.Fatal("forge_add_tool is present without AllowInstall")
		}
	}
}

// TestInstallToolAsksApprovalAndInstalls is the feature: an agent hands over
// source, a human approves through elicitation, and the tool is live in the
// same session with no restart.
func TestInstallToolAsksApprovalAndInstalls(t *testing.T) {
	tk := freshToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, AllowInstall: true})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	sess := connectWithElicitation(t, srv, approve(true))

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_add_tool",
		Arguments: map[string]any{"source": pingSrc},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		text, _ := res.Content[0].(*mcp.TextContent)
		if text != nil {
			t.Fatalf("install reported an error: %s", text.Text)
		}
		t.Fatalf("install reported an error: %+v", res.Content)
	}

	if _, err := tk.Get("ping"); err != nil {
		t.Fatalf("ping was not written to the store: %v", err)
	}

	names := listNames(t, sess)
	var found bool
	for _, n := range names {
		if n == "ping" {
			found = true
		}
	}
	if !found {
		t.Errorf("ping did not appear live in the same session; got %v", names)
	}

	call, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := call.Content[0].(*mcp.TextContent)
	if text == nil || text.Text != "pong" {
		t.Errorf("calling the freshly installed tool = %+v", call.Content)
	}
}

// TestInstallDeclinedInstallsNothing is the gate doing its job: a human saying
// no must leave no trace, the same guarantee Add already gives a module that
// describes itself badly.
func TestInstallDeclinedInstallsNothing(t *testing.T) {
	tk := freshToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, AllowInstall: true})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	sess := connectWithElicitation(t, srv, approve(false))

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_add_tool",
		Arguments: map[string]any{"source": pingSrc},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a decline was not reported as an error result")
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, "declined") {
		t.Errorf("content = %+v, want it to say the install was declined", res.Content)
	}

	if _, err := tk.Get("ping"); err == nil {
		t.Fatal("ping was installed despite the decline")
	}
}

// TestInstallWithNoOneToAskRefuses is the fail-closed rule that
// toolkit.Config.Prompter already applies to capability grants, extended to
// installation: a client that cannot be asked must never be read as a yes.
func TestInstallWithNoOneToAskRefuses(t *testing.T) {
	tk := freshToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, AllowInstall: true})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv) // no ElicitationHandler: this client cannot be asked

	// The SDK's own multi-round-trip middleware refuses to fulfil the
	// elicitation client-side, so this fails as a call error rather than a
	// tool result -- either way, nothing gets far enough to be installed.
	_, err = sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_add_tool",
		Arguments: map[string]any{"source": pingSrc},
	})
	if err == nil {
		t.Fatal("installing with no one to ask was not refused")
	}

	if _, err := tk.Get("ping"); err == nil {
		t.Fatal("ping was installed with no one able to approve it")
	}
}

// TestInstallBuildFailureIsAResult checks the ordering: a source that does not
// compile must fail before anyone is asked to approve anything, and the
// failure must read as fixable, not as forge having broken.
func TestInstallBuildFailureIsAResult(t *testing.T) {
	tk := freshToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, AllowInstall: true})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	sess := connectWithElicitation(t, srv, func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked = true
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	})

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_add_tool",
		Arguments: map[string]any{"source": brokenSrc},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a build failure was not reported as an error result")
	}
	if asked {
		t.Error("approval was requested for source that never compiled")
	}
}
