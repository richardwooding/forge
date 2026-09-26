package web

// The GUI is the only part of forge a browser can reach, and turning it on
// means opening a TCP port on a machine where anything local can knock. These
// tests are about who is allowed through that door; the page itself is judged
// by looking at it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const authority = "127.0.0.1:7777"

// testHandler builds a GUI over a fake asset tree and a fake API.
func testHandler(t *testing.T) (http.Handler, *Session) {
	t.Helper()
	s, err := NewSession()
	if err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[]}`))
	})
	h, err := handlerWith(Options{Session: s, API: api, Authority: authority},
		fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>forge</title>")}})
	if err != nil {
		t.Fatal(err)
	}
	return h, s
}

// do issues a request with the right Host, since every route is behind the
// host guard.
func do(t *testing.T, h http.Handler, method, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = authority
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// withSession returns a modifier that attaches a valid cookie.
func withSession(s *Session) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: s.session})
	}
}

func TestTheTokenWorksOnceAndOnlyOnce(t *testing.T) {
	h, s := testHandler(t)
	token := s.Token()
	if token == "" {
		t.Fatal("no token was minted")
	}

	first := do(t, h, http.MethodGet, Prefix+"session?t="+token, nil)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d, want 303", first.Code)
	}
	if !strings.Contains(first.Header().Get("Set-Cookie"), cookieName) {
		t.Error("the handshake set no session cookie")
	}
	// The redirect must drop the token, or it survives in history.
	if loc := first.Header().Get("Location"); strings.Contains(loc, "t=") {
		t.Errorf("the redirect carries the token: %s", loc)
	}

	second := do(t, h, http.MethodGet, Prefix+"session?t="+token, nil)
	if second.Code != http.StatusForbidden {
		t.Errorf("second use: %d, want 403 -- a reusable token survives in history and scrollback", second.Code)
	}
	if s.Token() != "" {
		t.Error("the token is still live after being spent")
	}
}

func TestAWrongTokenIsRefused(t *testing.T) {
	h, _ := testHandler(t)
	w := do(t, h, http.MethodGet, Prefix+"session?t=not-the-token", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", w.Code)
	}
}

func TestNoSessionNoAccess(t *testing.T) {
	h, _ := testHandler(t)
	for _, target := range []string{Prefix, Prefix + "index.html", apiPrefix + "/v1/tools"} {
		w := do(t, h, http.MethodGet, target, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d, want 401", target, w.Code)
		}
	}
}

func TestASessionGetsThrough(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodGet, Prefix, withSession(s))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "forge") {
		t.Errorf("the page was not served: %q", w.Body.String())
	}
}

// TestForeignHostIsRefused is the DNS-rebinding defence. After a rebind the
// attacker's page is same-origin with itself, so its GETs carry no Origin at
// all and an Origin check waves them through. The Host still names their
// domain, which is what this catches.
func TestForeignHostIsRefused(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodGet, apiPrefix+"/v1/tools", func(r *http.Request) {
		r.Host = "evil.example:7777"
		r.AddCookie(&http.Cookie{Name: cookieName, Value: s.session})
	})
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("a rebound host got %d, want 421", w.Code)
	}
}

// TestAnotherLocalPortIsRefused is the case SameSite does NOT cover: a page
// served by a different local service is the same *site* as this one, because
// site ignores the port. Only the exact origin check separates them.
func TestAnotherLocalPortIsRefused(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodPost, apiPrefix+"/v1/tools/x/invoke", func(r *http.Request) {
		r.Header.Set("Origin", "http://127.0.0.1:9999")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: s.session})
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("a page on another local port got %d, want 403", w.Code)
	}
}

func TestSameOriginPostIsAllowed(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodPost, apiPrefix+"/v1/tools/x/invoke", func(r *http.Request) {
		r.Header.Set("Origin", "http://"+authority)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: s.session})
	})
	if w.Code == http.StatusForbidden {
		t.Error("the GUI's own origin was refused; every form submission would fail")
	}
}

// TestAWriteWithNoOriginIsRefused: every browser attaches Origin to a POST, so
// one without it did not come from a page. On this listener, where a browser
// is the only legitimate client, that is worth refusing -- unlike the API
// listener, where curl must keep working.
func TestAWriteWithNoOriginIsRefused(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodPost, apiPrefix+"/v1/tools/x/invoke", withSession(s))
	if w.Code != http.StatusForbidden {
		t.Errorf("a POST with no Origin got %d, want 403", w.Code)
	}
}

// TestToolOutputCannotRenderAsADocument is the injection this closes. REST
// serves bytes with the tool's own media type, which is right for an API
// client; served from the GUI's origin it would be same-origin HTML written by
// sandboxed code.
func TestToolOutputCannotRenderAsADocument(t *testing.T) {
	s, err := NewSession()
	if err != nil {
		t.Fatal(err)
	}
	evil := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<script>alert(1)</script>`))
	})
	h, err := handlerWith(Options{Session: s, API: evil, Authority: authority},
		fstest.MapFS{"index.html": {Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, h, http.MethodGet, apiPrefix+"/v1/tools/evil", withSession(s))
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("a tool served text/html from the GUI origin: %q", ct)
	}
	if w.Header().Get("Content-Disposition") == "" {
		t.Error("a non-inert response was not forced to download")
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != "sandbox" {
		t.Errorf("API responses should be sandboxed, got %q", csp)
	}
}

func TestJSONIsLeftAlone(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodGet, apiPrefix+"/v1/tools", withSession(s))
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("JSON was mangled into %q", ct)
	}
	if w.Body.String() != `{"tools":[]}` {
		t.Errorf("body was altered: %s", w.Body.String())
	}
}

func TestThePageIsFramedByNobody(t *testing.T) {
	h, s := testHandler(t)
	w := do(t, h, http.MethodGet, Prefix, withSession(s))
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'none'", "default-src 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy is missing %q: %s", want, csp)
		}
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("responses may be sniffed")
	}
}

func TestNotBuiltIsReported(t *testing.T) {
	s, err := NewSession()
	if err != nil {
		t.Fatal(err)
	}
	// An asset tree with no index.html is what a checkout that has never been
	// built looks like.
	_, err = handlerWith(Options{Session: s, Authority: authority}, fstest.MapFS{".gitkeep": {}})
	if err == nil {
		t.Fatal("an unbuilt tree produced a handler; --gui would spend its token on a blank page")
	}
}
