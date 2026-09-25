// Package build turns a user's Go source into a wasm tool.
//
// Everything here runs on the host, unsandboxed. That is the honest shape of
// the problem and it is stated in forge's README as well: compiling a program
// runs the Go toolchain over code someone else wrote, and a hostile go.mod can
// pull arbitrary modules. `forge tool add` is a trust decision of the same kind
// as running `go build` on a stranger's repository. The wasm sandbox protects
// invocation, not installation.
//
// What this package can do is remove the surprises: build in a copy so the
// user's directory is never written to, construct the environment from scratch
// so no ambient setting changes the result, never run go:generate, and report
// failures against the paths the author actually has on disk.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config is how forge is set up to build, as opposed to what it is building.
type Config struct {
	// GoBin is the go command. Empty means look it up on PATH.
	GoBin string

	// StateDir holds forge's own module and build caches.
	StateDir string

	// Toolchain is the GOTOOLCHAIN setting. "local" pins the installed
	// toolchain, which is what stops a tool's go.mod from choosing one.
	Toolchain string

	// Proxy is GOPROXY. Set it to "off" to build without network.
	Proxy string

	// Timeout bounds one build.
	Timeout time.Duration

	// SDKReplace, when set, adds a replace directive pointing the forge SDK at
	// a local checkout. It is how someone works on a tool and the SDK at the
	// same time, and it is written only into the throwaway copy -- a replace
	// must never reach a committed go.mod.
	SDKReplace string
}

func (c Config) withDefaults() Config {
	if c.GoBin == "" {
		c.GoBin = "go"
	}
	if c.Toolchain == "" {
		c.Toolchain = "local"
	}
	if c.Proxy == "" {
		c.Proxy = "https://proxy.golang.org,direct"
	}
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Minute
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join(os.TempDir(), "forge-state")
	}
	return c
}

// Provenance records how an artifact was produced, so a build can be repeated
// and compared. Reproducibility holds for the same source, the same go.sum, the
// same toolchain version and the same flags; it is not claimed across Go
// releases, because the toolchain does not offer that.
type Provenance struct {
	GoVersion string    `json:"goVersion"`
	Target    string    `json:"target"`
	BuildMode string    `json:"buildMode"`
	Flags     []string  `json:"flags"`
	GoSumHash string    `json:"goSumHash,omitempty"`
	WasmSHA   string    `json:"wasmSha256"`
	Built     time.Time `json:"built"`
}

// Artifact is a compiled tool.
type Artifact struct {
	Wasm []byte
	Prov Provenance
}

// Builder compiles Go source to a wasm tool.
type Builder struct{ cfg Config }

// New returns a Builder.
func New(cfg Config) *Builder { return &Builder{cfg: cfg.withDefaults()} }

// Available reports the Go toolchain version, and whether it could be found.
func (b *Builder) Available(ctx context.Context) (string, bool) {
	bin, err := exec.LookPath(b.cfg.GoBin)
	if err != nil {
		return "", false
	}
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// Build compiles the Go source at src.
//
// src may be a directory with a go.mod, a directory without one, or a single
// .go file. In every case the source is copied into a throwaway workspace
// first: building in place would let the toolchain rewrite the author's go.mod
// and go.sum as a side effect of installing their tool.
func (b *Builder) Build(ctx context.Context, src string) (*Artifact, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	goBin, err := exec.LookPath(b.cfg.GoBin)
	if err != nil {
		return nil, fmt.Errorf("no Go toolchain found: %w (forge compiles tools with the go command; install Go, or add a prebuilt .wasm instead)", err)
	}

	src, err = filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", src, err)
	}

	work, err := os.MkdirTemp("", "forge-build-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	srcRoot := src
	if !info.IsDir() {
		srcRoot = filepath.Dir(src)
	}
	if err := b.stage(src, info, work); err != nil {
		return nil, err
	}

	if err := b.ensureModule(ctx, goBin, work); err != nil {
		return nil, err
	}

	out := filepath.Join(work, "forge-tool.wasm")
	flags := []string{
		"build",
		"-json",
		"-buildmode=c-shared",
		// Strips absolute source paths from the binary. Reproducibility, and
		// privacy: without it a shared .wasm carries the author's directory
		// layout.
		"-trimpath",
		// Stops the toolchain shelling out to git in a directory forge just
		// created, and keeps commit state out of the artifact.
		"-buildvcs=false",
		// The build must never rewrite go.mod or go.sum. An incomplete go.sum
		// then fails loudly, which is the desired behaviour.
		"-mod=readonly",
		"-ldflags=-s -w",
		"-o", out,
		"./",
	}

	cmd := exec.CommandContext(ctx, goBin, flags...)
	cmd.Dir = work
	cmd.Env = buildEnv(b.cfg)
	setProcessGroup(cmd)
	// go build spawns compiler subprocesses that outlive a plain Kill of the
	// parent, so cancellation has to reach the whole group.
	cmd.Cancel = func() error { return killGroup(cmd) }

	stdout, runErr := cmd.Output()
	if runErr != nil {
		var stderr string
		// errors.As rather than a type assertion: cmd.Output wraps its error
		// in some paths, and losing stderr here would throw away the compiler
		// output this whole error path exists to report.
		if ee, ok := errors.AsType[*exec.ExitError](runErr); ok {
			stderr = string(ee.Stderr)
		}
		return nil, b.buildError(ctx, stdout, stderr, work, srcRoot, runErr)
	}

	wasm, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("the build reported success but produced no output: %w", err)
	}

	sum := sha256.Sum256(wasm)
	prov := Provenance{
		Target:    "wasip1/wasm",
		BuildMode: "c-shared",
		Flags:     flags[1 : len(flags)-3],
		WasmSHA:   hex.EncodeToString(sum[:]),
		Built:     time.Now().UTC(),
	}
	if v, ok := b.Available(ctx); ok {
		prov.GoVersion = v
	}
	if h, err := hashFile(filepath.Join(work, "go.sum")); err == nil {
		prov.GoSumHash = h
	}
	return &Artifact{Wasm: wasm, Prov: prov}, nil
}

func hashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
