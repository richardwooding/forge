package hostsvc_test

// This file runs the capability host functions the way production does:
// a Go tool compiled to wasip1 by the real toolchain, calling the real SDK,
// through the real host module, against the real services.
//
// The unit tests beside it check each service on its own. Only this one proves
// the ABI -- the size-then-fetch round trip, the envelope, the denial type --
// because a mock host would be a second implementation of the thing under
// test, and would agree with itself whatever the guest actually does.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/hostsvc"
	"github.com/richardwooding/forge/internal/wasmrt"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// buildCapGuest compiles testdata/capguest against the SDK in this checkout.
func buildCapGuest(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles a wasm guest with the Go toolchain; skipped in -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}

	sdk, err := filepath.Abs(filepath.Join("..", "..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	for name, dst := range map[string]string{
		"main.go.txt": "main.go",
		"go.mod.txt":  "go.mod",
		"go.sum.txt":  "go.sum",
	} {
		b, err := os.ReadFile(filepath.Join("testdata", "capguest", name))
		if err != nil {
			t.Fatal(err)
		}
		// The guest is built against the SDK as it is right now, not as it was
		// published. A test that compiled against a tagged SDK would go on
		// passing after a change to the guest ABI broke it.
		b = bytes.ReplaceAll(b, []byte("SDK_PATH"), []byte(sdk))
		if err := os.WriteFile(filepath.Join(src, dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "capguest.wasm")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "build", "-buildmode=c-shared",
		"-trimpath", "-buildvcs=false", "-o", out, "./")
	cmd.Dir = src
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

type guestOut struct {
	Result string `json:"result"`
	Denied bool   `json:"denied"`
	Code   string `json:"code"`
}

// runner compiles the guest once and invokes it many times.
type runner struct {
	engine   *wasmrt.Engine
	compiled *wasmrt.Compiled
	services hostabi.Services
	grants   capability.Set
}

func newRunner(t *testing.T, services hostabi.Services, grants capability.Set) *runner {
	t.Helper()
	wasm := buildCapGuest(t)

	ctx := context.Background()
	e, err := wasmrt.NewEngine(ctx, wasmrt.Config{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	c, err := e.Compile(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	return &runner{engine: e, compiled: c, services: services, grants: grants}
}

func (r *runner) run(t *testing.T, do, arg string) guestOut {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	input, err := json.Marshal(map[string]string{"do": do, "arg": arg})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.engine.Invoke(ctx, r.compiled, wasmrt.Request{
		Op:    "run",
		Input: input,
		Opts: wasmrt.Options{
			Tool:     "capguest",
			Grants:   r.grants,
			Services: r.services,
			Timeout:  25 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("invoking %q: %v", do, err)
	}
	var out guestOut
	if err := json.Unmarshal(resp.Output, &out); err != nil {
		t.Fatalf("decoding the result of %q from %s: %v", do, resp.Output, err)
	}
	return out
}

// grantSet builds a grant set from kind/subject pairs.
func grantSet(t *testing.T, pairs ...[2]string) capability.Set {
	t.Helper()
	var grants []capability.Grant
	for _, p := range pairs {
		grants = append(grants, capability.Grant{
			Kind:  capability.Kind(p[0]),
			Scope: []string{p[1]},
		})
	}
	return capability.NewSet(grants...)
}

func TestGuestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Error("an Authorization header reached the server; the host should have refused it")
		}
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	// A grant names a host, not a host and port, so the port httptest chose
	// is stripped here as the service strips it.
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	services := hostabi.Services{
		// httptest listens on loopback, which the screening exists to refuse,
		// so this is the one place that is turned off deliberately.
		HTTP: hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true}),
	}
	r := newRunner(t, services, grantSet(t, [2]string{"net.http", host}))

	t.Run("a granted host is reached", func(t *testing.T) {
		got := r.run(t, "http", srv.URL)
		if got.Denied {
			t.Fatalf("the request was denied: %s", got.Result)
		}
		if got.Result != "200:pong" {
			t.Errorf("got %q, want %q", got.Result, "200:pong")
		}
	})

	t.Run("an ungranted host is refused", func(t *testing.T) {
		got := r.run(t, "http", "https://example.com/")
		if !got.Denied {
			t.Fatalf("reaching an ungranted host was allowed: %s", got.Result)
		}
		if !strings.Contains(got.Result, "net.http") {
			t.Errorf("the denial does not name the capability: %s", got.Result)
		}
		// out_of_scope, because the tool does hold a net.http grant -- just
		// not for this host. A tool acts on that differently from a refusal
		// no grant could lift.
		if got.Code != "out_of_scope" {
			t.Errorf("got code %q, want out_of_scope", got.Code)
		}
	})

	t.Run("an address forge never permits reports the floor", func(t *testing.T) {
		// Granted "*", so the only thing that can refuse this is the address
		// screening -- and it must say so with a code that tells the tool not
		// to suggest widening a grant, because no grant would help.
		//
		// Note this runs with the same AllowPrivate service as the rest of the
		// file, which is the point: that switch unblocks loopback for a local
		// test, and must not unblock the metadata endpoint along with it.
		wide := newRunner(t, services, grantSet(t, [2]string{"net.http", "*"}))
		got := wide.run(t, "http", "https://169.254.169.254/latest/meta-data/")
		if !got.Denied {
			t.Fatalf("the metadata endpoint was reachable: %s", got.Result)
		}
		if got.Code != "floor" {
			t.Errorf("got code %q, want floor", got.Code)
		}
	})

	t.Run("a guest-set Authorization header is refused", func(t *testing.T) {
		got := r.run(t, "http-headers", srv.URL)
		if !got.Denied {
			t.Fatalf("the header was accepted: %s", got.Result)
		}
		if !strings.Contains(got.Result, "secret") {
			t.Errorf("the denial should point at the secret capability: %s", got.Result)
		}
		if got.Code != "floor" {
			t.Errorf("got code %q, want floor: no grant permits a smuggled header", got.Code)
		}
	})
}

func TestGuestKV(t *testing.T) {
	services := hostabi.Services{
		KV: hostsvc.NewKV(hostsvc.KVConfig{Dir: t.TempDir()}),
	}
	r := newRunner(t, services, grantSet(t, [2]string{"kv", "notes"}))

	t.Run("a granted namespace round-trips", func(t *testing.T) {
		got := r.run(t, "kv", "")
		if got.Denied {
			t.Fatalf("the granted namespace was refused: %s", got.Result)
		}
		if want := "hello/true/[greeting]"; got.Result != want {
			t.Errorf("got %q, want %q", got.Result, want)
		}
	})

	t.Run("an ungranted namespace is refused", func(t *testing.T) {
		got := r.run(t, "kv-forbidden", "")
		if !got.Denied {
			t.Fatalf("an ungranted namespace was readable: %s", got.Result)
		}
	})
}

func TestGuestSecret(t *testing.T) {
	dir := t.TempDir()
	secrets := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: dir})
	if err := secrets.Put("token", "s3cret-value"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Put("other", "not-yours"); err != nil {
		t.Fatal(err)
	}

	r := newRunner(t, hostabi.Services{Secrets: secrets},
		grantSet(t, [2]string{"secret", "token"}))

	t.Run("a granted secret is readable", func(t *testing.T) {
		got := r.run(t, "secret", "token")
		if got.Denied {
			t.Fatalf("the granted secret was refused: %s", got.Result)
		}
		if want := "s3cret-value/true"; got.Result != want {
			t.Errorf("got %q, want %q", got.Result, want)
		}
	})

	t.Run("an ungranted secret is refused", func(t *testing.T) {
		got := r.run(t, "secret", "other")
		if !got.Denied {
			t.Fatalf("an ungranted secret was readable: %s", got.Result)
		}
		if strings.Contains(got.Result, "not-yours") {
			t.Fatal("the denial leaked the secret value")
		}
	})

	t.Run("a secret with no grant at all is refused", func(t *testing.T) {
		bare := newRunner(t, hostabi.Services{Secrets: secrets}, capability.Set{})
		got := bare.run(t, "secret", "token")
		if !got.Denied {
			t.Fatalf("a tool with no secret grant read one: %s", got.Result)
		}
	})
}

// TestGuestUnconfiguredService checks that a capability forge cannot provide
// reads as "not configured" rather than as a refusal. A tool author chasing a
// grant they already have is a bad afternoon.
func TestGuestUnconfiguredService(t *testing.T) {
	r := newRunner(t, hostabi.Services{}, grantSet(t, [2]string{"kv", "notes"}))
	got := r.run(t, "kv", "")
	if got.Denied {
		t.Fatalf("an unconfigured service was reported as a denial: %s", got.Result)
	}
	if !strings.Contains(got.Result, "configured") {
		t.Errorf("the message should say the store is not configured: %s", got.Result)
	}
}
