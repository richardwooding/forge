package toolkit_test

// The filesystem capability was declarable, grantable and displayed as held
// while doing nothing at all: Options.Mounts was never populated, so the
// os.Root jail was never installed and every open in a guest failed with
// EBADF. These tests run a real compiled guest against a real granted
// directory, because that is the only arrangement that would have caught it --
// the jail's own unit tests passed throughout, since the jail was correct and
// simply never reached.

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

const fsToolSource = `package main

import (
	"os"
	"path/filepath"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	Do   string ` + "`json:\"do\"`" + `
	Path string ` + "`json:\"path\"`" + `
	Data string ` + "`json:\"data,omitempty\"`" + `
}

type Out struct {
	Result string ` + "`json:\"result\"`" + `
	Failed bool   ` + "`json:\"failed,omitempty\"`" + `
}

var _ = tool.Register(tool.Spec{
	Name:    "fsprobe",
	Summary: "reads and writes files, to prove the jail is mounted",
	Needs: []tool.Need{
		{Kind: tool.FSRead, Scope: []string{"SCOPE_READ"}, Reason: "the test drives it"},
		{Kind: tool.FSWrite, Scope: []string{"SCOPE_WRITE"}, Reason: "the test drives it"},
	},
}, tool.Op("run", run))

func main() {}

func run(ctx *tool.Context, a Args) (Out, error) {
	switch a.Do {
	case "read":
		b, err := os.ReadFile(a.Path)
		if err != nil {
			return Out{Result: err.Error(), Failed: true}, nil
		}
		return Out{Result: string(b)}, nil
	case "list":
		entries, err := os.ReadDir(a.Path)
		if err != nil {
			return Out{Result: err.Error(), Failed: true}, nil
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return Out{Result: filepath.Join(names...)}, nil
	case "write":
		if err := os.WriteFile(a.Path, []byte(a.Data), 0o644); err != nil {
			return Out{Result: err.Error(), Failed: true}, nil
		}
		return Out{Result: "written"}, nil
	}
	return Out{Result: "unknown op", Failed: true}, nil
}
`

type fsOut struct {
	Result string `json:"result"`
	Failed bool   `json:"failed"`
}

// fsFixture installs fsprobe with readDir granted read-only and writeDir
// granted for writing.
func fsFixture(t *testing.T, readDir, writeDir string) *toolkit.Toolkit {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a wasm tool with the Go toolchain; skipped in -short")
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
			Cache:  filepath.Join(os.TempDir(), forgeTestCache),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })

	src := strings.NewReplacer(
		"SCOPE_READ", readDir,
		"SCOPE_WRITE", writeDir,
	).Replace(fsToolSource)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Add(ctx, dir); err != nil {
		t.Skipf("cannot build the fs probe: %v", err)
	}

	if err := tk.Policy().Grant("fsprobe", []capability.Grant{
		{Kind: capability.FSRead, Scope: []string{readDir}},
		{Kind: capability.FSWrite, Scope: []string{writeDir}},
	}); err != nil {
		t.Fatal(err)
	}
	return tk
}

func runFS(t *testing.T, tk *toolkit.Toolkit, do, path, data string) fsOut {
	t.Helper()
	input, err := json.Marshal(map[string]string{"do": do, "path": path, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tk.Invoke(context.Background(), toolkit.Call{Tool: "fsprobe", Op: "run", Input: input})
	if err != nil {
		t.Fatalf("invoking fsprobe %s: %v", do, err)
	}
	var out fsOut
	if err := json.Unmarshal(res.Rendition.JSON, &out); err != nil {
		t.Fatalf("decoding %s: %v", res.Rendition.JSON, err)
	}
	return out
}

// TestGrantedDirectoryIsReadable is issue #5 in one assertion.
func TestGrantedDirectoryIsReadable(t *testing.T) {
	readDir, writeDir := t.TempDir(), t.TempDir()
	want := "the file contents"
	if err := os.WriteFile(filepath.Join(readDir, "seed.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := fsFixture(t, readDir, writeDir)

	got := runFS(t, tk, "read", filepath.Join(readDir, "seed.txt"), "")
	if got.Failed {
		t.Fatalf("reading inside the granted scope failed: %s", got.Result)
	}
	if got.Result != want {
		t.Errorf("got %q, want %q", got.Result, want)
	}

	if listed := runFS(t, tk, "list", readDir, ""); listed.Failed {
		t.Errorf("listing the granted directory failed: %s", listed.Result)
	}
}

// TestGrantedPathsKeepTheirHostSpelling pins the mount decision. A tool handed
// an absolute host path must find the file there; mounting scopes at / would
// make every path a caller passes mean something else inside the guest.
func TestGrantedPathsKeepTheirHostSpelling(t *testing.T) {
	readDir, writeDir := t.TempDir(), t.TempDir()
	nested := filepath.Join(readDir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := fsFixture(t, readDir, writeDir)

	got := runFS(t, tk, "read", filepath.Join(nested, "deep.txt"), "")
	if got.Failed || got.Result != "deep" {
		t.Errorf("a nested path under the grant: %+v", got)
	}
}

func TestWriteGrantWritesAndReadOnlyDoesNot(t *testing.T) {
	readDir, writeDir := t.TempDir(), t.TempDir()
	tk := fsFixture(t, readDir, writeDir)

	target := filepath.Join(writeDir, "out.txt")
	if got := runFS(t, tk, "write", target, "hello"); got.Failed {
		t.Fatalf("writing inside the write grant failed: %s", got.Result)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "hello" {
		t.Fatalf("the file on disk: %q %v", b, err)
	}

	// The read-only mount must refuse a write, or fs.read and fs.write are
	// the same capability wearing two names.
	if got := runFS(t, tk, "write", filepath.Join(readDir, "nope.txt"), "x"); !got.Failed {
		t.Error("a read-only grant accepted a write")
	}
	if _, err := os.Stat(filepath.Join(readDir, "nope.txt")); err == nil {
		t.Error("the read-only mount wrote a file to disk")
	}
}

// TestUngrantedPathsStayUnreachable checks the jail is doing its job now that
// it is actually installed: neither an unrelated directory nor a traversal out
// of a granted one may be read.
func TestUngrantedPathsStayUnreachable(t *testing.T) {
	readDir, writeDir := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("not for tools"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := fsFixture(t, readDir, writeDir)

	for _, p := range []string{
		secret,
		"/etc/passwd",
		filepath.Join(readDir, "..", filepath.Base(outside), "secret.txt"),
	} {
		got := runFS(t, tk, "read", p, "")
		if !got.Failed {
			t.Errorf("read %s, which is outside every grant: %q", p, got.Result)
		}
		if strings.Contains(got.Result, "not for tools") {
			t.Errorf("the contents of %s leaked", p)
		}
	}
}

// TestNoFSGrantGetsNoMount: a tool with no filesystem grant should get an
// ordinary error, not a trap, and certainly not a mount.
func TestNoFSGrantGetsNoMount(t *testing.T) {
	readDir, writeDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(readDir, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := fsFixture(t, readDir, writeDir)
	if err := tk.Policy().Revoke("fsprobe"); err != nil {
		t.Fatal(err)
	}
	// Nobody to ask and nothing granted: the call is refused before the guest
	// runs, which is the correct answer and not an EBADF from inside it.
	input, err := json.Marshal(map[string]string{"do": "read", "path": filepath.Join(readDir, "seed.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Invoke(context.Background(), toolkit.Call{Tool: "fsprobe", Op: "run", Input: input}); err == nil {
		t.Error("a tool with revoked grants was invoked anyway")
	}
}
