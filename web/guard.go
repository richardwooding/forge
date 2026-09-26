package web

import (
	"fmt"
	"net/http"
	"strings"
)

// safeMethod reports whether a method only reads.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// originGuard enforces same-origin on a listener that knows its own address.
//
// This, not the cookie's SameSite attribute, is what stops another process on
// this machine from driving your tools. SameSite is computed on site, and for
// a loopback IP every port is the same site -- so a page served by some other
// local service is same-site with the GUI and Strict does not apply. Its
// Origin header, however, names its port, and that is an exact mismatch here.
//
// It differs deliberately from the guard on the API listener
// (internal/surface/cli/serve.go), which allows a request with no Origin
// because curl must keep working. On this listener the only legitimate client
// is a browser, so a write with no Origin is refused: that is the shape a
// non-browser client forging a request would take, and nothing legitimate
// produces it here.
func originGuard(self string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		switch {
		case origin == self:
			// Same origin, exactly. Port included.
		case origin == "" && safeMethod(r.Method):
			// A top-level navigation, or a same-origin GET -- the spec omits
			// Origin on those. The Host check has already run, which is what
			// makes this safe to allow.
		default:
			http.Error(w, fmt.Sprintf("this page is served from %s; %q may not drive it",
				self, originOrNone(origin)), http.StatusForbidden)
			return
		}

		// Sec-Fetch-Site, where the browser sends it, is a second opinion that
		// costs nothing. Absence means "no opinion", so non-browser clients
		// and tests are unaffected.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && !safeMethod(r.Method) {
			if site != "same-origin" && site != "none" {
				http.Error(w, "this request did not come from the GUI", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func originOrNone(o string) string {
	if o == "" {
		return "a request with no origin"
	}
	return o
}

// securityHeaders hardens every GUI response.
//
// frame-ancestors closes the clickjacking half of a rebinding attack, and
// nosniff stops a response being re-interpreted as script. The policy is
// deliberately tight: the page loads nothing it did not ship with.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		if strings.HasPrefix(r.URL.Path, Prefix+"api/") {
			// Nothing under the API prefix should ever execute if it is
			// opened directly in a tab.
			h.Set("Content-Security-Policy", "sandbox")
		} else {
			h.Set("Content-Security-Policy",
				"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; font-src 'self'; connect-src 'self'; "+
					"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// hostGuard rejects a request whose Host is not one this listener answers to.
//
// This is the defence against DNS rebinding, and the Origin check is not a
// substitute for it. After a rebind the attacker's page is same-origin with
// itself, so its GETs carry no Origin at all and an Origin allowlist waves
// them through. What the attacker cannot forge is the Host: it names the
// domain the browser was told to fetch, which is theirs.
func hostGuard(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, allowed) {
			http.Error(w, fmt.Sprintf("this forge answers to %s, not %q",
				strings.Join(allowed, " or "), r.Host), http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
