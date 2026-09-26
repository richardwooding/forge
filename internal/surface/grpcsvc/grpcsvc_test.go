package grpcsvc_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/grpcsvc"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"

	forgev1 "github.com/richardwooding/forge/grpcapi/forge/v1"
)

const greeterSrc = `package main

import (
	"errors"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	Name string ` + "`json:\"name\" jsonschema:\"who to greet\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "greeter", Summary: "Greets someone", Labels: []string{"demo"}},
	tool.Op("greet", greet, tool.Text(), tool.ReadOnly()),
	tool.Op("fail", fail, tool.Text()),
)

func main() {}

func greet(ctx *tool.Context, a Args) (string, error) { return "Hello, " + a.Name + "!", nil }
func fail(ctx *tool.Context, a Args) (string, error)  { return "", errors.New("the tool decided to fail") }
`

const bigSrc = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	N int64 ` + "`json:\"n\"`" + `
}

type Out struct {
	N int64 ` + "`json:\"n\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "echo64", Summary: "Round-trips a large integer", Labels: []string{"math"}},
	tool.Op("run", run),
)

func main() {}

func run(ctx *tool.Context, a Args) (Out, error) { return Out{N: a.N}, nil }
`

func fixture(t *testing.T) (*toolkit.Toolkit, *view.Store) {
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
			Cache:  filepath.Join(os.TempDir(), "forge-test-grpccache"),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })

	for _, src := range []string{greeterSrc, bigSrc} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.Add(ctx, dir); err != nil {
			t.Skipf("cannot build a fixture tool: %v", err)
		}
	}
	views, err := view.Open(tk.Paths().Config)
	if err != nil {
		t.Fatal(err)
	}
	return tk, views
}

// dial runs the service over bufconn, so the test exercises real gRPC
// marshalling rather than calling the methods directly.
func dial(t *testing.T) (forgev1.ToolServiceClient, *toolkit.Toolkit, *view.Store) {
	t.Helper()
	tk, views := fixture(t)

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	forgev1.RegisterToolServiceServer(srv, grpcsvc.New(grpcsvc.Options{
		Toolkit: tk, Views: views, Default: labels.All,
	}))
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("server stopped: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return forgev1.NewToolServiceClient(conn), tk, views
}

func TestListAndDescribe(t *testing.T) {
	client, tk, _ := dial(t)
	ctx := context.Background()

	list, err := client.ListTools(ctx, &forgev1.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range list.GetTools() {
		names[tl.GetName()] = true
	}
	for _, want := range []string{"greeter_greet", "greeter_fail", "echo64"} {
		if !names[want] {
			t.Errorf("no tool named %q; got %v", want, names)
		}
	}

	desc, err := client.DescribeTool(ctx, &forgev1.DescribeToolRequest{Name: "greeter_greet"})
	if err != nil {
		t.Fatal(err)
	}
	// The schema crosses as text, not as a Struct: Struct's numbers are
	// doubles, so a round trip would turn "minLength": 3 into 3.0.
	b, err := tk.Bound("greeter", "greet")
	if err != nil {
		t.Fatal(err)
	}
	if desc.GetTool().GetInputSchemaJson() != string(b.CanonJSON) {
		t.Errorf("advertised schema differs from the canonical bytes:\n got %s\nwant %s",
			desc.GetTool().GetInputSchemaJson(), b.CanonJSON)
	}
	if !desc.GetTool().GetReadOnly() {
		t.Error("the read-only annotation did not survive")
	}
}

func TestInvokeJSONInput(t *testing.T) {
	client, _, _ := dial(t)
	res, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		Name:  "greeter_greet",
		Input: &forgev1.InvokeRequest_Json{Json: []byte(`{"name":"world"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetIsError() {
		t.Fatalf("tool errored: %s", res.GetError())
	}
	if res.GetText() != "Hello, world!" {
		t.Errorf("text = %q", res.GetText())
	}
}

// TestAFailingToolIsOKWithIsError is the parity table on this surface. A
// non-OK status would hide the message in status.Details(), where most clients
// never look, and would conflate "the tool said no" with "forge could not run
// it".
func TestAFailingToolIsOKWithIsError(t *testing.T) {
	client, _, _ := dial(t)
	res, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		Name:  "greeter_fail",
		Input: &forgev1.InvokeRequest_Json{Json: []byte(`{"name":"x"}`)},
	})
	if err != nil {
		t.Fatalf("a failing tool became a non-OK status: %v", err)
	}
	if !res.GetIsError() {
		t.Error("is_error not set")
	}
	if !strings.Contains(res.GetError(), "decided to fail") {
		t.Errorf("error = %q, want the tool's own message", res.GetError())
	}
}

func TestInvalidInputIsInvalidArgumentWithPointers(t *testing.T) {
	client, _, _ := dial(t)
	_, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		Name:  "greeter_greet",
		Input: &forgev1.InvokeRequest_Json{Json: []byte(`{}`)},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", status.Code(err))
	}
	// The pointer goes in the message, because a client that does not unpack
	// details would otherwise be told only that something was wrong, while
	// every other forge surface says which field.
	if !strings.Contains(err.Error(), "/name") {
		t.Errorf("err = %v, want it to name the field", err)
	}
}

func TestUnknownToolIsNotFound(t *testing.T) {
	client, _, _ := dial(t)
	_, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		Name:  "nosuchtool",
		Input: &forgev1.InvokeRequest_Json{Json: []byte(`{}`)},
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %s, want NotFound", status.Code(err))
	}
}

func TestOutOfViewIsNotFound(t *testing.T) {
	// Not PermissionDenied: a narrow view must not leak what it hides.
	client, _, views := dial(t)
	if err := views.Set("demo", "demo"); err != nil {
		t.Fatal(err)
	}
	_, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		View:  "demo",
		Name:  "echo64",
		Input: &forgev1.InvokeRequest_Json{Json: []byte(`{"n":1}`)},
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %s, want NotFound", status.Code(err))
	}
}

func TestAnUnknownViewIsNotFound(t *testing.T) {
	client, _, _ := dial(t)
	_, err := client.ListTools(context.Background(), &forgev1.ListToolsRequest{View: "nosuchview"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %s, want NotFound rather than a silent fall back to everything", status.Code(err))
	}
}

// TestJSONInputKeepsLargeIntegersExactly is the guarantee the rest of forge
// makes end to end.
func TestJSONInputKeepsLargeIntegersExactly(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1
	client, _, _ := dial(t)

	res, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		Name:  "echo64",
		Input: &forgev1.InvokeRequest_Json{Json: []byte(`{"n":` + big + `}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.GetJson()), big) {
		t.Errorf("json = %s, want it to contain %s exactly", res.GetJson(), big)
	}
}

// TestStructInputLosesPrecisionBeyond2Pow53 pins the one place forge's
// end-to-end integer guarantee does not hold.
//
// google.protobuf.Struct stores every number as a double. The field exists so
// grpcurl stays pleasant, and the cost is documented on the proto field rather
// than left to be discovered. Asserting it here is what stops a documented
// divergence from quietly becoming an undocumented one -- if a future protobuf
// release fixed it, this test fails and the documentation gets corrected.
func TestStructInputLosesPrecisionBeyond2Pow53(t *testing.T) {
	client, _, _ := dial(t)

	st, err := structpb.NewStruct(map[string]any{"n": float64(9007199254740993)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Invoke(context.Background(), &forgev1.InvokeRequest{
		Name:  "echo64",
		Input: &forgev1.InvokeRequest_StructValue{StructValue: st},
	})
	if err != nil {
		t.Fatal(err)
	}

	var out struct {
		N json.Number `json:"n"`
	}
	if err := json.Unmarshal(res.GetJson(), &out); err != nil {
		t.Fatal(err)
	}
	if out.N.String() == "9007199254740993" {
		t.Error("Struct now preserves integers beyond 2^53; the proto comment and the README should be corrected")
	}
	if out.N.String() != "9007199254740992" {
		t.Logf("Struct rounded to %s (expected 9007199254740992)", out.N)
	}
}

func TestInvokeStreamEndsWithTheResult(t *testing.T) {
	client, _, _ := dial(t)
	stream, err := client.InvokeStream(context.Background(), &forgev1.InvokeStreamRequest{
		Name:  "greeter_greet",
		Input: &forgev1.InvokeStreamRequest_Json{Json: []byte(`{"name":"stream"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	var result *forgev1.InvokeResponse
	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		if r := ev.GetResult(); r != nil {
			result = r
		}
	}
	if result == nil {
		t.Fatal("the stream ended without a result")
	}
	if result.GetText() != "Hello, stream!" {
		t.Errorf("text = %q", result.GetText())
	}
}
