package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/rest"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"
)

const greeterSrc = `package main

import (
	"errors"
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	Name string ` + "`json:\"name\" jsonschema:\"who to greet\"`" + `
	Loud bool   ` + "`json:\"loud,omitempty\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "greeter", Summary: "Greets someone", Labels: []string{"demo", "text"}},
	tool.Op("greet", greet, tool.Text(), tool.ReadOnly()),
	tool.Op("fail", fail, tool.Text()),
)

func main() {}

func greet(ctx *tool.Context, a Args) (string, error) {
	g := "Hello, " + a.Name + "!"
	if a.Loud {
		g = strings.ToUpper(g)
	}
	return g, nil
}

func fail(ctx *tool.Context, a Args) (string, error) {
	return "", errors.New("the tool decided to fail")
}
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
			Cache:  filepath.Join(os.TempDir(), "forge-test-restcache"),
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

	views, err := view.Open(tk.Paths().Config)
	if err != nil {
		t.Fatal(err)
	}
	return tk, views
}

func serve(t *testing.T) (*httptest.Server, *toolkit.Toolkit, *view.Store) {
	t.Helper()
	tk, views := fixture(t)
	srv := httptest.NewServer(rest.New(rest.Options{
		Toolkit: tk, Views: views, Default: labels.All,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, tk, views
}

// reply is what a test needs from a response. Returning this rather than an
// *http.Response means the body is read and closed in exactly one place, and
// no caller can hold a response open by forgetting to.
type reply struct {
	Status int
	Body   string
	Header http.Header
}

// The body is read and closed in the same function that opened it. Handing the
// response to a shared helper reads better but leaves the close somewhere a
// linter cannot follow, and a test helper is not worth arguing with a linter
// over.
func post(t *testing.T, srv *httptest.Server, path, body string) reply {
	t.Helper()
	res, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{Status: res.StatusCode, Body: string(b), Header: res.Header}
}

func get(t *testing.T, srv *httptest.Server, path string) reply {
	t.Helper()
	res, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{Status: res.StatusCode, Body: string(b), Header: res.Header}
}

func TestInvokeReturnsTheToolsOutput(t *testing.T) {
	srv, _, _ := serve(t)

	r := post(t, srv, "/v1/tools/greeter_greet/invoke", `{"name":"world"}`)
	if r.Status != http.StatusOK {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain for a text tool", ct)
	}
	if r.Body != "Hello, world!" {
		t.Errorf("body = %q", r.Body)
	}
}

func TestJSONToolReturnsJSON(t *testing.T) {
	srv, _, _ := serve(t)
	r := post(t, srv, "/v1/tools/counter/invoke", `{"n":21}`)
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	var out struct {
		Doubled int `json:"doubled"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Doubled != 42 {
		t.Errorf("doubled = %d", out.Doubled)
	}
}

// TestAFailingToolIs422 is the parity table's load-bearing row, on this
// surface: the request was understood and what it asked for could not be done.
// 500 would say forge broke, which is a different thing and sends whoever is
// reading the logs to the wrong place.
func TestAFailingToolIs422(t *testing.T) {
	srv, _, _ := serve(t)
	r := post(t, srv, "/v1/tools/greeter_fail/invoke", `{"name":"x"}`)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", r.Status)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(r.Body, "decided to fail") {
		t.Errorf("body does not carry the tool's own message: %s", r.Body)
	}
}

func TestInvalidInputIs400WithPointers(t *testing.T) {
	srv, _, _ := serve(t)
	r := post(t, srv, "/v1/tools/greeter_greet/invoke", `{}`)
	if r.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", r.Status)
	}
	var p struct {
		Type       string `json:"type"`
		Status     int    `json:"status"`
		Tool       string `json:"tool"`
		Violations []struct {
			Pointer string `json:"pointer"`
			Message string `json:"message"`
		} `json:"violations"`
	}
	if err := json.Unmarshal([]byte(r.Body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 400 || p.Tool != "greeter" {
		t.Errorf("problem = %+v", p)
	}
	// The pointer is the reason for using 9457 at all: a client should not
	// have to parse a sentence to learn which field is wrong.
	if len(p.Violations) != 1 || p.Violations[0].Pointer != "/name" {
		t.Errorf("violations = %+v, want one at /name", p.Violations)
	}
}

func TestUnknownToolIs404(t *testing.T) {
	srv, _, _ := serve(t)
	r := post(t, srv, "/v1/tools/nosuchtool/invoke", `{}`)
	if r.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", r.Status)
	}
}

// TestOutOfViewIs404NotForbidden: a narrow view must not leak what it hides.
// The CLI is the deliberate exception, because there the caller is a local
// human who owns the machine.
func TestOutOfViewIs404NotForbidden(t *testing.T) {
	srv, _, views := serve(t)
	if err := views.Set("demo", "demo"); err != nil {
		t.Fatal(err)
	}

	r := post(t, srv, "/v1/views/demo/tools/counter/invoke", `{"n":1}`)
	if r.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.Status)
	}
	if strings.Contains(strings.ToLower(r.Body), "forbidden") {
		t.Error("the response hints that the tool exists")
	}

	// ...and the tool it does cover still works through the same view.
	if in := post(t, srv, "/v1/views/demo/tools/greeter_greet/invoke", `{"name":"x"}`); in.Status != http.StatusOK {
		t.Errorf("in-view tool status = %d", in.Status)
	}
}

func TestAnUnknownViewServesNothing(t *testing.T) {
	srv, _, _ := serve(t)
	r := get(t, srv, "/v1/views/nosuchview/tools")
	if r.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 rather than a silent fall back to everything: %s",
			r.Status, r.Body)
	}
}

func TestListAndDescribe(t *testing.T) {
	srv, _, _ := serve(t)

	r := get(t, srv, "/v1/tools")
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	var list struct {
		Tools []struct {
			Name      string `json:"name"`
			InvokeURL string `json:"invokeUrl"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(r.Body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 3 {
		t.Errorf("listed %d operations, want 3", len(list.Tools))
	}
	for _, tl := range list.Tools {
		if !strings.HasSuffix(tl.InvokeURL, "/tools/"+tl.Name+"/invoke") {
			t.Errorf("invokeUrl %q does not match name %q", tl.InvokeURL, tl.Name)
		}
	}

	r = get(t, srv, "/v1/tools/counter")
	if r.Status != http.StatusOK {
		t.Fatalf("describe status %d", r.Status)
	}
	if !strings.Contains(r.Body, `"inputSchema"`) {
		t.Errorf("describe carries no schema: %s", r.Body)
	}
}

// TestTheAdvertisedSchemaIsTheCanonicalOne is the parity clause. Every surface
// must advertise byte-identical schemas; this checks REST against the bytes
// binding produced, which is the same copy MCP hands out and the same one
// Normalize enforces.
func TestTheAdvertisedSchemaIsTheCanonicalOne(t *testing.T) {
	srv, tk, _ := serve(t)

	b, err := tk.Bound("greeter", "greet")
	if err != nil {
		t.Fatal(err)
	}

	desc := get(t, srv, "/v1/tools/greeter_greet")
	var described struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal([]byte(desc.Body), &described); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, described.InputSchema, b.CanonJSON) {
		t.Errorf("describe advertises a different schema:\n got %s\nwant %s", described.InputSchema, b.CanonJSON)
	}

	doc := get(t, srv, "/v1/openapi.json").Body
	var api struct {
		Paths map[string]struct {
			Post struct {
				OperationID string `json:"operationId"`
				RequestBody struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
			} `json:"post"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(doc), &api); err != nil {
		t.Fatal(err)
	}
	path := "/v1/tools/greeter_greet/invoke"
	op, ok := api.Paths[path]
	if !ok {
		t.Fatalf("openapi has no path %s; has %v", path, keys(api.Paths))
	}
	if op.Post.OperationID != "greeter_greet" {
		t.Errorf("operationId = %q, want the same name every surface uses", op.Post.OperationID)
	}
	if !jsonEqual(t, op.Post.RequestBody.Content["application/json"].Schema, b.CanonJSON) {
		t.Error("openapi advertises a different schema from the canonical one")
	}
}

func TestOpenAPIIsPerView(t *testing.T) {
	srv, _, views := serve(t)
	if err := views.Set("demo", "demo"); err != nil {
		t.Fatal(err)
	}

	doc := get(t, srv, "/v1/views/demo/openapi.json").Body
	if strings.Contains(doc, "counter") {
		t.Error("a view's document describes a tool outside it")
	}
	if !strings.Contains(doc, "greeter_greet") {
		t.Error("a view's document is missing a tool inside it")
	}
	// Paths point back into the same view, so a generated client stays in it.
	if !strings.Contains(doc, "/v1/views/demo/tools/greeter_greet/invoke") {
		t.Errorf("paths do not stay within the view: %s", doc)
	}
}

func TestIndexIsSelfContained(t *testing.T) {
	// No CDN, no vendored bundle: a binary whose argument is that it sandboxes
	// what it runs should not ship a megabyte of third-party script, and a
	// localhost tool list does not need one.
	srv, _, _ := serve(t)
	r := get(t, srv, "/")
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	page := r.Body
	if strings.Contains(page, "<script") || strings.Contains(page, "https://cdn") {
		t.Error("the index pulls in script")
	}
	for _, want := range []string{"greeter_greet", "counter", "openapi.json"} {
		if !strings.Contains(page, want) {
			t.Errorf("index does not mention %q", want)
		}
	}
}

func TestUnknownPathIsAProblemDocument(t *testing.T) {
	srv, _, _ := serve(t)
	r := get(t, srv, "/nope")
	if r.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", r.Status)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
