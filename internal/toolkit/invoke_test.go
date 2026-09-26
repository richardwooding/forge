package toolkit_test

// Tool-to-tool invocation is the last capability to land and the one with the
// most moving parts: it needs the registry, the policy, Intersect, the call
// path and a second wasm instance all working together. It is tested here,
// at the toolkit, because that is the lowest layer where all of those exist.
//
// Two distinct instances is not an optimisation. Re-entering a busy Go-on-wasm
// instance deadlocks -- one M, the caller's goroutine suspended mid-host-call
// -- so the callee must be a separate instance, and these tests only pass
// because it is.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/toolkit"
)

// callerSource calls another tool and reports what came back.
const callerSource = `package main

import (
	"fmt"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	Target string ` + "`json:\"target\"`" + `
	Word   string ` + "`json:\"word\"`" + `
}

type Out struct {
	Result string ` + "`json:\"result\"`" + `
	Denied bool   ` + "`json:\"denied,omitempty\"`" + `
	Code   string ` + "`json:\"code,omitempty\"`" + `
}

var _ = tool.Register(tool.Spec{
	Name:    "caller",
	Summary: "calls another tool",
	Needs: []tool.Need{
		{Kind: tool.Invoke, Scope: []string{"echoer", "caller"}, Reason: "the test drives it"},
		{Kind: tool.Secret, Scope: []string{"shared"}, Reason: "the test drives it"},
	},
}, tool.Op("run", run))

func main() {}

func run(ctx *tool.Context, a Args) (Out, error) {
	var out struct {
		Echo string ` + "`json:\"echo\"`" + `
	}
	err := tool.Call(a.Target, "run", map[string]string{"word": a.Word}, &out)
	if err != nil {
		if d, ok := tool.Denied(err); ok {
			return Out{Result: d.Error(), Denied: true, Code: d.Code}, nil
		}
		return Out{Result: err.Error()}, nil
	}
	return Out{Result: fmt.Sprintf("got %q", out.Echo)}, nil
}
`

// echoerSource is the callee. It also tries to read a secret, so the test can
// check that a callee cannot use a capability its caller lacks.
const echoerSource = `package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	Word string ` + "`json:\"word\"`" + `
}

type Out struct {
	Echo   string ` + "`json:\"echo\"`" + `
	Secret string ` + "`json:\"secret,omitempty\"`" + `
}

var _ = tool.Register(tool.Spec{
	Name:    "echoer",
	Summary: "echoes a word",
	Needs: []tool.Need{
		{Kind: tool.Secret, Scope: []string{"shared", "private"}, Reason: "the test drives it"},
	},
}, tool.Op("run", run))

func main() {}

func run(ctx *tool.Context, a Args) (Out, error) {
	out := Out{Echo: a.Word + "!"}
	// Whether this succeeds is the attenuation test: echoer was granted
	// secret(private) directly, but a caller without it must not gain it.
	if v, found, err := tool.GetSecret("private"); err == nil && found {
		out.Secret = v
	}
	return out, nil
}
`

func invokeFixture(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm tools with the Go toolchain; skipped in -short")
	}
	sdk, err := filepath.Abs(filepath.Join("..", "..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	ctx := context.Background()
	tk, err := toolkit.New(ctx, toolkit.Config{
		Paths: toolkit.Paths{
			Data:   filepath.Join(home, "data"),
			Cache:  filepath.Join(os.TempDir(), "forge-test-invokecache"),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })

	// "other" is echoer under a different name: a tool caller was never
	// granted permission to invoke, which is what the scope test needs.
	otherSource := strings.NewReplacer(
		`Name:    "echoer"`, `Name:    "other"`,
	).Replace(echoerSource)

	for name, src := range map[string]string{
		"caller": callerSource,
		"echoer": echoerSource,
		"other":  otherSource,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.Add(ctx, dir); err != nil {
			t.Skipf("cannot build %s: %v", name, err)
		}
	}

	if s := tk.Secrets(); s != nil {
		for _, n := range []string{"shared", "private"} {
			if err := s.Put(n, "value-of-"+n); err != nil {
				t.Fatal(err)
			}
		}
	}
	return tk
}

// grant gives a tool exactly what it declares.
func grant(t *testing.T, tk *toolkit.Toolkit, name string) {
	t.Helper()
	rec, err := tk.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	var grants []capability.Grant
	for _, req := range rec.Spec.Requires {
		grants = append(grants, capability.Grant{Kind: req.Kind, Scope: req.Scope})
	}
	if err := tk.Policy().Grant(name, grants); err != nil {
		t.Fatal(err)
	}
}

type callerOut struct {
	Result string `json:"result"`
	Denied bool   `json:"denied"`
	Code   string `json:"code"`
}

func runCaller(t *testing.T, tk *toolkit.Toolkit, target, word string) callerOut {
	t.Helper()
	input, err := json.Marshal(map[string]string{"target": target, "word": word})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tk.Invoke(context.Background(), toolkit.Call{
		Tool: "caller", Op: "run", Input: input,
	})
	if err != nil {
		t.Fatalf("invoking caller: %v", err)
	}
	var out callerOut
	if err := json.Unmarshal(res.Rendition.JSON, &out); err != nil {
		t.Fatalf("decoding %s: %v", res.Rendition.JSON, err)
	}
	return out
}

func TestInvokeReachesAnotherTool(t *testing.T) {
	tk := invokeFixture(t)
	grant(t, tk, "caller")
	grant(t, tk, "echoer")

	got := runCaller(t, tk, "echoer", "ping")
	if got.Denied {
		t.Fatalf("the call was denied: %s", got.Result)
	}
	if want := `got "ping!"`; got.Result != want {
		t.Errorf("got %q, want %q", got.Result, want)
	}
}

// TestInvokeAttenuatesGrants is the security property. echoer holds
// secret(private) in its own right, but reached through caller -- which does
// not -- it must not have it. Otherwise any tool could borrow another's
// capabilities simply by calling it.
func TestInvokeAttenuatesGrants(t *testing.T) {
	tk := invokeFixture(t)
	grant(t, tk, "caller")
	grant(t, tk, "echoer")

	// Called directly, echoer reads the secret it was granted.
	input, err := json.Marshal(map[string]string{"word": "x"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tk.Invoke(context.Background(), toolkit.Call{Tool: "echoer", Op: "run", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	var direct struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(res.Rendition.JSON, &direct); err != nil {
		t.Fatal(err)
	}
	if direct.Secret != "value-of-private" {
		t.Fatalf("echoer should read its own secret directly, got %q", direct.Secret)
	}

	// Reached through caller, it must not -- caller holds secret(shared) only.
	got := runCaller(t, tk, "echoer", "x")
	if got.Denied {
		t.Fatalf("the call itself was denied: %s", got.Result)
	}
	if strings.Contains(got.Result, "value-of-private") {
		t.Error("a callee kept a capability its caller does not hold")
	}
}

// TestInvokeRefusesSelfCall checks the cycle guard at its tightest point. A
// tool calling itself must be refused at the call, not discovered eight levels
// down by running out of depth.
func TestInvokeRefusesSelfCall(t *testing.T) {
	tk := invokeFixture(t)
	grant(t, tk, "caller")

	got := runCaller(t, tk, "caller", "loop")
	if !got.Denied {
		t.Fatalf("a tool called itself: %s", got.Result)
	}
	if !strings.Contains(got.Result, "chain") {
		t.Errorf("the refusal should explain the chain: %s", got.Result)
	}
}

// TestInvokeIsScopedToNamedCallees checks that tool.invoke is a list of
// callees rather than a key to the whole registry. caller declares -- and was
// granted -- invoke for echoer and itself; "other" is installed and working,
// and must still be out of reach.
func TestInvokeIsScopedToNamedCallees(t *testing.T) {
	tk := invokeFixture(t)
	grant(t, tk, "caller")
	grant(t, tk, "other")

	// "other" answers when called directly, so a refusal below is about scope
	// and not about the tool being broken or absent.
	input, err := json.Marshal(map[string]string{"word": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Invoke(context.Background(), toolkit.Call{
		Tool: "other", Op: "run", Input: input,
	}); err != nil {
		t.Fatalf("other should work when called directly: %v", err)
	}

	got := runCaller(t, tk, "other", "x")
	if !got.Denied {
		t.Fatalf("a callee outside the grant was reachable: %s", got.Result)
	}
	if got.Code != string(capability.DenyOutOfScope) {
		t.Errorf("got code %q, want %q", got.Code, capability.DenyOutOfScope)
	}
}
