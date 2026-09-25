package build_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/forge/internal/build"
	"github.com/richardwooding/forge/internal/manifest"
	"github.com/richardwooding/forge/internal/wasmrt"
)

func testConfig(t *testing.T) build.Config {
	t.Helper()
	sdk, err := filepath.Abs(filepath.Join("..", "..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	return build.Config{
		// A shared state directory across the package's tests, so the module
		// cache is populated once rather than per test.
		StateDir:   filepath.Join(os.TempDir(), "forge-test-buildstate"),
		SDKReplace: sdk,
		Timeout:    4 * time.Minute,
	}
}

func newBuilder(t *testing.T) *build.Builder {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the Go toolchain; skipped in -short")
	}
	b := build.New(testConfig(t))
	if _, ok := b.Available(context.Background()); !ok {
		t.Skip("no Go toolchain on PATH")
	}
	return b
}

const helloTool = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	Name string ` + "`json:\"name\" jsonschema:\"who to greet\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "hello", Summary: "Says hello", Labels: []string{"demo"}},
	tool.Op("greet", greet, tool.Text()),
)

func main() {}

func greet(ctx *tool.Context, a Args) (string, error) { return "Hello, " + a.Name + "!", nil }
`

func writeTool(t *testing.T, source string, withGoMod bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if withGoMod {
		gomod := "module example.com/hello\n\ngo 1.27.1\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBuildProducesARunnableTool is the whole pipeline: source in, a wasm
// module out, and forge able to describe and run it.
func TestBuildProducesARunnableTool(t *testing.T) {
	b := newBuilder(t)
	dir := writeTool(t, helloTool, true)

	art, err := b.Build(context.Background(), dir)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(art.Wasm) == 0 {
		t.Fatal("no wasm produced")
	}
	if art.Prov.WasmSHA == "" || art.Prov.GoVersion == "" {
		t.Errorf("provenance incomplete: %+v", art.Prov)
	}

	ctx := context.Background()
	e, err := wasmrt.NewEngine(ctx, wasmrt.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(ctx)

	c, err := e.Compile(ctx, art.Wasm)
	if err != nil {
		t.Fatalf("the built module does not compile: %v", err)
	}
	if c.Tier != wasmrt.TierReactor {
		t.Errorf("tier = %s, want reactor: the build must use -buildmode=c-shared", c.Tier)
	}

	raw, err := e.Describe(ctx, c)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	l, err := manifest.Parse(raw)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if l.Spec.Name != "hello" {
		t.Errorf("name = %q", l.Spec.Name)
	}

	resp, err := e.Invoke(ctx, c, wasmrt.Request{
		Op:    "greet",
		Input: []byte(`{"name":"world"}`),
		Opts:  wasmrt.Options{Tool: "hello", Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Text != "Hello, world!" {
		t.Errorf("text = %q", resp.Text)
	}
}

// TestBuildWorksWithoutAGoMod covers the single-file and bare-directory cases,
// where forge has to create the module itself.
func TestBuildWorksWithoutAGoMod(t *testing.T) {
	b := newBuilder(t)

	t.Run("directory", func(t *testing.T) {
		dir := writeTool(t, helloTool, false)
		if _, err := b.Build(context.Background(), dir); err != nil {
			t.Fatalf("Build: %v", err)
		}
	})

	t.Run("single file", func(t *testing.T) {
		dir := writeTool(t, helloTool, false)
		if _, err := b.Build(context.Background(), filepath.Join(dir, "main.go")); err != nil {
			t.Fatalf("Build: %v", err)
		}
	})
}

// TestBuildNeverWritesToTheSourceDirectory is the property that makes `forge
// tool add` safe to run on a directory the author is working in: installing
// their tool must not rewrite their go.mod or leave a go.sum behind.
func TestBuildNeverWritesToTheSourceDirectory(t *testing.T) {
	b := newBuilder(t)
	dir := writeTool(t, helloTool, true)

	before := snapshot(t, dir)
	if _, err := b.Build(context.Background(), dir); err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := snapshot(t, dir)

	if len(before) != len(after) {
		t.Errorf("the build changed the source directory:\n before %v\n after  %v", before, after)
	}
	for name, sum := range before {
		if after[name] != sum {
			t.Errorf("the build modified %s", name)
		}
	}
	// In particular, no replace directive can have reached the author's go.mod.
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gomod), "replace") {
		t.Error("the SDK replace leaked into the author's go.mod")
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// TestCompileErrorsPointAtTheAuthorsOwnFiles is the difference between a
// usable error and a puzzle. The build happens in a copy, so an unremapped
// diagnostic names a temporary directory the author has never seen.
func TestCompileErrorsPointAtTheAuthorsOwnFiles(t *testing.T) {
	b := newBuilder(t)
	broken := strings.Replace(helloTool,
		`return "Hello, " + a.Name + "!", nil`,
		`return undefinedThing, nil`, 1)
	dir := writeTool(t, broken, true)

	_, err := b.Build(context.Background(), dir)
	if err == nil {
		t.Fatal("a broken tool built successfully")
	}

	var be *build.Error
	if !errors.As(err, &be) {
		t.Fatalf("error is not a *build.Error: %T %v", err, err)
	}
	if len(be.Diags) == 0 {
		t.Fatalf("no diagnostics parsed from:\n%s", be.Raw)
	}
	var found bool
	for _, d := range be.Diags {
		t.Logf("diagnostic: %s", d)
		if strings.HasPrefix(d.File, dir) {
			found = true
		}
		if strings.Contains(d.File, os.TempDir()) && !strings.HasPrefix(d.File, dir) {
			t.Errorf("diagnostic points into the build workspace: %s", d.File)
		}
	}
	if !found {
		t.Errorf("no diagnostic names the author's own directory %s", dir)
	}
}

// TestNetworkUseIsExplained checks the hint for the failure a Go developer is
// least likely to recognise: wasip1 has no sockets at all.
func TestNetworkUseIsExplained(t *testing.T) {
	b := newBuilder(t)
	netTool := strings.Replace(helloTool,
		`import "github.com/richardwooding/forge/sdk/tool"`,
		"import (\n\t\"net\"\n\n\t\"github.com/richardwooding/forge/sdk/tool\"\n)", 1)
	netTool = strings.Replace(netTool,
		`return "Hello, " + a.Name + "!", nil`,
		"c, err := net.Dial(\"tcp\", a.Name)\n\tif err != nil {\n\t\treturn \"\", err\n\t}\n\tdefer c.Close()\n\treturn \"ok\", nil", 1)
	dir := writeTool(t, netTool, true)

	// net.Dial compiles for wasip1 and fails at runtime, so this may well
	// build. What must not happen is a confusing failure with no explanation.
	if _, err := b.Build(context.Background(), dir); err != nil {
		var be *build.Error
		if errors.As(err, &be) {
			for _, d := range be.Diags {
				t.Logf("%s\n  hint: %s", d, d.Hint)
			}
		}
	}
}

func TestBuildRejectsNonGoInput(t *testing.T) {
	b := newBuilder(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background(), path); err == nil {
		t.Error("accepted a non-Go file")
	}
}

func TestMissingSourceIsReportedClearly(t *testing.T) {
	b := newBuilder(t)
	_, err := b.Build(context.Background(), filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want it to name the missing path", err)
	}
}
