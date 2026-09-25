package wasmrt_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// buildFixture compiles testdata/<name> into a wasip1 reactor module, with the
// repository's own sdk wired in through a replace directive in the temporary
// module. The replace lives only in the temp directory: it is how a tool author
// works on the SDK and their tool together, and it must never end up in a
// committed go.mod.
//
// The result is cached for the package's test run, because compiling the Go
// runtime to wasm costs about a second and several tests want the same module.
func buildFixture(t *testing.T, name string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles a wasm guest with the Go toolchain; skipped in -short")
	}
	v, err := fixtureCache(name)
	if err != nil {
		t.Skipf("cannot build the %s fixture: %v", name, err)
	}
	return v
}

var (
	fixtureMu   sync.Mutex
	fixtureDone = map[string]fixtureResult{}
)

type fixtureResult struct {
	wasm []byte
	err  error
}

func fixtureCache(name string) ([]byte, error) {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if r, ok := fixtureDone[name]; ok {
		return r.wasm, r.err
	}
	wasm, err := compileFixture(name)
	fixtureDone[name] = fixtureResult{wasm, err}
	return wasm, err
}

func compileFixture(name string) ([]byte, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	sdkDir, err := filepath.Abs(filepath.Join("..", "..", "sdk"))
	if err != nil {
		return nil, err
	}

	src, err := os.MkdirTemp("", "forge-fixture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(src)

	body, err := os.ReadFile(filepath.Join("testdata", name, "main.go.txt"))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), body, 0o644); err != nil {
		return nil, err
	}
	gomod := "module forge.local/" + name + "\n\ngo 1.27.1\n\n" +
		"require github.com/richardwooding/forge/sdk v0.0.0\n\n" +
		"replace github.com/richardwooding/forge/sdk => " + sdkDir + "\n"
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte(gomod), 0o644); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// The same hermetic environment the build package will use. GOWORK=off and
	// an empty GOFLAGS matter here as much as in production: a go.work anywhere
	// above the temp directory would hijack module resolution, and an inherited
	// GOFLAGS would silently change the build.
	env := append(os.Environ(),
		"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local",
	)

	tidy := exec.CommandContext(ctx, goBin, "mod", "tidy")
	tidy.Dir, tidy.Env = src, env
	var tidyErr bytes.Buffer
	tidy.Stderr = &tidyErr
	if err := tidy.Run(); err != nil {
		return nil, wrap(err, tidyErr.String())
	}

	out := filepath.Join(src, "fixture.wasm")
	build := exec.CommandContext(ctx, goBin, "build", "-buildmode=c-shared",
		"-trimpath", "-buildvcs=false", "-o", out, "./")
	build.Dir, build.Env = src, env
	var buildErr bytes.Buffer
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		return nil, wrap(err, buildErr.String())
	}
	return os.ReadFile(out)
}

func wrap(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return &fixtureError{err, stderr}
}

type fixtureError struct {
	err    error
	stderr string
}

func (e *fixtureError) Error() string { return e.err.Error() + ": " + e.stderr }
func (e *fixtureError) Unwrap() error { return e.err }

func cacheDir(t *testing.T) string {
	t.Helper()
	// A shared directory across the package's tests, so the second engine
	// compiles from cache. Keyed by GOARCH because compiled code is native.
	dir := filepath.Join(os.TempDir(), "forge-test-wasmcache-"+runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
