package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
)

func fsOpen() []capability.Request {
	return []capability.Request{{
		Kind:   capability.FSRead,
		Scope:  []string{"*"},
		Reason: "read the files you ask it to hash",
	}}
}

// TestScopeNarrowsAnOpenRequest is the case the flag exists for: a tool cannot
// name your directories, so it declares "*" and you say which one you meant.
func TestScopeNarrowsAnOpenRequest(t *testing.T) {
	narrowed, err := parseScopes([]string{"fs.read=/home/you/Downloads"})
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := narrowRequests("hashsum", fsOpen(), narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || len(reqs[0].Scope) != 1 || reqs[0].Scope[0] != "/home/you/Downloads" {
		t.Fatalf("scope = %+v, want the one directory given", reqs)
	}
	// The reason has to survive: it is what a person reads when deciding, and
	// rebuilding the request without it would blank the prompt.
	if reqs[0].Reason == "" {
		t.Error("narrowing dropped the reason the tool gave")
	}
}

// TestScopeIsMadeAbsolute covers the failure this flag was written to end. A
// relative or ~ path is what a person types, and forge mounts only absolute
// ones -- so storing it as typed would produce a grant that reads correctly in
// `forge grant ls` and fails at invoke time.
func TestScopeIsMadeAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	narrowed, err := parseScopes([]string{"fs.read=testdata"})
	if err != nil {
		t.Fatal(err)
	}
	if got := narrowed[capability.FSRead][0]; got != filepath.Join(cwd, "testdata") {
		t.Errorf("relative scope = %q, want it resolved against the working directory", got)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to expand against")
	}
	narrowed, err = parseScopes([]string{"fs.read=~/Downloads"})
	if err != nil {
		t.Fatal(err)
	}
	if got := narrowed[capability.FSRead][0]; got != filepath.Join(home, "Downloads") {
		t.Errorf("~ scope = %q, want it expanded to %s", got, filepath.Join(home, "Downloads"))
	}
}

// TestScopeOnlyNarrows is the safety property. --scope is for saying which
// directory you meant, never for handing a tool something it never asked for.
func TestScopeOnlyNarrows(t *testing.T) {
	t.Run("a kind the tool never declared", func(t *testing.T) {
		narrowed, err := parseScopes([]string{"net.http=evil.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = narrowRequests("hashsum", fsOpen(), narrowed)
		if err == nil {
			t.Fatal("granted net.http to a tool that only asked for fs.read")
		}
		if !strings.Contains(err.Error(), "does not ask for net.http") {
			t.Errorf("error = %q, want it to say the tool never asked", err)
		}
	})

	t.Run("a value outside a concrete declaration", func(t *testing.T) {
		declared := []capability.Request{{
			Kind:  capability.NetHTTP,
			Scope: []string{"api.github.com"},
		}}
		narrowed, err := parseScopes([]string{"net.http=evil.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := narrowRequests("fetch", declared, narrowed); err == nil {
			t.Fatal("replaced a declared host with one the tool never named")
		}
	})

	t.Run("dropping one of several declared scopes is allowed", func(t *testing.T) {
		declared := []capability.Request{{
			Kind:  capability.NetHTTP,
			Scope: []string{"api.github.com", "proxy.golang.org"},
		}}
		narrowed, err := parseScopes([]string{"net.http=api.github.com"})
		if err != nil {
			t.Fatal(err)
		}
		reqs, err := narrowRequests("fetch", declared, narrowed)
		if err != nil {
			t.Fatal(err)
		}
		if len(reqs[0].Scope) != 1 || reqs[0].Scope[0] != "api.github.com" {
			t.Errorf("scope = %v, want just the one kept", reqs[0].Scope)
		}
	})
}

// TestUnnarrowedRequestsAreUntouched: naming one capability must not silently
// change the others a tool asked for.
func TestUnnarrowedRequestsAreUntouched(t *testing.T) {
	declared := []capability.Request{
		{Kind: capability.FSRead, Scope: []string{"*"}},
		{Kind: capability.KV, Scope: []string{"hashsum"}},
	}
	narrowed, err := parseScopes([]string{"fs.read=/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := narrowRequests("hashsum", declared, narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want both kept", len(reqs))
	}
	for _, r := range reqs {
		if r.Kind == capability.KV && (len(r.Scope) != 1 || r.Scope[0] != "hashsum") {
			t.Errorf("kv scope = %v, want it left alone", r.Scope)
		}
	}
}

func TestMalformedScopeFlags(t *testing.T) {
	for _, bad := range []string{"fs.read", "fs.read=", "=/tmp"} {
		if _, err := parseScopes([]string{bad}); err == nil {
			t.Errorf("--scope %q was accepted", bad)
		}
	}
}
