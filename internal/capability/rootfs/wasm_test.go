package rootfs_test

// This file is the one that actually proves the jail. The unit tests in
// rootfs_test.go call the FS directly; this one compiles a hostile Go program
// to wasm with the real toolchain and lets it loose through wazero's WASI path
// handling, which is the only code path that matters in production.
//
// The distinction is not academic. A sandbox test that calls the boundary
// directly proves the boundary rejects what the test thought to pass it; it
// says nothing about the paths the runtime constructs on the way there.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/forge/internal/capability/rootfs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// buildEscaper compiles testdata/escaper to a wasip1 reactor module.
func buildEscaper(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles a wasm guest with the Go toolchain; skipped in -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}

	src := t.TempDir()
	for name, dst := range map[string]string{"main.go.txt": "main.go", "go.mod.txt": "go.mod"} {
		b, err := os.ReadFile(filepath.Join("testdata", "escaper", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "escaper.wasm")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "build", "-buildmode=c-shared",
		"-trimpath", "-buildvcs=false", "-o", out, "./")
	cmd.Dir = src
	// The same hermetic environment forge's build package will use: never
	// inherit GOFLAGS or GOWORK, and never let go.mod pull a new toolchain.
	cmd.Env = append(os.Environ(),
		"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot build wasip1 guest (%v): %s", err, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixture builds the directory tree the guest will be let loose in.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("work/sub")
	write("secret.txt", "TOP SECRET")
	write("work/hello.txt", "hello")
	for _, ln := range [][2]string{
		{"../secret.txt", "work/rel-escape"},
		{"/etc/passwd", "work/abs-escape"},
		{"../../secret.txt", "work/sub/up"},
	} {
		if err := os.Symlink(ln[0], filepath.Join(dir, ln[1])); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHostileGuestCannotEscapeTheMount(t *testing.T) {
	wasm := buildEscaper(t)
	dir := fixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(256))
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	root, err := os.OpenRoot(filepath.Join(dir, "work"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	fsc, ok := wazero.NewFSConfig().(sysfs.FSConfig)
	if !ok {
		t.Fatal("wazero.NewFSConfig() is not a sysfs.FSConfig; the mount API moved")
	}

	var stdout bytes.Buffer
	cm, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		WithStdout(&stdout).
		WithSysNanotime().WithSysNanosleep().
		WithFSConfig(fsc.WithSysFSMount(rootfs.New(root, rootfs.ReadWrite), "/work")))
	if err != nil {
		t.Fatal(err)
	}

	fn := mod.ExportedFunction("attempt")
	if fn == nil {
		t.Fatal("guest did not export attempt; reactor build may have changed")
	}
	res, err := fn.Call(ctx)
	if err != nil {
		t.Fatalf("attempt: %v\n%s", err, stdout.String())
	}

	t.Logf("guest output:\n%s", stdout.String())

	if n := api.DecodeI32(res[0]); n != 0 {
		t.Errorf("guest reported %d escapes", n)
	}
	if strings.Contains(stdout.String(), "ESCAPED") {
		t.Error("guest escaped the mount")
	}
	if strings.Contains(stdout.String(), "TOP SECRET") {
		t.Error("guest read the secret outside its mount")
	}
	if !strings.Contains(stdout.String(), `OK legitimate read "hello"`) {
		t.Error("the legitimate read failed, so the test proved nothing")
	}
	// Nothing may have been created outside the jail.
	if _, err := os.Stat(filepath.Join(dir, "pwned.txt")); err == nil {
		t.Error("guest wrote a file outside its mount")
	}
}
