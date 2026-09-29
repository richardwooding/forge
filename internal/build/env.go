package build

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// HostGoEnv is the effective module configuration read from the host
// toolchain: the values a plain `go build` in the developer's own shell would
// use, after `go env -w` and the ambient environment have both been applied.
//
// Resolved is false when no probe succeeded -- no toolchain, a broken env file,
// a wedged credential helper. Every consumer treats that as "use forge's own
// defaults" rather than as an error, because a machine that cannot answer the
// question is exactly the machine where the old hardcoded values were fine.
type HostGoEnv struct {
	GOPROXY    string
	GOPRIVATE  string
	GONOPROXY  string
	GONOSUMDB  string
	GOSUMDB    string
	GOINSECURE string

	Resolved bool
}

// hostProbeTimeout bounds the `go env` probe.
//
// The probe runs once, during New, and `go env` can block: a credential helper
// that opens a GUI prompt, a proxy that accepts a connection and never answers.
// Without a bound, forge's startup would inherit that hang, on every command,
// including ones that never build anything.
const hostProbeTimeout = 5 * time.Second

// hostGoEnvVars are the knobs forge adopts from the host. They are the ones
// that decide *where* modules are fetched from and *what is checked* on the way
// in -- not the ones that decide what gets built, which stay forge's own.
var hostGoEnvVars = []string{
	"GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE",
}

// resolveHostGoEnv asks the host toolchain what its effective module
// configuration is.
//
// It runs with the real environment, which is the point: the answer has to
// reflect the user's `go env -w` file and their ambient GOPROXY exactly as
// their own `go build` would see them. Asking the toolchain is better than
// re-deriving the values here, because `go env` already expands GOPRIVATE into
// GONOPROXY and GONOSUMDB, and forge would have to reimplement that rule and
// keep it correct across Go releases.
//
// Every failure returns an unresolved value. None is fatal.
func resolveHostGoEnv(ctx context.Context, goBin string) HostGoEnv {
	if goBin == "" {
		goBin = "go"
	}
	bin, err := exec.LookPath(goBin)
	if err != nil {
		return HostGoEnv{}
	}

	ctx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()

	args := append([]string{"env", "-json"}, hostGoEnvVars...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return HostGoEnv{}
	}

	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		return HostGoEnv{}
	}
	return HostGoEnv{
		GOPROXY:    m["GOPROXY"],
		GOPRIVATE:  m["GOPRIVATE"],
		GONOPROXY:  m["GONOPROXY"],
		GONOSUMDB:  m["GONOSUMDB"],
		GOSUMDB:    m["GOSUMDB"],
		GOINSECURE: m["GOINSECURE"],
		Resolved:   true,
	}
}

// buildEnv constructs the environment for a tool build.
//
// The rule is: forge decides what is built, the machine decides where the
// bytes come from. So the knobs that change the *result* are set here from
// scratch and never inherited, and the knobs that only change *reachability*
// are taken from the host.
//
// Fixed here, whatever the machine says, because inheriting them would change
// what a tool is:
//
//   - GOFLAGS is emptied. A developer with GOFLAGS=-tags something set for
//     their own work would silently change every tool they install.
//
//   - GOWORK is off. A go.work anywhere above the temporary directory captures
//     it and hijacks module resolution. This is not hypothetical: a stray
//     go.work at the root of this very projects directory once broke unrelated
//     builds in a sibling repository.
//
//   - GOTOOLCHAIN is pinned. With the default of auto, a tool's go.mod saying
//     "go 1.99" makes the go command download and execute a different toolchain
//     during an install. That is both a supply-chain hole and a source of
//     builds that differ between machines for reasons nobody can see.
//
//   - GOENV is off. The values that matter have already been resolved out of
//     that file by resolveHostGoEnv, explicitly and visibly; letting the file
//     itself through as well would re-expose GOFLAGS and GOWORK through the
//     back door.
//
//   - GOMODCACHE and GOCACHE point at forge's own state, so the second build
//     is fast without touching the user's caches.
//
// Taken from the host, because forge was previously stricter than the
// developer's own `go build` and so could not build at all on a machine behind
// a private module proxy, an air-gapped mirror or a corporate HTTP proxy:
// GOPROXY, GOPRIVATE, GONOPROXY, GONOSUMDB, GOINSECURE, and GOSUMDB -- which is
// allowed to be off, because on an air-gapped network sum.golang.org is
// unreachable and hardcoding it fails every build.
//
// This does not weaken the artifact. With a committed go.sum and -mod=readonly,
// the checksums decide *which* bytes are acceptable; the proxy only decides
// where they are fetched from. What it does change is that forge no longer
// silently overrides a decision the developer made for their whole machine --
// and `forge doctor` prints the effective values, so the configuration is
// visible rather than assumed.
//
// HOME stays inside forge's state directory. Private modules are fetched by
// `go` shelling out to `git`, which needs the user's insteadOf rewrites, SSH
// agent and netrc -- so exactly those are passed through, by name. Exporting
// the real home instead would hand the build the user's whole configuration
// surface: credential helpers that open GUI prompts, git hooks and templates,
// gnupg, and the go env file this function has deliberately resolved already.
//
// Hermetic restores the strict, machine-independent environment: no host
// values, no passthrough. It is the right setting for CI and for a release
// build, and it is what FORGE_HERMETIC=1 selects.
func buildEnv(cfg Config, extra ...string) []string {
	env := map[string]string{
		"GOOS":        "wasip1",
		"GOARCH":      "wasm",
		"CGO_ENABLED": "0",
		"GOFLAGS":     "",
		"GOWORK":      "off",
		"GOTOOLCHAIN": cfg.Toolchain,
		"GOENV":       "off",
		"GOMODCACHE":  filepath.Join(cfg.StateDir, "gomodcache"),
		"GOCACHE":     filepath.Join(cfg.StateDir, "gocache"),
		"HOME":        cfg.StateDir,
		"PATH":        os.Getenv("PATH"),
	}

	if cfg.Hermetic {
		// Byte for byte what forge set before the host was ever consulted.
		// Keeping it exact is what makes this a usable escape hatch: a build
		// that fails only in transparent mode can be compared against one that
		// differs in nothing but the knobs this issue is about.
		env["GOSUMDB"] = "sum.golang.org"
		env["GOPROXY"] = effectiveProxy(cfg)
		for _, k := range []string{
			"GOPRIVATE", "GONOSUMDB", "GONOSUMCHECK", "GOINSECURE", "GONOSUMVERIFY",
		} {
			env[k] = ""
		}
	} else {
		env["GOPROXY"] = effectiveProxy(cfg)
		env["GOSUMDB"] = "sum.golang.org"
		if cfg.Host.Resolved {
			// Empty is a meaningful answer here -- it is what an ordinary
			// machine reports -- so these are assigned unconditionally rather
			// than only when non-empty.
			env["GOPRIVATE"] = cfg.Host.GOPRIVATE
			env["GONOPROXY"] = cfg.Host.GONOPROXY
			env["GONOSUMDB"] = cfg.Host.GONOSUMDB
			env["GOINSECURE"] = cfg.Host.GOINSECURE
			if cfg.Host.GOSUMDB != "" {
				env["GOSUMDB"] = cfg.Host.GOSUMDB
			}
		}
		addGitIdentity(env)
	}

	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env["TMPDIR"] = tmp
	}
	// Keep the terminal-neutral bits the toolchain genuinely reads.
	for _, k := range []string{"SystemRoot", "ComSpec", "USERPROFILE"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}

	out := make([]string, 0, len(env)+len(extra))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return append(out, extra...)
}

// effectiveProxy decides GOPROXY.
//
// Offline wins over everything: FORGE_OFFLINE is someone saying "do not touch
// the network", and a host proxy is still the network. Hermetic ignores the
// host by definition.
//
// Every caller must go through here. Provenance and EnvInfo both report this
// value, and a version that answered differently from the one buildEnv used
// would make `forge doctor` and a tool's recorded build name a proxy that was
// never contacted.
func effectiveProxy(cfg Config) string {
	if cfg.Proxy == "off" {
		return "off"
	}
	if !cfg.Hermetic && cfg.Host.Resolved && cfg.Host.GOPROXY != "" {
		return cfg.Host.GOPROXY
	}
	return cfg.Proxy
}

// addGitIdentity gives git what it needs to fetch a private module, without
// giving it the rest of the user's home directory.
//
// GIT_CONFIG_GLOBAL and NETRC are pointed at the real files, which is what
// makes insteadOf rewrites and stored credentials work while HOME still points
// at forge's state. The variables below are passed through only when set, so an
// ordinary machine's build environment is unchanged.
func addGitIdentity(env map[string]string) {
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".gitconfig"); fileExists(p) {
			env["GIT_CONFIG_GLOBAL"] = p
		}
		if p := filepath.Join(home, ".netrc"); fileExists(p) {
			env["NETRC"] = p
		}
	}
	for _, k := range []string{
		"SSH_AUTH_SOCK", "GIT_SSH", "GIT_SSH_COMMAND",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
