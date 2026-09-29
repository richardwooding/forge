package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envMap parses what buildEnv returns back into something assertable.
func envMap(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range buildEnv(cfg.withDefaults()) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("not KEY=VALUE: %q", kv)
		}
		out[k] = v
	}
	return out
}

func hostEnv() HostGoEnv {
	return HostGoEnv{
		GOPROXY:   "https://mirror.example/goproxy",
		GOPRIVATE: "corp.example/*",
		GONOPROXY: "corp.example/*",
		GONOSUMDB: "corp.example/*",
		GOSUMDB:   "off",
		Resolved:  true,
	}
}

// TestBuildEnvTransparentUsesHostValues is the issue: forge was stricter than
// the developer's own `go build`, so a machine behind a private mirror could
// not install a tool at all.
func TestBuildEnvTransparentUsesHostValues(t *testing.T) {
	state := t.TempDir()
	env := envMap(t, Config{StateDir: state, Host: hostEnv()})

	for k, want := range map[string]string{
		"GOPROXY":   "https://mirror.example/goproxy",
		"GOPRIVATE": "corp.example/*",
		"GONOPROXY": "corp.example/*",
		"GONOSUMDB": "corp.example/*",
		"GOSUMDB":   "off",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}

	// What forge builds is still forge's decision, and adopting the machine's
	// module configuration must not have loosened any of it.
	for k, want := range map[string]string{
		"GOFLAGS":     "",
		"GOWORK":      "off",
		"GOENV":       "off",
		"GOTOOLCHAIN": "local",
		"GOOS":        "wasip1",
		"GOARCH":      "wasm",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q -- this one is not the machine's to set", k, env[k], want)
		}
	}
	for _, k := range []string{"GOMODCACHE", "GOCACHE"} {
		if !strings.HasPrefix(env[k], state) {
			t.Errorf("%s = %q, want it under forge's state dir %q", k, env[k], state)
		}
	}
	if env["HOME"] != state {
		t.Errorf("HOME = %q, want forge's state dir; the real home is not handed to the build", env["HOME"])
	}
}

// TestBuildEnvGoSumDBMayBeOff is the reason GOSUMDB is adopted at all: on an
// air-gapped network sum.golang.org is unreachable, so hardcoding it failed
// every build there.
func TestBuildEnvGoSumDBMayBeOff(t *testing.T) {
	env := envMap(t, Config{StateDir: t.TempDir(), Host: hostEnv()})
	if env["GOSUMDB"] != "off" {
		t.Errorf("GOSUMDB = %q, want off", env["GOSUMDB"])
	}
}

// TestBuildEnvOfflineWins: FORGE_OFFLINE means do not touch the network, and a
// host proxy is still the network.
func TestBuildEnvOfflineWins(t *testing.T) {
	env := envMap(t, Config{StateDir: t.TempDir(), Proxy: "off", Host: hostEnv()})
	if env["GOPROXY"] != "off" {
		t.Errorf("GOPROXY = %q, want off: offline must beat the host proxy", env["GOPROXY"])
	}
}

// TestBuildEnvHermeticRestoresStrict guards the escape hatch. A build that
// fails only in transparent mode has to be comparable against one that differs
// in nothing else.
func TestBuildEnvHermeticRestoresStrict(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.example")

	env := envMap(t, Config{StateDir: state, Hermetic: true, Host: hostEnv()})

	for k, want := range map[string]string{
		"GOPROXY":   "https://proxy.golang.org,direct",
		"GOSUMDB":   "sum.golang.org",
		"GOPRIVATE": "",
		"GONOSUMDB": "",
		"HOME":      state,
	} {
		if env[k] != want {
			t.Errorf("hermetic %s = %q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{
		"HTTPS_PROXY", "SSH_AUTH_SOCK", "GIT_CONFIG_GLOBAL", "NETRC",
	} {
		if _, ok := env[k]; ok {
			t.Errorf("hermetic build inherited %s; it is meant to inherit nothing", k)
		}
	}
}

// TestBuildEnvSurgicalGitAndProxyPassthrough: private modules are fetched by
// git, which needs the user's rewrites and credentials -- but handing over the
// whole home directory would bring credential helpers, hooks and gnupg with it.
func TestBuildEnvSurgicalGitAndProxyPassthrough(t *testing.T) {
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	netrc := filepath.Join(home, ".netrc")
	for _, p := range []string{gitconfig, netrc} {
		if err := os.WriteFile(p, []byte("# test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.example")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")

	state := t.TempDir()
	env := envMap(t, Config{StateDir: state, Host: hostEnv()})

	if env["GIT_CONFIG_GLOBAL"] != gitconfig {
		t.Errorf("GIT_CONFIG_GLOBAL = %q, want %q", env["GIT_CONFIG_GLOBAL"], gitconfig)
	}
	if env["NETRC"] != netrc {
		t.Errorf("NETRC = %q, want %q", env["NETRC"], netrc)
	}
	if env["SSH_AUTH_SOCK"] != "/tmp/agent.example" {
		t.Errorf("SSH_AUTH_SOCK = %q, want it passed through", env["SSH_AUTH_SOCK"])
	}
	if env["HTTPS_PROXY"] != "http://proxy.example:3128" {
		t.Errorf("HTTPS_PROXY = %q, want it passed through", env["HTTPS_PROXY"])
	}
	// The whole point of doing it by name: HOME itself is not the real one.
	if env["HOME"] == home {
		t.Error("HOME is the user's real home; only the named git files should cross over")
	}
	if env["HOME"] != state {
		t.Errorf("HOME = %q, want forge's state dir %q", env["HOME"], state)
	}
}

// TestBuildEnvAbsentGitFilesAreNotInvented: pointing GIT_CONFIG_GLOBAL at a
// file that does not exist is not the same as leaving it unset, and git treats
// the two differently.
func TestBuildEnvAbsentGitFilesAreNotInvented(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	env := envMap(t, Config{StateDir: t.TempDir(), Host: hostEnv()})
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "NETRC"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q, but no such file exists", k, v)
		}
	}
}

// TestBuildEnvUnresolvedFallsBackToDefaults: a machine that cannot answer is
// the machine where forge's old hardcoded values were always fine.
func TestBuildEnvUnresolvedFallsBackToDefaults(t *testing.T) {
	env := envMap(t, Config{StateDir: t.TempDir()})
	if env["GOPROXY"] != "https://proxy.golang.org,direct" {
		t.Errorf("GOPROXY = %q, want the default", env["GOPROXY"])
	}
	if env["GOSUMDB"] != "sum.golang.org" {
		t.Errorf("GOSUMDB = %q, want the default", env["GOSUMDB"])
	}
	if env["GOPRIVATE"] != "" {
		t.Errorf("GOPRIVATE = %q, want empty when nothing was resolved", env["GOPRIVATE"])
	}
}

// TestResolveHostGoEnvFailsSoft: no toolchain must leave forge on its defaults,
// not panic and not error. This is the laptop with no Go installed, which can
// still run every tool it already has.
func TestResolveHostGoEnvFailsSoft(t *testing.T) {
	got := resolveHostGoEnv(context.Background(), filepath.Join(t.TempDir(), "definitely-not-go"))
	if got.Resolved {
		t.Errorf("resolved from a nonexistent toolchain: %+v", got)
	}
}

// TestEnvInfoReportsWhatWasDecided backs the doctor output: it has to describe
// the environment forge will use, not the one it was configured with.
func TestEnvInfoReportsWhatWasDecided(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")

	transparent := (&Builder{cfg: Config{Host: hostEnv()}.withDefaults()}).EnvInfo()
	if transparent.GoProxy != "https://mirror.example/goproxy" {
		t.Errorf("GoProxy = %q, want the host's", transparent.GoProxy)
	}
	if transparent.GoSumDB != "off" {
		t.Errorf("GoSumDB = %q, want off", transparent.GoSumDB)
	}
	if transparent.Private != "corp.example/*" {
		t.Errorf("Private = %q, want the host's", transparent.Private)
	}
	if transparent.HTTPProxy == "" {
		t.Error("HTTPProxy is empty though one is set")
	}

	hermetic := (&Builder{cfg: Config{Hermetic: true, Host: hostEnv()}.withDefaults()}).EnvInfo()
	if hermetic.GoProxy != "https://proxy.golang.org,direct" {
		t.Errorf("hermetic GoProxy = %q, want the default", hermetic.GoProxy)
	}
	if hermetic.GoSumDB != "sum.golang.org" {
		t.Errorf("hermetic GoSumDB = %q, want the default", hermetic.GoSumDB)
	}
	if hermetic.HTTPProxy != "" {
		t.Errorf("hermetic HTTPProxy = %q, want nothing reported", hermetic.HTTPProxy)
	}
}
