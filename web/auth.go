package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
)

// cookieName is the session cookie. Prefixed so it cannot be confused with
// anything else a browser has stored for localhost, which is a crowded origin
// on a developer's machine.
const cookieName = "forge_gui_session"

// tokenParam carries the single-use token in the printed URL.
const tokenParam = "t"

// Session is the GUI's authentication.
//
// A browser cannot reach a unix socket, so turning the GUI on means opening a
// TCP listener, and a TCP listener on a developer's machine is reachable by
// every other process on it and by any web page that can guess the port. The
// handshake below is what stands between "forge is running" and "anything
// local can run your tools".
//
// The shape is: forge prints one URL carrying a single-use token, opening it
// exchanges that token for a session cookie, and the token is spent. Nothing
// reusable is ever written to disk or left in shell history, and the token
// leaves the address bar on the redirect that follows.
type Session struct {
	mu      sync.Mutex
	token   string // the single-use handshake token; emptied once spent
	session string // the cookie value, good for the life of the process
}

// NewSession mints a handshake token and the session behind it.
func NewSession() (*Session, error) {
	token, err := secret()
	if err != nil {
		return nil, err
	}
	sess, err := secret()
	if err != nil {
		return nil, err
	}
	return &Session{token: token, session: sess}, nil
}

// secret returns 32 bytes of randomness, URL-safe.
//
// crypto/rand only: a predictable token here is the whole authentication, and
// math/rand seeded from the clock is guessable by anything that knows roughly
// when forge started.
func secret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Token is the handshake token, for building the URL to print. It is empty
// once the token has been spent.
func (s *Session) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// redeem exchanges a handshake token for the session, once.
//
// The comparison is constant-time. A byte-by-byte compare that returns early
// leaks the token one character at a time to anything that can measure it, and
// on loopback an attacker can measure very accurately.
func (s *Session) redeem(offered string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token == "" || offered == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(offered), []byte(s.token)) != 1 {
		return "", false
	}
	// Spent. A token that worked twice would survive in browser history, in a
	// terminal scrollback, and in whatever read the URL the first time.
	s.token = ""
	return s.session, true
}

// valid reports whether a request carries the session cookie.
func (s *Session) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) == 1
}

// setCookie installs the session cookie.
func (s *Session) setCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:  cookieName,
		Value: value,
		Path:  "/",

		// HttpOnly: the page never needs to read this, and not exposing it to
		// script means an injected script cannot lift it.
		HttpOnly: true,

		// SameSite=Strict, but do not mistake it for the thing holding this
		// door shut.
		//
		// SameSite is computed on *site*, which for an IP literal is the IP
		// with no port: http://127.0.0.1:1234 and http://127.0.0.1:53871 are
		// the SAME SITE. So Strict does nothing against the one cross-origin
		// attacker that actually exists on a developer's machine -- another
		// service listening on another local port, or a page it serves. The
		// exact-origin check in handler.go is what stops that, and it is why
		// no separate CSRF token is needed. Keep Strict anyway: it costs
		// nothing and it does keep the cookie off cross-site navigations.
		//
		// For the same reason, cookies are not port-scoped either: any local
		// server reachable at a /ui/ path will be sent this cookie by the
		// browser. The residual attacker there is a process already running
		// as this user, which can reach the unix socket regardless.
		SameSite: http.SameSiteStrictMode,

		// Deliberately NOT Secure. The listener is plain http on loopback, and
		// a Secure cookie would never be sent over it -- the GUI would appear
		// to authenticate and then fail every request.
		Secure: false,
	})
}
