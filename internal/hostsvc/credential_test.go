package hostsvc_test

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/hostsvc"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// credentialServer records the headers each request arrived with.
func credentialServer(t *testing.T) (*httptest.Server, string, *[]http.Header) {
	t.Helper()
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
	}))
	t.Cleanup(srv.Close)
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return srv, host, &seen
}

func credentialService(t *testing.T, secrets *hostsvc.Secrets) *hostsvc.HTTP {
	t.Helper()
	return hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true, Credentials: secrets})
}

func newSecrets(t *testing.T, audit func(hostsvc.SecretAccess)) *hostsvc.Secrets {
	t.Helper()
	return hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: t.TempDir(), Audit: audit})
}

func grantsFor(host string, secrets ...string) capability.Set {
	return capability.NewSet(
		capability.Grant{Kind: capability.NetHTTP, Scope: []string{host}},
		capability.Grant{Kind: capability.Secret, Scope: secrets},
	)
}

func TestCredentialIsAttachedByTheHost(t *testing.T) {
	srv, host, seen := credentialServer(t)
	secrets := newSecrets(t, nil)
	if err := secrets.Put("token", "tok-123456"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Put("login", "user:pass"); err != nil {
		t.Fatal(err)
	}
	svc := credentialService(t, secrets)
	grants := grantsFor(host, "token", "login")

	for _, tc := range []struct {
		name   string
		cred   hostabi.HTTPCredential
		header string
		want   string
	}{
		{"bearer by default", hostabi.HTTPCredential{Secret: "token"}, "Authorization", "Bearer tok-123456"},
		{"explicit bearer", hostabi.HTTPCredential{Secret: "token", Scheme: "Bearer"}, "Authorization", "Bearer tok-123456"},
		{"basic", hostabi.HTTPCredential{Secret: "login", Scheme: "Basic"}, "Authorization",
			"Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))},
		{"raw custom header", hostabi.HTTPCredential{Secret: "token", Header: "X-Api-Key"}, "X-Api-Key", "tok-123456"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*seen = nil
			cred := tc.cred
			if _, err := svc.Do(context.Background(), "t", grants,
				hostabi.HTTPRequest{URL: srv.URL, Credential: &cred}); err != nil {
				t.Fatal(err)
			}
			if len(*seen) != 1 {
				t.Fatalf("server saw %d requests, want 1", len(*seen))
			}
			if got := (*seen)[0].Get(tc.header); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestCredentialRefusals(t *testing.T) {
	srv, host, seen := credentialServer(t)
	secrets := newSecrets(t, nil)
	for name, value := range map[string]string{"token": "tok-123456", "bound": "bound-654321", "other": "other-000000"} {
		if err := secrets.Put(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.Bind("bound", []string{"graph.facebook.com"}); err != nil {
		t.Fatal(err)
	}
	svc := credentialService(t, secrets)
	grants := grantsFor(host, "token", "bound", "missing")

	for _, tc := range []struct {
		name    string
		req     hostabi.HTTPRequest
		code    capability.DenyCode
		message string
	}{
		{"ungranted secret", hostabi.HTTPRequest{Credential: &hostabi.HTTPCredential{Secret: "other"}},
			capability.DenyOutOfScope, "other"},
		{"bound elsewhere", hostabi.HTTPRequest{Credential: &hostabi.HTTPCredential{Secret: "bound"}},
			capability.DenyOutOfScope, "graph.facebook.com"},
		{"as Host", hostabi.HTTPRequest{Credential: &hostabi.HTTPCredential{Secret: "token", Header: "Host"}},
			capability.DenyFloor, "Host"},
		{"as a proxy header", hostabi.HTTPRequest{Credential: &hostabi.HTTPCredential{Secret: "token", Header: "Proxy-Authorization"}},
			capability.DenyFloor, "Proxy-Authorization"},
		{"duplicated by a guest header", hostabi.HTTPRequest{
			Headers:    map[string]string{"x-api-key": "mine"},
			Credential: &hostabi.HTTPCredential{Secret: "token", Header: "X-Api-Key"}},
			capability.DenyNone, "both"},
		{"missing secret", hostabi.HTTPRequest{Credential: &hostabi.HTTPCredential{Secret: "missing"}},
			capability.DenyNone, "forge secret set missing"},
		{"unknown scheme", hostabi.HTTPRequest{Credential: &hostabi.HTTPCredential{Secret: "token", Scheme: "Digest"}},
			capability.DenyNone, "Digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*seen = nil
			tc.req.URL = srv.URL
			_, err := svc.Do(context.Background(), "t", grants, tc.req)
			if err == nil {
				t.Fatal("the request was allowed")
			}
			if len(*seen) != 0 {
				t.Error("the server was reached despite the refusal")
			}
			d, isDenial := capability.AsDenial(err)
			if tc.code == capability.DenyNone {
				if isDenial {
					t.Errorf("got a %s denial, want a plain failure: %v", d.Code, err)
				}
			} else if !isDenial || d.Code != tc.code {
				t.Errorf("got %v, want a %s denial", err, tc.code)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not mention %q", err, tc.message)
			}
			for _, v := range []string{"tok-123456", "bound-654321", "other-000000"} {
				if strings.Contains(err.Error(), v) {
					t.Fatal("the refusal leaked a secret value")
				}
			}
		})
	}
}

func TestCredentialWithoutSourceFails(t *testing.T) {
	srv, host, seen := credentialServer(t)
	svc := hostsvc.NewHTTP(hostsvc.HTTPConfig{AllowPrivate: true})
	_, err := svc.Do(context.Background(), "t", grantsFor(host, "token"),
		hostabi.HTTPRequest{URL: srv.URL, Credential: &hostabi.HTTPCredential{Secret: "token"}})
	if err == nil || !strings.Contains(err.Error(), "configured") {
		t.Fatalf("got %v, want an error saying no secret source is configured", err)
	}
	if len(*seen) != 0 {
		t.Error("the request went out without its credential")
	}
}

func TestBoundCredentialReachesItsHost(t *testing.T) {
	secrets := newSecrets(t, nil)
	if err := secrets.Put("token", "tok-123456"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Bind("token", []string{"*.facebook.com", "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, host := range []string{"graph.facebook.com", "api.example.com", "API.EXAMPLE.COM"} {
		if _, err := secrets.Credential(ctx, "t", "token", host); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	// "*." never covers the bare domain, exactly as with net.http grants.
	for _, host := range []string{"facebook.com", "evil.example.com", "api.example.com.evil.net"} {
		if _, err := secrets.Credential(ctx, "t", "token", host); err == nil {
			t.Errorf("%s was allowed", host)
		}
	}
}

func TestCredentialReadIsAudited(t *testing.T) {
	var log []hostsvc.SecretAccess
	secrets := newSecrets(t, func(a hostsvc.SecretAccess) { log = append(log, a) })
	if err := secrets.Put("token", "tok-123456"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Bind("token", []string{"api.example.com"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := secrets.Credential(ctx, "whatsapp", "token", "api.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Credential(ctx, "whatsapp", "token", "elsewhere.com"); err == nil {
		t.Fatal("a bound secret was released to another host")
	}
	want := []hostsvc.SecretAccess{{Tool: "whatsapp", Name: "token", Found: true}}
	if !slices.Equal(log, want) {
		t.Errorf("audit = %+v, want %+v: a refused binding must not read the value", log, want)
	}
}

func TestSecretBindingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	secrets := hostsvc.NewSecrets(hostsvc.SecretsConfig{Dir: dir})

	if err := secrets.Bind("token", []string{"B.example.com", "a.example.com", "a.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Put("token", "tok-123456"); err != nil {
		t.Fatal(err)
	}
	hosts, err := secrets.Hosts("token")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.example.com", "b.example.com"}; !slices.Equal(hosts, want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
	if bound, _ := secrets.Bound("token"); !bound {
		t.Error("Bound reported false for a bound secret")
	}

	info, err := os.Stat(filepath.Join(dir, ".hosts", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("binding file is %o, want 600", perm)
	}

	names, err := secrets.Names()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"token"}) {
		t.Errorf("Names() = %v; the binding directory must never be listed as a secret", names)
	}

	if err := secrets.Unbind("token"); err != nil {
		t.Fatal(err)
	}
	if bound, _ := secrets.Bound("token"); bound {
		t.Error("still bound after Unbind")
	}

	if err := secrets.Bind("token", []string{"a.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Remove("token"); err != nil {
		t.Fatal(err)
	}
	if bound, _ := secrets.Bound("token"); bound {
		t.Error("removing a secret left its binding behind for the next secret of that name")
	}
}

func TestSecretBindingRefusesBadPatterns(t *testing.T) {
	secrets := newSecrets(t, nil)
	for _, h := range []string{
		"", "*", "*.", "https://api.example.com", "api.example.com:443",
		"api.example.com/path", "user@api.example.com", "a.*.example.com",
		"-bad.example.com", "api..example.com", "has space.com",
	} {
		if err := secrets.Bind("token", []string{h}); err == nil {
			t.Errorf("%q was accepted as a binding", h)
		}
	}
	if err := secrets.Bind("token", nil); err == nil {
		t.Error("an empty binding was accepted")
	}
	if err := secrets.Bind("../escape", []string{"api.example.com"}); err == nil {
		t.Error("a path-shaped secret name was accepted")
	}
}

func TestEchoedCredentialIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("X-Echo", auth)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Malformed access token ` + strings.TrimPrefix(auth, "Bearer ") + `"}}`))
	}))
	defer srv.Close()
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	secrets := newSecrets(t, nil)
	if err := secrets.Put("token", "tok-123456"); err != nil {
		t.Fatal(err)
	}
	res, err := credentialService(t, secrets).Do(context.Background(), "t", grantsFor(host, "token"),
		hostabi.HTTPRequest{URL: srv.URL, Credential: &hostabi.HTTPCredential{Secret: "token"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Body), "tok-123456") || strings.Contains(res.Headers["X-Echo"], "tok-123456") {
		t.Fatalf("the echoed token reached the guest: body %s, header %q", res.Body, res.Headers["X-Echo"])
	}
	if !strings.Contains(string(res.Body), "Malformed access token [redacted]") {
		t.Errorf("the rest of the body should survive: %s", res.Body)
	}
}

func TestURLCredentialIsSubstitutedByTheHost(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"ok":false,"description":"echo ` + r.URL.RequestURI() + `"}`))
	}))
	defer srv.Close()
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	secrets := newSecrets(t, nil)
	if err := secrets.Put("bot", "123456:AB/C+D"); err != nil {
		t.Fatal(err)
	}
	svc := credentialService(t, secrets)
	grants := grantsFor(host, "bot")
	cred := &hostabi.HTTPCredential{Secret: "bot", In: "url"}

	for _, tc := range []struct{ url, want string }{
		{"/bot{credential}/getMe", "/bot123456:AB%2FC+D/getMe"},
		{"/v1/things?key={credential}&x=1", "/v1/things?key=123456%3AAB%2FC%2BD&x=1"},
	} {
		paths = nil
		res, err := svc.Do(context.Background(), "t", grants, hostabi.HTTPRequest{URL: srv.URL + tc.url, Credential: cred})
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if len(paths) != 1 || paths[0] != tc.want {
			t.Errorf("%s: server saw %q, want %q", tc.url, paths, tc.want)
		}
		if strings.Contains(string(res.Body), "123456") {
			t.Errorf("%s: the echoed token reached the guest: %s", tc.url, res.Body)
		}
	}
}

func TestURLCredentialRefusals(t *testing.T) {
	srv, host, seen := credentialServer(t)
	secrets := newSecrets(t, nil)
	if err := secrets.Put("bot", "123456:ABCDEF"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Put("bound", "654321:ZYXWVU"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Bind("bound", []string{"api.telegram.org"}); err != nil {
		t.Fatal(err)
	}
	svc := credentialService(t, secrets)
	grants := grantsFor(host, "bot", "bound")
	inURL := func(name string) *hostabi.HTTPCredential { return &hostabi.HTTPCredential{Secret: name, In: "url"} }
	port := strings.TrimPrefix(srv.URL, "http://"+host)

	for _, tc := range []struct {
		name    string
		req     hostabi.HTTPRequest
		code    capability.DenyCode
		message string
	}{
		{"no placeholder", hostabi.HTTPRequest{URL: srv.URL + "/getMe", Credential: inURL("bot")},
			capability.DenyNone, "no {credential}"},
		{"placeholder twice", hostabi.HTTPRequest{URL: srv.URL + "/{credential}/{credential}", Credential: inURL("bot")},
			capability.DenyNone, "2 times"},
		// Refused by URL parsing before forge's own authority check is reached;
		// either way the placeholder never gets near the host.
		{"placeholder as userinfo", hostabi.HTTPRequest{URL: "http://{credential}@" + host + port + "/x", Credential: inURL("bot")},
			capability.DenyNone, "userinfo"},
		{"placeholder in the host", hostabi.HTTPRequest{URL: "http://api{credential}.example.com/x", Credential: inURL("bot")},
			capability.DenyNone, "host"},
		{"placeholder as the port", hostabi.HTTPRequest{URL: "http://" + host + ":{credential}/x", Credential: inURL("bot")},
			capability.DenyNone, "port"},
		{"placeholder in the fragment", hostabi.HTTPRequest{URL: srv.URL + "/x#{credential}", Credential: inURL("bot")},
			capability.DenyNone, "fragment"},
		{"header with a url credential", hostabi.HTTPRequest{URL: srv.URL + "/{credential}",
			Credential: &hostabi.HTTPCredential{Secret: "bot", In: "url", Header: "X-Key"}},
			capability.DenyNone, "no header or scheme"},
		{"placeholder with a header credential", hostabi.HTTPRequest{URL: srv.URL + "/{credential}",
			Credential: &hostabi.HTTPCredential{Secret: "bot"}},
			capability.DenyNone, "set In to url"},
		{"unknown placement", hostabi.HTTPRequest{URL: srv.URL + "/{credential}",
			Credential: &hostabi.HTTPCredential{Secret: "bot", In: "body"}},
			capability.DenyNone, "body"},
		{"bound elsewhere", hostabi.HTTPRequest{URL: srv.URL + "/bot{credential}/getMe", Credential: inURL("bound")},
			capability.DenyOutOfScope, "api.telegram.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*seen = nil
			_, err := svc.Do(context.Background(), "t", grants, tc.req)
			if err == nil {
				t.Fatal("the request was allowed")
			}
			if len(*seen) != 0 {
				t.Error("the server was reached despite the refusal")
			}
			d, isDenial := capability.AsDenial(err)
			if tc.code == capability.DenyNone {
				if isDenial {
					t.Errorf("got a %s denial, want a plain failure: %v", d.Code, err)
				}
			} else if !isDenial || d.Code != tc.code {
				t.Errorf("got %v, want a %s denial", err, tc.code)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not mention %q", err, tc.message)
			}
			if strings.Contains(err.Error(), "123456") || strings.Contains(err.Error(), "654321") {
				t.Fatal("the refusal leaked a secret value")
			}
		})
	}
}

// TestURLCredentialStaysOutOfFailures checks the path that would leak it most
// easily: a failed dial, whose *url.Error quotes the full URL.
func TestURLCredentialStaysOutOfFailures(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	dead := srv.URL
	srv.Close()

	secrets := newSecrets(t, nil)
	if err := secrets.Put("bot", "123456:ABCDEF"); err != nil {
		t.Fatal(err)
	}
	_, err = credentialService(t, secrets).Do(context.Background(), "t", grantsFor(host, "bot"),
		hostabi.HTTPRequest{URL: dead + "/bot{credential}/getMe", Credential: &hostabi.HTTPCredential{Secret: "bot", In: "url"}})
	if err == nil {
		t.Fatal("a request to a closed server succeeded")
	}
	if strings.Contains(err.Error(), "123456") {
		t.Fatalf("the failure quoted the token: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("the failure should still say which URL failed, with the token redacted: %v", err)
	}
}
