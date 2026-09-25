package build

import (
	"os"
	"path/filepath"
	"sort"
)

// buildEnv constructs the environment for a tool build from scratch.
//
// Nothing is inherited that could change the result. Every entry below is here
// because leaving it to the ambient environment would either break the build,
// make it unreproducible, or weaken it:
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
//   - GOSUMDB stays on and GOPRIVATE, GONOSUMDB, GONOSUMCHECK and GOINSECURE
//     are cleared. A developer with GOPRIVATE=* set for their employer's
//     modules would otherwise disable checksum verification for everything a
//     tool pulls in.
//
//   - GOENV is off, so `go env -w` settings do not leak in either.
//
//   - GOMODCACHE and GOCACHE point at forge's own state, so the second build
//     is fast without touching the user's caches.
func buildEnv(cfg Config, extra ...string) []string {
	env := map[string]string{
		"GOOS":          "wasip1",
		"GOARCH":        "wasm",
		"CGO_ENABLED":   "0",
		"GOFLAGS":       "",
		"GOWORK":        "off",
		"GOTOOLCHAIN":   cfg.Toolchain,
		"GOENV":         "off",
		"GOSUMDB":       "sum.golang.org",
		"GOPRIVATE":     "",
		"GONOSUMDB":     "",
		"GONOSUMCHECK":  "",
		"GOINSECURE":    "",
		"GONOSUMVERIFY": "",
		"GOPROXY":       cfg.Proxy,
		"GOMODCACHE":    filepath.Join(cfg.StateDir, "gomodcache"),
		"GOCACHE":       filepath.Join(cfg.StateDir, "gocache"),
		"HOME":          cfg.StateDir,
		"PATH":          os.Getenv("PATH"),
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
