// Package wasmrt runs tools.
//
// It owns the wazero runtime, the compiled-module cache and the instance
// lifecycle, and it is the only package that knows a tool is wasm at all.
package wasmrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	wsys "github.com/tetratelabs/wazero/sys"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/capability/rootfs"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// Tier is how a module is entered.
type Tier string

const (
	// TierReactor is a module built with -buildmode=c-shared. It exports
	// _initialize and forge's own entry points, main never runs, and the
	// instance survives between calls.
	TierReactor Tier = "reactor"

	// TierCommand is an ordinary wasip1 command: one run of _start per call,
	// with the manifest harvested from stdout. Kept for modules built by an
	// older Go, by TinyGo without wasmexport, or in another language.
	TierCommand Tier = "command"
)

// Export names the guest must provide in reactor mode.
const (
	exportDescribe = "forge_describe"
	exportInvoke   = "forge_invoke"
	exportInit     = "_initialize"
)

// Config configures an engine.
type Config struct {
	// CacheDir holds compiled modules between runs. Compiling a 1.9 MB Go
	// module takes about half a second cold and about 23 ms from this cache,
	// so it is worth having even for a single-shot CLI.
	CacheDir string

	// ForceInterpreter selects wazero's interpreter even where the compiler is
	// available. Only useful for reproducing a platform that has no compiler.
	ForceInterpreter bool

	// MemoryLimitPages caps a single instance's linear memory. A page is 64 KiB.
	MemoryLimitPages uint32

	// MaxConcurrent bounds simultaneous invocations across every surface. Zero
	// means GOMAXPROCS.
	MaxConcurrent int
}

func (c Config) withDefaults() Config {
	if c.MemoryLimitPages == 0 {
		c.MemoryLimitPages = 4096 // 256 MiB
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 8
	}
	return c
}

// Engine compiles and runs tools.
type Engine struct {
	cfg   Config
	rt    wazero.Runtime
	cache wazero.CompilationCache
	sem   chan struct{}

	mu       sync.Mutex
	compiled map[string]*Compiled
}

// NewEngine builds an engine. Close it when done.
func NewEngine(ctx context.Context, cfg Config) (*Engine, error) {
	cfg = cfg.withDefaults()

	rc := wazero.NewRuntimeConfig().
		// Set explicitly rather than relying on a default: Go 1.26+ emits sign
		// extension and non-trapping float conversions unconditionally, and a
		// silent feature mismatch shows up as an unreadable validation error.
		WithCoreFeatures(api.CoreFeaturesV2).
		// wazero has no fuel metering, so this plus a context deadline is the
		// only thing standing between forge and a tool that loops forever.
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(cfg.MemoryLimitPages).
		// forge reports errors through its own envelopes, so wasm debug info
		// costs memory for nothing.
		WithDebugInfoEnabled(false)

	if cfg.ForceInterpreter {
		rc = wazero.NewRuntimeConfigInterpreter().
			WithCoreFeatures(api.CoreFeaturesV2).
			WithCloseOnContextDone(true).
			WithMemoryLimitPages(cfg.MemoryLimitPages).
			WithDebugInfoEnabled(false)
	}

	var cache wazero.CompilationCache
	if cfg.CacheDir != "" {
		if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
			return nil, fmt.Errorf("compilation cache directory: %w", err)
		}
		var err error
		cache, err = wazero.NewCompilationCacheWithDir(cfg.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("compilation cache: %w", err)
		}
		rc = rc.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiating WASI: %w", err)
	}
	if err := hostabi.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}

	return &Engine{
		cfg:      cfg,
		rt:       rt,
		cache:    cache,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		compiled: map[string]*Compiled{},
	}, nil
}

// Close releases the runtime and everything compiled into it.
func (e *Engine) Close(ctx context.Context) error {
	err := e.rt.Close(ctx)
	if e.cache != nil {
		_ = e.cache.Close(ctx)
	}
	return err
}

// Compiled is a module ready to instantiate. Compiling is the expensive step,
// and the result is safe to share across concurrent instantiations, so it is
// cached per digest for the life of the engine.
type Compiled struct {
	Digest   string
	Tier     Tier
	MinPages uint32

	cm wazero.CompiledModule
}

// Compile prepares a module, reusing an earlier compilation of the same bytes.
func (e *Engine) Compile(ctx context.Context, wasm []byte) (*Compiled, error) {
	sum := sha256.Sum256(wasm)
	digest := hex.EncodeToString(sum[:])

	e.mu.Lock()
	if c, ok := e.compiled[digest]; ok {
		e.mu.Unlock()
		return c, nil
	}
	e.mu.Unlock()

	cm, err := e.rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compiling module: %w", err)
	}

	c := &Compiled{Digest: digest, Tier: detectTier(cm), cm: cm}
	for _, m := range cm.ExportedMemories() {
		c.MinPages = m.Min()
	}
	// Refusing here, rather than at the first call, turns an obscure
	// instantiation failure into a clear message at install time.
	if c.MinPages > e.cfg.MemoryLimitPages {
		_ = cm.Close(ctx)
		return nil, fmt.Errorf("module needs %d pages of memory but the limit is %d (%d MiB); raise the limit or use a smaller tool",
			c.MinPages, e.cfg.MemoryLimitPages, e.cfg.MemoryLimitPages/16)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.compiled[digest]; ok {
		// Another goroutine won the race; keep one and discard the duplicate.
		_ = cm.Close(ctx)
		return existing, nil
	}
	e.compiled[digest] = c
	return c, nil
}

// detectTier decides how a module is entered by looking at what it exports.
func detectTier(cm wazero.CompiledModule) Tier {
	exports := cm.ExportedFunctions()
	_, hasInit := exports[exportInit]
	_, hasDescribe := exports[exportDescribe]
	if hasInit && hasDescribe {
		return TierReactor
	}
	return TierCommand
}

// Mount is one filesystem grant, resolved to a real directory.
type Mount struct {
	// HostPath is the directory on this machine.
	HostPath string
	// GuestPath is where the tool sees it.
	GuestPath string
	Mode      rootfs.Mode
}

// Options are the per-invocation settings the runtime needs.
type Options struct {
	Tool   string
	Grants capability.Set
	Mounts []Mount

	// Env is passed verbatim; the caller has already narrowed it to the names
	// the tool was granted.
	Env map[string]string

	Args   []string
	Stdin  []byte
	Stdout WriterTo
	Stderr WriterTo

	Timeout time.Duration
	Limits  hostabi.Limits

	OnLog      func(level hostabi.Level, tool, msg string)
	OnProgress func(done, total int64, msg string)

	// Services back the capability host functions. A nil service is a
	// capability this process cannot provide, which the guest is told about
	// in those words -- never as a denial, which would send a tool author
	// looking for a grant that was never the problem.
	Services hostabi.Services

	// CallPath is the chain of tools that led to this one, empty for a call
	// that started at a surface. tool.invoke appends to it, and a tool already
	// on the path is refused.
	CallPath []string

	// Budget is the allowance shared by a whole call tree. A nested call must
	// pass the one it inherited; leaving it nil gets a fresh allowance, which
	// is right only for a call that starts at a surface.
	Budget *hostabi.Budget
}

// WriterTo is the subset of io.Writer the runtime needs, kept as its own name
// so callers can pass a capped writer without importing io here.
type WriterTo interface {
	Write(p []byte) (int, error)
}

// ErrNoDescribe reports a module that does not implement forge's describe
// entry point.
var ErrNoDescribe = errors.New("module does not export " + exportDescribe)

// buildModuleConfig assembles a wazero ModuleConfig granting exactly what the
// options allow, starting from wazero's defaults, which grant nothing.
func (e *Engine) buildModuleConfig(opts Options, start string) (wazero.ModuleConfig, error) {
	cfg := wazero.NewModuleConfig().
		// Required. The default name comes from the module's name section, so
		// two concurrent instances of one tool would collide with "module has
		// already been instantiated".
		WithName("").
		WithStartFunctions(start).
		// Always a real monotonic clock and a real CSPRNG, regardless of
		// grants. wazero's deterministic default makes Go's map hash seed
		// predictable inside the guest, so withholding randomness is not a
		// restriction but a weakness; and the scheduler wants a clock that
		// moves. The wall clock is separate and is gated, because a tool
		// without that grant should see a frozen clock, not a real one.
		WithSysNanotime().
		WithSysNanosleep().
		WithRandSource(cryptoRand{})

	if opts.Grants.Has(capability.ClockWall) {
		cfg = cfg.WithSysWalltime()
	}
	if len(opts.Args) > 0 {
		cfg = cfg.WithArgs(opts.Args...)
	}
	for k, v := range opts.Env {
		cfg = cfg.WithEnv(k, v)
	}
	if len(opts.Stdin) > 0 {
		cfg = cfg.WithStdin(newBytesReader(opts.Stdin))
	}
	if opts.Stdout != nil {
		cfg = cfg.WithStdout(opts.Stdout)
	}
	if opts.Stderr != nil {
		cfg = cfg.WithStderr(opts.Stderr)
	}

	if len(opts.Mounts) > 0 {
		fsCfg, err := buildFSConfig(opts.Mounts)
		if err != nil {
			return nil, err
		}
		cfg = cfg.WithFSConfig(fsCfg)
	}
	// With no mounts, WithFSConfig is not called at all, so path_open reports
	// unsupported and an ordinary os.Open inside the guest returns a normal Go
	// error rather than trapping.
	return cfg, nil
}

// buildFSConfig mounts each granted directory through an os.Root-backed jail.
//
// Never wazero's own WithDirMount or WithReadOnlyDirMount: both document that a
// guest can escape them with "../..", which makes them unusable for tool code.
func buildFSConfig(mounts []Mount) (wazero.FSConfig, error) {
	cfg := wazero.NewFSConfig()
	for _, m := range mounts {
		sfc, ok := cfg.(sysfs.FSConfig)
		if !ok {
			return nil, errors.New("wazero.FSConfig no longer implements sysfs.FSConfig; the mount API has moved and forge's filesystem jail would be bypassed")
		}
		root, err := os.OpenRoot(m.HostPath)
		if err != nil {
			return nil, fmt.Errorf("mounting %s: %w", m.HostPath, err)
		}
		cfg = sfc.WithSysFSMount(rootfs.New(root, m.Mode), m.GuestPath)
	}
	return cfg, nil
}

// exitCode extracts a guest exit status, reporting whether the error was an
// ordinary exit rather than a failure.
func exitCode(err error) (int, bool) {
	if ee, ok := errors.AsType[*wsys.ExitError](err); ok {
		return int(ee.ExitCode()), true
	}
	return 0, false
}
