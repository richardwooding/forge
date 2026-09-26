package web

import (
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// Prefix is where the GUI is mounted.
//
// Not "/": the root is the server-rendered, script-free index, and a test
// asserts it stays that way. The GUI is an addition, not a replacement.
const Prefix = "/ui/"

// apiPrefix is where the REST surface is re-exposed to the page.
const apiPrefix = Prefix + "api"

// Options configure the GUI.
type Options struct {
	// Session authenticates the browser. Required.
	Session *Session

	// API is the REST handler the page talks to, mounted under /ui/api.
	// Reusing it rather than reimplementing is what keeps the GUI a client of
	// an existing surface instead of becoming a sixth one.
	API http.Handler

	// Authority is the host:port this listener bound, exactly as printed.
	Authority string
}

// Handler serves the GUI.
//
// Returns ErrNotBuilt when the front end is absent, so the caller can say so
// at startup rather than spending the single-use token on a blank page.
func Handler(opts Options) (http.Handler, error) {
	assets, err := Assets()
	if err != nil {
		return nil, err
	}
	return handlerWith(opts, assets)
}

// handlerWith is Handler over a given asset tree, so a test can supply one
// without a build having run.
func handlerWith(opts Options, assets fs.FS) (http.Handler, error) {
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, ErrNotBuilt
	}

	mux := http.NewServeMux()

	// The one route that does not need a session: it is how you get one.
	mux.HandleFunc("GET "+Prefix+"session", func(w http.ResponseWriter, r *http.Request) {
		handshake(w, r, opts.Session)
	})

	// The REST surface, behind the session, with its responses neutralised.
	mux.Handle(apiPrefix+"/", opts.Session.require(
		neutralise(http.StripPrefix(apiPrefix, opts.API))))

	// The page itself.
	mux.Handle(Prefix, opts.Session.require(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, assets)
		})))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix, http.StatusSeeOther)
	})

	self := "http://" + opts.Authority
	return securityHeaders(
		hostGuard([]string{opts.Authority}, originGuard(self, mux))), nil
}

// handshake exchanges the single-use token for the session cookie.
func handshake(w http.ResponseWriter, r *http.Request, s *Session) {
	// Already authenticated: make a refresh or a back-button idempotent
	// rather than a baffling refusal.
	if s.valid(r) {
		http.Redirect(w, r, Prefix, http.StatusSeeOther)
		return
	}

	token := r.URL.Query().Get(tokenParam)
	if token == "" {
		http.Error(w, "open the link `forge serve --gui` printed in your terminal.",
			http.StatusBadRequest)
		return
	}

	value, ok := s.redeem(token)
	if !ok {
		// One message for spent and one for wrong would be friendlier, but the
		// distinction is not worth an oracle; against 256 bits of randomness
		// neither is guessable, and the user action is the same either way.
		http.Error(w, "that link has been used already, or is not from this forge. "+
			"Restart `forge serve --gui` for a fresh one.", http.StatusForbidden)
		return
	}

	s.setCookie(w, value)
	// See-other to the bare path, so the token leaves the address bar and the
	// history entry it would otherwise sit in.
	http.Redirect(w, r, Prefix, http.StatusSeeOther)
}

// require rejects anything without a valid session.
func (s *Session) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.valid(r) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "this GUI session has expired or was never started. "+
				"Restart `forge serve --gui` for a new link.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// neutralise stops a tool's output being rendered as a document by the
// browser.
//
// The REST surface serves OutputBytes with the tool's OWN media type, which is
// right for an API client and dangerous here: a tool returning text/html would
// have that HTML served from the GUI's own origin, which is same-origin script
// injection from code the whole project exists to sandbox. HttpOnly keeps it
// away from the cookie, but not from using it.
//
// Rather than reimplement invoke to avoid the problem, force anything that is
// not plainly data to download instead of render. The page fetches these with
// fetch() and decides how to display them itself, so nothing legitimate
// changes.
func neutralise(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&neutralised{ResponseWriter: w}, r)
	})
}

type neutralised struct {
	http.ResponseWriter
	done bool
}

func (n *neutralised) WriteHeader(code int) {
	if !n.done {
		n.done = true
		ct := n.Header().Get("Content-Type")
		if !inertType(ct) {
			n.Header().Set("Content-Type", "application/octet-stream")
			n.Header().Set("Content-Disposition", "attachment")
		}
	}
	n.ResponseWriter.WriteHeader(code)
}

func (n *neutralised) Write(b []byte) (int, error) {
	if !n.done {
		n.WriteHeader(http.StatusOK)
	}
	return n.ResponseWriter.Write(b)
}

// inertType reports whether a content type is safe to leave alone: data the
// browser will not execute or render as a document.
func inertType(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	switch strings.TrimSpace(strings.ToLower(base)) {
	case "application/json", "application/problem+json", "text/plain", "":
		return true
	}
	return false
}

// serveAsset serves a built file, falling back to index.html for a path the
// client router owns.
func serveAsset(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	name := strings.TrimPrefix(r.URL.Path, Prefix)
	if name == "" || !strings.Contains(lastSegment(name), ".") {
		serveIndex(w, assets)
		return
	}
	http.StripPrefix(Prefix, http.FileServer(http.FS(assets))).ServeHTTP(w, r)
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func serveIndex(w http.ResponseWriter, assets fs.FS) {
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "the GUI is not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The tool list comes from whatever is installed right now, so a cached
	// page would be wrong the moment one is added.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// hostAllowed compares a request's Host against what the listener bound.
func hostAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(host, a) {
			return true
		}
	}
	return false
}

// URL is the address to print, carrying the single-use token.
func URL(authority, token string) string {
	return fmt.Sprintf("http://%s%ssession?%s=%s", authority, Prefix, tokenParam, token)
}
