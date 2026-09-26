package hostsvc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/hostsvc"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

func anyHost(t *testing.T) capability.Set {
	t.Helper()
	return capability.NewSet(capability.Grant{Kind: capability.NetHTTP, Scope: []string{"*"}})
}

// TestHTTPRefusesInternalAddresses is the SSRF case. The grant is deliberately
// wide open -- "*" -- so that what is being tested is the address screening
// and nothing else. A tool allowed to reach "anything" still may not reach the
// metadata endpoint or the machine forge is running on.
func TestHTTPRefusesInternalAddresses(t *testing.T) {
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{})
	grants := anyHost(t)

	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"https://127.0.0.1/",                       // loopback
		"https://localhost/",                       // loopback by name
		"https://10.0.0.1/",                        // RFC 1918
		"https://192.168.1.1/",                     // RFC 1918
		"https://172.16.0.1/",                      // RFC 1918
		"https://[::1]/",                           // loopback, v6
		"https://[fd00::1]/",                       // unique local, v6
		"https://0.0.0.0/",                         // unspecified
	} {
		t.Run(url, func(t *testing.T) {
			_, err := svc.Do(context.Background(), "t", grants, hostabi.HTTPRequest{URL: url})
			if err == nil {
				t.Fatalf("%s was allowed", url)
			}
		})
	}
}

func TestHTTPRefusesNonHTTPSchemes(t *testing.T) {
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{})
	grants := anyHost(t)

	for _, url := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/",
		"/etc/passwd",
		"example.com",
		"",
	} {
		t.Run(url, func(t *testing.T) {
			if _, err := svc.Do(context.Background(), "t", grants, hostabi.HTTPRequest{URL: url}); err == nil {
				t.Fatalf("%q was accepted", url)
			}
		})
	}
}

// TestHTTPRefusesPlainHTTP checks that a grant for a host is not also a
// decision to talk to it in clear.
func TestHTTPRefusesPlainHTTP(t *testing.T) {
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{})
	_, err := svc.Do(context.Background(), "t", anyHost(t),
		hostabi.HTTPRequest{URL: "http://example.com/"})
	if err == nil {
		t.Fatal("plain http was allowed")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the message should explain that forge uses https: %v", err)
	}
}

func TestHTTPRefusesSmuggledHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true})

	for _, header := range []string{
		"Authorization", "authorization", "Cookie", "Host",
		"Proxy-Authorization", "Proxy-Connection", "proxy-anything",
	} {
		t.Run(header, func(t *testing.T) {
			_, err := svc.Do(context.Background(), "t", anyHost(t), hostabi.HTTPRequest{
				URL:     srv.URL,
				Headers: map[string]string{header: "x"},
			})
			if err == nil {
				t.Fatalf("%s was accepted", header)
			}
		})
	}
}

// TestHTTPDoesNotFollowRedirects checks that a permitted host cannot bounce a
// tool somewhere the user never allowed.
func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true})
	res, err := svc.Do(context.Background(), "t", anyHost(t), hostabi.HTTPRequest{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if reached {
		t.Error("the redirect was followed; every hop must be re-checked by the guest asking again")
	}
	if res.Status != http.StatusFound {
		t.Errorf("got status %d, want %d so the guest can see the redirect", res.Status, http.StatusFound)
	}
	if res.Headers["Location"] == "" {
		t.Error("the Location header should reach the guest so it can choose to follow")
	}
}

func TestHTTPTruncatesLargeBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true, MaxBodyBytes: 100})
	res, err := svc.Do(context.Background(), "t", anyHost(t), hostabi.HTTPRequest{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != 100 {
		t.Errorf("got %d bytes, want the 100-byte cap", len(res.Body))
	}
	if !res.Truncated {
		t.Error("Truncated is not set; a tool would parse half a document as whole")
	}
}

// TestHTTPRefusesUngrantedHostEvenWhenResolvable is the ordering check: the
// grant is tested before the address screening, so a host the user never
// allowed is refused whatever it resolves to.
func TestHTTPRefusesUngrantedHost(t *testing.T) {
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true})
	grants := capability.NewSet(capability.Grant{
		Kind: capability.NetHTTP, Scope: []string{"allowed.example"},
	})
	_, err := svc.Do(context.Background(), "t", grants,
		hostabi.HTTPRequest{URL: "https://other.example/"})
	if err == nil {
		t.Fatal("an ungranted host was allowed")
	}
	if _, ok := capability.AsDenial(err); !ok {
		t.Errorf("an ungranted host should read as a denial, not a failure: %v", err)
	}
}

// --- KV -----------------------------------------------------------------

func TestKVRoundTrip(t *testing.T) {
	kv := hostsvc.NewKV(hostsvc.KVConfig{Dir: t.TempDir()})
	ctx := context.Background()

	if _, found, err := kv.Get(ctx, "tool", "ns", "missing"); err != nil || found {
		t.Fatalf("a missing key: found=%v err=%v", found, err)
	}
	if err := kv.Set(ctx, "tool", "ns", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	v, found, err := kv.Get(ctx, "tool", "ns", "k")
	if err != nil || !found || string(v) != "v" {
		t.Fatalf("got %q found=%v err=%v", v, found, err)
	}

	// An empty value is not a missing key.
	if err := kv.Set(ctx, "tool", "ns", "empty", []byte{}); err != nil {
		t.Fatal(err)
	}
	if v, found, err := kv.Get(ctx, "tool", "ns", "empty"); err != nil || !found || len(v) != 0 {
		t.Fatalf("an empty value should be found: %q found=%v err=%v", v, found, err)
	}

	if err := kv.Delete(ctx, "tool", "ns", "k"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := kv.Get(ctx, "tool", "ns", "k"); found {
		t.Error("the key survived deletion")
	}
	// Deleting twice is not an error: the caller's intent is satisfied.
	if err := kv.Delete(ctx, "tool", "ns", "k"); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
}

func TestKVIsolatesToolsAndNamespaces(t *testing.T) {
	kv := hostsvc.NewKV(hostsvc.KVConfig{Dir: t.TempDir()})
	ctx := context.Background()

	if err := kv.Set(ctx, "a", "ns", "k", []byte("from a")); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := kv.Get(ctx, "b", "ns", "k"); found {
		t.Error("one tool read another tool's value")
	}
	if _, found, _ := kv.Get(ctx, "a", "other", "k"); found {
		t.Error("one namespace read another namespace's value")
	}
}

// TestKVRefusesPathTricks checks that no guest-supplied string is ever
// interpreted as a path. Keys are encoded, so they cannot escape at all; tool
// and namespace names are validated, so they cannot either.
func TestKVRefusesPathTricks(t *testing.T) {
	dir := t.TempDir()
	kv := hostsvc.NewKV(hostsvc.KVConfig{Dir: dir})
	ctx := context.Background()

	canary := filepath.Join(dir, "canary")
	if err := os.WriteFile(canary, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"..", "../..", "a/b", "/abs", ".", "a\x00b", ""} {
		t.Run("namespace "+bad, func(t *testing.T) {
			if err := kv.Set(ctx, "tool", bad, "k", []byte("x")); err == nil {
				t.Errorf("namespace %q was accepted", bad)
			}
		})
		t.Run("tool "+bad, func(t *testing.T) {
			if err := kv.Set(ctx, bad, "ns", "k", []byte("x")); err == nil {
				t.Errorf("tool %q was accepted", bad)
			}
		})
	}

	// A key holding separators is fine: it is encoded, never used as a path.
	for _, key := range []string{"../../canary", "/etc/passwd", "a/b/c"} {
		t.Run("key "+key, func(t *testing.T) {
			if err := kv.Set(ctx, "tool", "ns", key, []byte("written")); err != nil {
				t.Fatalf("an encoded key should be storable: %v", err)
			}
			v, found, err := kv.Get(ctx, "tool", "ns", key)
			if err != nil || !found || string(v) != "written" {
				t.Fatalf("got %q found=%v err=%v", v, found, err)
			}
		})
	}

	if b, err := os.ReadFile(canary); err != nil || string(b) != "original" {
		t.Fatalf("the canary was touched: %q %v", b, err)
	}
}

func TestKVListIsSortedAndPrefixed(t *testing.T) {
	kv := hostsvc.NewKV(hostsvc.KVConfig{Dir: t.TempDir()})
	ctx := context.Background()
	for _, k := range []string{"b:2", "a:1", "b:1", "c:1"} {
		if err := kv.Set(ctx, "tool", "ns", k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := kv.List(ctx, "tool", "ns", "b:")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "b:1,b:2" {
		t.Errorf("got %v, want [b:1 b:2]", got)
	}
	// A namespace that was never written lists empty rather than failing.
	if got, err := kv.List(ctx, "tool", "fresh", ""); err != nil || len(got) != 0 {
		t.Errorf("an unused namespace: %v %v", got, err)
	}
}

func TestKVEnforcesLimits(t *testing.T) {
	kv := hostsvc.NewKV(hostsvc.KVConfig{Dir: t.TempDir(), MaxValueBytes: 10, MaxKeys: 2})
	ctx := context.Background()

	if err := kv.Set(ctx, "tool", "ns", "k", make([]byte, 11)); !errors.Is(err, hostsvc.ErrValueTooLarge) {
		t.Errorf("an oversized value: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := kv.Set(ctx, "tool", "ns", k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := kv.Set(ctx, "tool", "ns", "c", []byte("x")); !errors.Is(err, hostsvc.ErrTooManyKeys) {
		t.Errorf("a third key in a two-key namespace: %v", err)
	}
	// A full namespace stays usable for what is already in it.
	if err := kv.Set(ctx, "tool", "ns", "a", []byte("y")); err != nil {
		t.Errorf("overwriting an existing key in a full namespace: %v", err)
	}
}

// --- Secrets ------------------------------------------------------------

func TestSecretsRoundTrip(t *testing.T) {
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: t.TempDir()})
	ctx := context.Background()

	if _, found, err := s.Secret(ctx, "t", "absent"); err != nil || found {
		t.Fatalf("a missing secret: found=%v err=%v", found, err)
	}
	if err := s.Put("token", "value"); err != nil {
		t.Fatal(err)
	}
	v, found, err := s.Secret(ctx, "t", "token")
	if err != nil || !found || v != "value" {
		t.Fatalf("got %q found=%v err=%v", v, found, err)
	}

	names, err := s.Names()
	if err != nil || len(names) != 1 || names[0] != "token" {
		t.Fatalf("got %v %v", names, err)
	}
	if err := s.Remove("token"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Secret(ctx, "t", "token"); found {
		t.Error("the secret survived removal")
	}
}

// TestSecretsTrimsTrailingNewline covers `echo value > file`, which is how
// most secrets get onto disk.
func TestSecretsTrimsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: dir})
	if v, _, err := s.Secret(context.Background(), "t", "token"); err != nil || v != "value" {
		t.Errorf("got %q %v, want %q", v, err, "value")
	}
}

func TestSecretsRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: dir})
	_, _, err := s.Secret(context.Background(), "t", "token")
	if !errors.Is(err, hostsvc.ErrSecretPermissions) {
		t.Fatalf("a world-readable secret was served: %v", err)
	}
	if strings.Contains(err.Error(), "value") {
		t.Error("the error leaked the secret")
	}
}

// TestSecretsRefusesSymlink stops whoever can write the secrets directory from
// pointing forge at any file the user can read.
func TestSecretsRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(elsewhere, []byte("not for tools"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "token")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: dir})
	if _, _, err := s.Secret(context.Background(), "t", "token"); err == nil {
		t.Fatal("a symlinked secret was read")
	}
}

func TestSecretsRefusesPathTricks(t *testing.T) {
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: t.TempDir()})
	for _, bad := range []string{"../token", "/etc/passwd", "..", "a/b", ""} {
		if _, _, err := s.Secret(context.Background(), "t", bad); err == nil {
			t.Errorf("the name %q was accepted", bad)
		}
	}
}

func TestSecretsRedact(t *testing.T) {
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: t.TempDir()})
	if err := s.Put("token", "swordfish-1234"); err != nil {
		t.Fatal(err)
	}
	// Nothing is redacted until it has actually been handed out; forge does
	// not read every secret on disk just to know what to hide.
	if got := s.Redact("saw swordfish-1234"); got != "saw swordfish-1234" {
		t.Errorf("got %q before the secret was read", got)
	}
	if _, _, err := s.Secret(context.Background(), "t", "token"); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Redact("saw swordfish-1234 twice: swordfish-1234"),
		"saw [redacted] twice: [redacted]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSecretsRedactSkipsShortValues: replacing every occurrence of a two-letter
// secret would mangle the output far worse than the leak it prevents.
func TestSecretsRedactShortValues(t *testing.T) {
	dir := t.TempDir()
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: dir})
	if err := s.Put("tiny", "ab"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Secret(context.Background(), "t", "tiny"); err != nil {
		t.Fatal(err)
	}
	if got := s.Redact("a table of absolutes"); got != "a table of absolutes" {
		t.Errorf("a short secret mangled the text: %q", got)
	}
}

func TestSecretsAudit(t *testing.T) {
	dir := t.TempDir()
	var seen []hostsvc.SecretAccess
	s := hostsvc.NewSecrets(hostsvc.SecretsConfig{
		Dir:   dir,
		Audit: func(a hostsvc.SecretAccess) { seen = append(seen, a) },
	})
	if err := s.Put("token", "value"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _, _ = s.Secret(ctx, "toolA", "token")
	_, _, _ = s.Secret(ctx, "toolB", "absent")

	if len(seen) != 2 {
		t.Fatalf("got %d audit records, want 2 -- a lookup that finds nothing is worth recording too", len(seen))
	}
	if seen[0] != (hostsvc.SecretAccess{Tool: "toolA", Name: "token", Found: true}) {
		t.Errorf("got %+v", seen[0])
	}
	if seen[1] != (hostsvc.SecretAccess{Tool: "toolB", Name: "absent", Found: false}) {
		t.Errorf("got %+v", seen[1])
	}
}

// TestHTTPKeepsLinkLocalBlockedUnderAllowPrivate is the one refusal that must
// survive the escape hatch. AllowPrivate exists so a tool can reach a service
// on this machine; if it also opened 169.254.169.254 then every local-dev
// setup would be one tool away from handing out cloud credentials.
func TestHTTPKeepsLinkLocalBlockedUnderAllowPrivate(t *testing.T) {
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true})

	for _, url := range []string{
		"https://169.254.169.254/latest/meta-data/",
		"https://169.254.170.2/v2/credentials/",
		"https://[fe80::1]/",
	} {
		t.Run(url, func(t *testing.T) {
			_, err := svc.Do(context.Background(), "t", anyHost(t), hostabi.HTTPRequest{URL: url})
			if err == nil {
				t.Fatalf("%s was reachable with AllowPrivate set", url)
			}
			d, ok := capability.AsDenial(err)
			if !ok {
				t.Fatalf("a blocked address should read as a denial: %v", err)
			}
			if d.Code != capability.DenyFloor {
				t.Errorf("got code %q, want %q: no grant can lift this",
					d.Code, capability.DenyFloor)
			}
		})
	}

	// Loopback is still reachable, or the escape hatch would not work at all.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if _, err := svc.Do(context.Background(), "t", anyHost(t), hostabi.HTTPRequest{URL: srv.URL}); err != nil {
		t.Errorf("loopback should be reachable under AllowPrivate: %v", err)
	}
}
