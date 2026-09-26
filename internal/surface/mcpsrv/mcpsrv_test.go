package mcpsrv_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/toolkit"
)

// forgeTestCache is shared by every test package that builds a wasm tool.
//
// One directory rather than one per package because the first wasip1 build on
// a cold cache compiles the whole standard library for that target, and a
// cache per package pays that cost once per package -- which on CI was most
// of the run. Go's build cache is content-addressed, so sharing it is what it
// is for, not a shortcut.
const forgeTestCache = "forge-test-cache"

const greeterSrc = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	Name string ` + "`json:\"name\" jsonschema:\"who to greet\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "greeter", Summary: "Greets someone", Labels: []string{"demo", "text"}},
	tool.Op("greet", greet, tool.Text(), tool.ReadOnly()),
	tool.Op("fail", fail, tool.Text()),
)

func main() {}

func greet(ctx *tool.Context, a Args) (string, error) { return "Hello, " + a.Name + "!", nil }
func fail(ctx *tool.Context, a Args) (string, error)  { return "", errBad }

var errBad = errorString("the tool decided to fail")

type errorString string

func (e errorString) Error() string { return string(e) }
`

const counterSrc = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	N int ` + "`json:\"n\"`" + `
}

type Out struct {
	Doubled int ` + "`json:\"doubled\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "counter", Summary: "Doubles a number", Labels: []string{"math"}},
	tool.Op("double", double),
)

func main() {}

func double(ctx *tool.Context, a Args) (Out, error) { return Out{Doubled: a.N * 2}, nil }
`

func fixture(t *testing.T) *toolkit.Toolkit {
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

	for _, src := range []string{greeterSrc, counterSrc} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.Add(ctx, dir); err != nil {
			t.Skipf("cannot build a fixture tool: %v", err)
		}
	}
	return tk
}

// connect wires a real MCP client to a server over the SDK's in-memory
// transport, so the test exercises the actual protocol rather than calling
// handlers directly.
func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()

	go func() {
		if err := srv.Run(ctx, serverT); err != nil {
			t.Logf("server stopped: %v", err)
		}
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func listNames(t *testing.T, sess *mcp.ClientSession) []string {
	t.Helper()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func TestToolsAppearWithTheirSchemas(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}

	// A tool with several operations is split; one with a single operation
	// keeps its own name, because "counter" reads better than "counter_double".
	for _, want := range []string{"greeter_greet", "greeter_fail", "counter"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("no tool named %q; got %v", want, listNames(t, sess))
		}
	}

	greet := byName["greeter_greet"]
	if greet == nil {
		t.Fatal("greeter_greet missing")
	}
	// The schema goes over verbatim: forge and the SDK use the same jsonschema
	// package, so there is no conversion step to get wrong.
	raw, err := json.Marshal(greet.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"who to greet"`) {
		t.Errorf("input schema lost its description: %s", raw)
	}
	if greet.Annotations == nil || !greet.Annotations.ReadOnlyHint {
		t.Error("the read-only annotation did not survive")
	}
	if !strings.Contains(greet.Description, "Greets someone") {
		t.Errorf("description = %q", greet.Description)
	}
}

// TestTheViewShrinksTheToolList is the reason this surface exists.
func TestTheViewShrinksTheToolList(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})

	srv, err := mgr.Server("demo", labels.MustParse("demo"))
	if err != nil {
		t.Fatal(err)
	}
	names := listNames(t, connect(t, srv))

	for _, n := range names {
		if strings.HasPrefix(n, "counter") {
			t.Errorf("a tool outside the view was exposed: %v", names)
		}
	}
	if len(names) != 2 {
		t.Errorf("view exposed %v, want only the greeter's two operations", names)
	}
}

func TestEachViewGetsItsOwnServer(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})

	demo, err := mgr.Server("demo", labels.MustParse("demo"))
	if err != nil {
		t.Fatal(err)
	}
	math, err := mgr.Server("math", labels.MustParse("math"))
	if err != nil {
		t.Fatal(err)
	}
	if demo == math {
		t.Fatal("two views shared one server")
	}
	if got := listNames(t, connect(t, math)); len(got) != 1 || got[0] != "counter" {
		t.Errorf("math view = %v, want just counter", got)
	}
}

func TestCallingAToolReturnsItsResult(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv)

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "greeter_greet",
		Arguments: map[string]any{"name": "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("call reported an error: %+v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "Hello, world!" {
		t.Errorf("content = %+v", res.Content[0])
	}
}

func TestJSONResultsCarryStructuredContentAndText(t *testing.T) {
	// Both, because a client that ignores structuredContent would otherwise
	// show the call as having returned nothing.
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv)

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "counter",
		Arguments: map[string]any{"n": 21},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuredContent == nil {
		t.Error("no structuredContent")
	} else {
		m, _ := res.StructuredContent.(map[string]any)
		if m["doubled"] != float64(42) {
			t.Errorf("structuredContent = %v", res.StructuredContent)
		}
	}
	if len(res.Content) == 0 {
		t.Error("no text fallback for clients that ignore structuredContent")
	}
}

// TestAToolThatFailsIsAResultNotAProtocolError is what lets a model see the
// failure and try something else. A protocol error would surface as the call
// having broken rather than the tool having said no.
func TestAToolThatFailsIsAResultNotAProtocolError(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv)

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "greeter_fail",
		Arguments: map[string]any{"name": "x"},
	})
	if err != nil {
		t.Fatalf("a failing tool became a protocol error: %v", err)
	}
	if !res.IsError {
		t.Error("IsError not set")
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, "decided to fail") {
		t.Errorf("content = %+v, want the tool's own message", res.Content)
	}
}

// TestInvalidInputIsAProtocolError is the other half: the model cannot fix a
// schema violation by rewording, so it is reported as an error rather than as
// a result it might mistake for output.
func TestInvalidInputIsAProtocolError(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv)

	_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "greeter_greet",
		Arguments: map[string]any{},
	})
	if err == nil {
		t.Fatal("missing required input was accepted")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("err = %v, want it to name the field", err)
	}
}

func TestAddingAToolUpdatesLiveServers(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv)

	before := len(listNames(t, sess))

	dir := t.TempDir()
	src := strings.ReplaceAll(counterSrc, "counter", "tripler")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Add(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Sync(); err != nil {
		t.Fatal(err)
	}

	// Give the SDK's 10ms coalescing timer room to fire.
	time.Sleep(100 * time.Millisecond)

	after := listNames(t, sess)
	if len(after) != before+1 {
		t.Errorf("tool list went from %d to %d, want one more: %v", before, len(after), after)
	}
	var found bool
	for _, n := range after {
		if n == "tripler" {
			found = true
		}
	}
	if !found {
		t.Errorf("the new tool did not appear: %v", after)
	}
}

func TestMetaToolsFindTheToolsAViewHides(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, MetaTools: true})
	srv, _ := mgr.Server("demo", labels.MustParse("demo"))
	sess := connect(t, srv)

	names := listNames(t, sess)
	var hasSearch bool
	for _, n := range names {
		if n == "forge_search_tools" {
			hasSearch = true
		}
		// A view-changing tool would be a privilege-escalation primitive
		// reachable by prompt injection in any other tool's output.
		if n == "forge_set_view" {
			t.Error("a view-changing meta-tool is exposed")
		}
	}
	if !hasSearch {
		t.Fatalf("no search meta-tool: %v", names)
	}

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_search_tools",
		Arguments: map[string]any{"query": "double"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, "counter") {
		t.Errorf("search did not find the hidden tool: %+v", res.Content)
	}
	if !strings.Contains(text.Text, "not in the current view") {
		t.Errorf("search did not say the tool is out of view: %q", text.Text)
	}
	// Schemas are deliberately absent: they are the expensive part, and leaving
	// them out is what makes discovery cheap enough to offer.
	if strings.Contains(text.Text, "jsonschema") || strings.Contains(text.Text, `"properties"`) {
		t.Errorf("search leaked schemas into the result: %q", text.Text)
	}
}

func TestDescribeToolExplainsAnOutOfViewTool(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, MetaTools: true})
	srv, _ := mgr.Server("demo", labels.MustParse("demo"))
	sess := connect(t, srv)

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_describe_tool",
		Arguments: map[string]any{"name": "counter"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil {
		t.Fatal("no content")
	}
	if !strings.Contains(text.Text, "NOT in the current view") {
		t.Errorf("describe did not say the tool is unreachable: %q", text.Text)
	}
	if !strings.Contains(text.Text, "doubled") && !strings.Contains(text.Text, "properties") {
		t.Errorf("describe did not include the schema: %q", text.Text)
	}
}

// TestSkillToolFollowsMetaTools pins where forge_install_skill is registered.
//
// It was first written outside the MetaTools flag, on the reasoning that
// writing a Markdown file is harmless. The view tests caught it: a server told
// to expose no meta-tools was still exposing one, which turns "an unknown view
// serves nothing" into "an unknown view confirms forge is listening here".
func TestSkillToolFollowsMetaTools(t *testing.T) {
	for _, meta := range []bool{true, false} {
		t.Run(strconv.FormatBool(meta), func(t *testing.T) {
			tk := fixture(t)
			mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, MetaTools: meta})
			srv, err := mgr.Server("demo", labels.MustParse("demo"))
			if err != nil {
				t.Fatal(err)
			}

			var found bool
			for _, n := range listNames(t, connect(t, srv)) {
				if n == "forge_install_skill" {
					found = true
				}
			}
			if found != meta {
				t.Errorf("forge_install_skill present=%v with MetaTools=%v; it belongs behind that flag",
					found, meta)
			}
		})
	}
}
