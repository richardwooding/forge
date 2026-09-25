package mcpsrv_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/view"
)

// TestOriginGuard covers the attack that actually reaches a local server.
//
// "It only listens on localhost" is not a defence: a page the user is already
// viewing can POST to 127.0.0.1, and DNS rebinding makes that same-origin from
// the browser's point of view. A request carrying no Origin is not from a
// browser, which is what lets an ordinary MCP client work.
func TestOriginGuard(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	h := mgr.Handler(mcpsrv.HTTPOptions{
		Default:        labels.All,
		AllowedOrigins: []string{"https://trusted.example"},
	})

	tests := []struct {
		name, origin string
		wantBlocked  bool
	}{
		{"no origin (an ordinary client)", "", false},
		{"allowed origin", "https://trusted.example", false},
		{"a hostile page", "https://evil.example", true},
		{"a rebound localhost origin", "http://127.0.0.1:3000", true},
		{"a near miss", "https://trusted.example.evil.test", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			blocked := rec.Code == http.StatusForbidden
			if blocked != tt.wantBlocked {
				t.Errorf("origin %q: status %d, blocked=%v want %v", tt.origin, rec.Code, blocked, tt.wantBlocked)
			}
		})
	}
}

// TestAnUnknownViewServesNothing is the safe failure. On this surface the view
// is the only boundary, so falling back to everything would turn a typo in a
// client config into full exposure.
func TestAnUnknownViewServesNothing(t *testing.T) {
	tk := fixture(t)
	views, err := view.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := views.Set("demo", "demo"); err != nil {
		t.Fatal(err)
	}

	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})
	// Resolve the same way the handler does.
	sel, _, err := views.Resolve("nosuchview", "")
	if err == nil {
		t.Fatal("an unknown view resolved without error")
	}
	_ = sel

	srv, err := mgr.Server("nosuchview", labels.None)
	if err != nil {
		t.Fatal(err)
	}
	if got := listNames(t, connect(t, srv)); len(got) != 0 {
		t.Errorf("an unknown view exposed %v, want nothing", got)
	}
}
