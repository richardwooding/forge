package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/surface/cli"
	"github.com/richardwooding/forge/internal/toolkit"
)

// fixture installs a tool built from source into a throwaway forge home. It is
// slow because it runs the real toolchain, which is the point: these tests
// exercise the command tree that is actually built from a real manifest.
func fixture(t *testing.T) *toolkit.Toolkit {
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
			Cache:  filepath.Join(os.TempDir(), "forge-test-clicache"),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(toolSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Add(ctx, src); err != nil {
		t.Skipf("cannot build the fixture tool: %v", err)
	}
	return tk
}

const toolSource = `package main

import (
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	Name  string   ` + "`json:\"name\" jsonschema:\"who to greet\"`" + `
	Times int      ` + "`json:\"times,omitempty\"`" + `
	Loud  bool     ` + "`json:\"loud,omitempty\"`" + `
	Tags  []string ` + "`json:\"tags,omitempty\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "hello", Summary: "Greets someone", Labels: []string{"demo", "text"}},
	tool.Op("greet", greet, tool.Text()),
)

func main() {}

func greet(ctx *tool.Context, a Args) (string, error) {
	if a.Times <= 0 {
		a.Times = 1
	}
	g := strings.TrimSpace(strings.Repeat("Hello, "+a.Name+"! ", a.Times))
	if a.Loud {
		g = strings.ToUpper(g)
	}
	if len(a.Tags) > 0 {
		g += " [" + strings.Join(a.Tags, "|") + "]"
	}
	return g, nil
}
`

// run executes the command tree and returns stdout, stderr and the exit code
// the real binary would use.
func run(t *testing.T, tk *toolkit.Toolkit, sel labels.Selector, args ...string) (string, string, int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	root, err := cli.New(cli.Options{Toolkit: tk, Stdout: &out, Stderr: &errBuf, Selector: sel})
	if err != nil {
		t.Fatal(err)
	}
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errBuf)

	execErr := root.ExecuteContext(context.Background())
	if execErr != nil {
		cli.Render(&errBuf, execErr)
	}
	return out.String(), errBuf.String(), cli.ExitCode(execErr)
}

func TestToolBecomesASubcommandWithSchemaFlags(t *testing.T) {
	tk := fixture(t)
	out, errOut, code := run(t, tk, labels.All, "hello", "--name", "world")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "Hello, world!") {
		t.Errorf("stdout = %q", out)
	}
}

func TestBooleanFlagWorksBareAndExplicit(t *testing.T) {
	tk := fixture(t)
	for _, args := range [][]string{
		{"hello", "--name", "x", "--loud"},
		{"hello", "--name", "x", "--loud=true"},
	} {
		out, errOut, code := run(t, tk, labels.All, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d, %s", args, code, errOut)
		}
		if !strings.Contains(out, "HELLO, X!") {
			t.Errorf("%v: stdout = %q", args, out)
		}
	}
}

// TestRepeatableFlagDoesNotSplitOnCommas is the pflag StringSlice trap, checked
// through the real command tree rather than only in binding.
func TestRepeatableFlagDoesNotSplitOnCommas(t *testing.T) {
	tk := fixture(t)
	out, errOut, code := run(t, tk, labels.All,
		"hello", "--name", "x", "--tags", "a,b", "--tags", "c")
	if code != 0 {
		t.Fatalf("exit %d, %s", code, errOut)
	}
	if !strings.Contains(out, "[a,b|c]") {
		t.Errorf("stdout = %q, want the comma preserved inside one tag", out)
	}
}

// TestUnsetFlagsStayAbsentSoDefaultsApply is why the CLI never gives a flag the
// schema's default: it must be able to tell "not set" from "set to the zero
// value", or Normalize could not apply a default at all.
func TestUnsetFlagsStayAbsentSoDefaultsApply(t *testing.T) {
	tk := fixture(t)
	out, _, code := run(t, tk, labels.All, "hello", "--name", "x")
	if code != 0 {
		t.Fatal(code)
	}
	// times was not given, so the handler's own fallback of 1 applies.
	if strings.Count(out, "Hello") != 1 {
		t.Errorf("stdout = %q, want one greeting", out)
	}
}

func TestInputJSONAndSetLayer(t *testing.T) {
	tk := fixture(t)
	// A flag wins over --set, which wins over --input-json.
	var doc bytes.Buffer
	json.NewEncoder(&doc).Encode(map[string]any{"name": "from-json", "times": 1})

	root := t.TempDir()
	path := filepath.Join(root, "in.json")
	if err := os.WriteFile(path, doc.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := run(t, tk, labels.All,
		"hello", "--input-json", path, "--set", "name=from-set", "--name", "from-flag")
	if code != 0 {
		t.Fatalf("exit %d, %s", code, errOut)
	}
	if !strings.Contains(out, "Hello, from-flag!") {
		t.Errorf("stdout = %q, want the explicit flag to win", out)
	}
}

// TestOutOfViewToolIsExplained is the CLI's deliberate exception to the parity
// table. Every other surface answers not-found so a narrow view does not leak
// what it hides; here the caller is a local human who owns the machine.
func TestOutOfViewToolIsExplained(t *testing.T) {
	tk := fixture(t)
	sel := labels.MustParse("sql")

	_, errOut, code := run(t, tk, sel, "hello", "--name", "x")
	if code == 0 {
		t.Fatal("an out-of-view tool ran")
	}
	if !strings.Contains(errOut, "not in the current view") {
		t.Errorf("stderr = %q, want it to explain the view", errOut)
	}
	if !strings.Contains(errOut, "forge run hello") {
		t.Errorf("stderr = %q, want it to say how to run it anyway", errOut)
	}
}

func TestRunReachesAToolOutsideTheView(t *testing.T) {
	tk := fixture(t)
	out, errOut, code := run(t, tk, labels.MustParse("sql"),
		"run", "hello", "--input-json", "-")
	_ = out
	// run with no stdin gives an empty document, so this must fail on the
	// missing name rather than on the tool being invisible.
	if strings.Contains(errOut, "not in the current view") {
		t.Errorf("run was blocked by the view: %s", errOut)
	}
	if code == 0 {
		t.Log("stderr:", errOut)
	}
}

func TestUnknownToolSaysHowToLook(t *testing.T) {
	tk := fixture(t)
	_, errOut, code := run(t, tk, labels.All, "definitelynotatool")
	if code == 0 {
		t.Fatal("unknown tool succeeded")
	}
	if !strings.Contains(errOut, "forge ls") {
		t.Errorf("stderr = %q, want it to suggest forge ls", errOut)
	}
}

func TestValidationErrorsUseTheSharedFault(t *testing.T) {
	tk := fixture(t)
	_, errOut, code := run(t, tk, labels.All, "hello")
	if code != binding.FaultInvalidInput.ExitCode() {
		t.Errorf("exit = %d, want %d from the parity table", code, binding.FaultInvalidInput.ExitCode())
	}
	if !strings.Contains(errOut, "required property is missing") {
		t.Errorf("stderr = %q", errOut)
	}
	// With one problem the message is built from the violation, so listing it
	// again would say the same thing twice.
	if strings.Count(errOut, "required property is missing") != 1 {
		t.Errorf("stderr repeats the violation: %q", errOut)
	}
}

func TestLsFiltersByTheView(t *testing.T) {
	tk := fixture(t)

	out, _, code := run(t, tk, labels.MustParse("demo"), "ls")
	if code != 0 || !strings.Contains(out, "hello") {
		t.Errorf("matching view: out = %q, code = %d", out, code)
	}

	out, _, code = run(t, tk, labels.MustParse("sql"), "ls")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.Contains(out, "hello") {
		t.Errorf("a non-matching view listed the tool: %q", out)
	}
	if !strings.Contains(out, "no tools match") {
		t.Errorf("out = %q, want it to say the selector matched nothing", out)
	}
}

func TestInfoShowsSchemaDerivedFlags(t *testing.T) {
	tk := fixture(t)
	out, errOut, code := run(t, tk, labels.All, "info", "hello")
	if code != 0 {
		t.Fatalf("exit %d, %s", code, errOut)
	}
	for _, want := range []string{"hello", "demo", "greet", "--name", "--tags"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output does not mention %q:\n%s", want, out)
		}
	}
}

func TestStoreRecordSurvivesReinstall(t *testing.T) {
	// Reinstalling must keep the labels and grants a user added, or every
	// upgrade would re-ask questions they had already answered.
	tk := fixture(t)
	rec, err := tk.Get("hello")
	if err != nil {
		t.Fatal(err)
	}
	rec.ExtraLabels = []string{"mine"}
	if err := tk.Store().Put(rec); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(toolSource), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tk.Add(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replaced {
		t.Error("reinstall was not reported as a replacement")
	}
	got, err := tk.Get("hello")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(got.ExtraLabels, "mine") {
		t.Errorf("reinstall discarded the user's labels: %v", got.ExtraLabels)
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

var _ = store.Record{}
