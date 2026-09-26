package mcpsrv_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/toolkit"
)

// needySrc declares a capability, so invoking it has to go through the policy
// layer before the module ever runs.
const needySrc = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	URL string ` + "`json:\"url\"`" + `
}

var _ = tool.Register(
	tool.Spec{
		Name:    "needy",
		Summary: "Wants the network",
		Labels:  []string{"demo"},
		Needs: []tool.Need{{
			Kind:   tool.NetHTTP,
			Scope:  []string{"api.example.com"},
			Reason: "fetch the page you ask for",
		}},
	},
	tool.Op("get", get, tool.Text()),
)

func main() {}

func get(ctx *tool.Context, a Args) (string, error) { return "fetched " + a.URL, nil }
`

func needyToolkit(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a wasm tool with the Go toolchain; skipped in -short")
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
			Cache:  filepath.Join(os.TempDir(), "forge-test-mcpcache"),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
		// No Prompter: the process has nobody to ask, exactly as `forge mcp`
		// does, so the session prompter is the only way a human is reached.
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(needySrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Add(ctx, dir); err != nil {
		t.Skipf("cannot build the fixture tool: %v", err)
	}
	return tk
}

func callNeedy(t *testing.T, sess *mcp.ClientSession) (*mcp.CallToolResult, error) {
	t.Helper()
	return sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "needy",
		Arguments: map[string]any{"url": "https://api.example.com/x"},
	})
}

// TestCapabilityIsAskedThroughElicitation is issue #2: before this, a tool
// needing a capability over MCP was refused on the spot, with no prompt shown
// to anyone, and the refusal was recorded permanently.
func TestCapabilityIsAskedThroughElicitation(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	sess := connectWithElicitation(t, srv, func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked = append(asked, req.Params.Message)
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true, "remember": true}}, nil
	})

	res, err := callNeedy(t, sess)
	if err != nil {
		t.Fatalf("call failed after approval: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported an error: %+v", res.Content)
	}
	if len(asked) != 1 {
		t.Fatalf("elicited %d times, want once", len(asked))
	}
	// The prompt has to say what is wanted and why, or it is not a question a
	// person can answer.
	for _, want := range []string{"needy", "net.http", "api.example.com", "fetch the page"} {
		if !strings.Contains(asked[0], want) {
			t.Errorf("prompt does not mention %q:\n%s", want, asked[0])
		}
	}
	if !tk.Policy().Granted("needy").Allow(capability.NetHTTP, "api.example.com").OK {
		t.Error("remember:true did not persist the grant")
	}
}

func TestAcceptWithoutRememberGrantsOnlyThisCall(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)

	var asked int
	sess := connectWithElicitation(t, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked++
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true}}, nil
	})

	for i := range 2 {
		if _, err := callNeedy(t, sess); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if asked != 2 {
		t.Errorf("elicited %d times, want once per call when not remembered", asked)
	}
	if tk.Policy().Granted("needy").Has(capability.NetHTTP) {
		t.Error("a one-off approval was persisted")
	}
}

// TestCancellingIsNotARefusal separates the two things a client can mean.
// Closing a dialog is not answering the question, and recording it as a no
// would disable the tool for good on the strength of a dismissed prompt.
func TestCancellingIsNotARefusal(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)

	sess := connectWithElicitation(t, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "cancel"}, nil
	})

	if _, err := callNeedy(t, sess); err == nil {
		t.Fatal("a cancelled prompt still ran the tool")
	}
	if got := tk.Policy().Tools(); len(got) != 0 {
		t.Errorf("cancelling recorded a decision for %v", got)
	}
}

// TestAcceptingWithAllowFalseIsARefusal: a client may render the question as a
// form rather than a yes/no, so "accepted the dialog, answered no" has to mean
// the same thing as declining it.
func TestAcceptingWithAllowFalseIsARefusal(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)

	sess := connectWithElicitation(t, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": false}}, nil
	})
	if _, err := callNeedy(t, sess); err == nil {
		t.Fatal("allow:false still ran the tool")
	}
	if got := tk.Policy().Tools(); len(got) != 1 {
		t.Errorf("Tools() = %v, want the refusal recorded", got)
	}
}

func TestDecliningIsRemembered(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)

	var asked int
	sess := connectWithElicitation(t, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked++
		return &mcp.ElicitResult{Action: "decline"}, nil
	})

	if _, err := callNeedy(t, sess); err == nil {
		t.Fatal("a declined capability still ran the tool")
	}
	if _, err := callNeedy(t, sess); err == nil {
		t.Fatal("second call ran after a refusal")
	}
	if asked != 1 {
		t.Errorf("asked %d times after a refusal, want once", asked)
	}
}

// TestAClientThatCannotBeAskedDoesNotPoisonTheTool is the half of issue #2
// that was worse than it looked: a session with no elicitation support used to
// record a permanent refusal, killing the tool on every other surface too.
func TestAClientThatCannotBeAskedDoesNotPoisonTheTool(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)

	plain := connect(t, srv) // no ElicitationHandler
	if _, err := callNeedy(t, plain); err == nil {
		t.Fatal("a capability was granted with nobody to ask")
	}
	if got := tk.Policy().Tools(); len(got) != 0 {
		t.Fatalf("a decision was recorded for %v, but nobody was ever asked", got)
	}

	// The same tool must still be approvable afterwards -- here through a
	// second session that can be asked, which stands in for the user running
	// `forge grant allow` in a terminal.
	asking := connectWithElicitation(t, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true, "remember": true}}, nil
	})
	if _, err := callNeedy(t, asking); err != nil {
		t.Fatalf("the tool was left unusable by the earlier non-answer: %v", err)
	}
}

// TestAnOutOfBandGrantIsSeenWithoutRestarting is issue #3 through the surface
// that reported it.
func TestAnOutOfBandGrantIsSeenWithoutRestarting(t *testing.T) {
	tk := needyToolkit(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv) // cannot be asked

	if _, err := callNeedy(t, sess); err == nil {
		t.Fatal("ran without a grant")
	}

	// What `forge grant allow needy` does, from another process.
	other, err := toolkit.New(context.Background(), toolkit.Config{Paths: tk.Paths()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	if err := other.Policy().Grant("needy", []capability.Grant{
		{Kind: capability.NetHTTP, Scope: []string{"api.example.com"}},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := callNeedy(t, sess)
	if err != nil {
		t.Fatalf("the running server did not pick up an out-of-band grant: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool errored: %+v", res.Content)
	}
}
