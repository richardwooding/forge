package view

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/labels"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSetGetAndPersist(t *testing.T) {
	s, dir := open(t)
	if err := s.Set("dev", "git || json"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := reopened.Get("dev")
	if err != nil {
		t.Fatal(err)
	}
	sel, err := labels.Parse(v.Selector)
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches([]string{"git"}) || sel.Matches([]string{"sql"}) {
		t.Errorf("selector %q did not survive: %v", v.Selector, sel)
	}
}

// TestSelectorsAreStoredCanonically is what makes a saved view stable. The
// canonical form round-trips, so reloading cannot change what a view means.
func TestSelectorsAreStoredCanonically(t *testing.T) {
	s, _ := open(t)
	if err := s.Set("v", "  git   &&   !slow  "); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get("v")
	if err != nil {
		t.Fatal(err)
	}
	if v.Selector != "git && !slow" {
		t.Errorf("stored %q, want the canonical form", v.Selector)
	}
}

func TestSetRejectsBadNamesAndSelectors(t *testing.T) {
	s, _ := open(t)
	// Names appear in a URL path, because the MCP surface mounts one server per
	// view at /mcp/{view}. A name needing escaping there would be a trap for
	// whoever first writes a view called "my tools".
	for _, name := range []string{"My View", "my view", "a/b", "", "-leading", "UPPER"} {
		if err := s.Set(name, "*"); err == nil {
			t.Errorf("accepted view name %q", name)
		}
	}
	// A malformed selector is rejected where it is typed, not the next time
	// forge starts.
	if err := s.Set("ok", "git &&"); err == nil {
		t.Error("accepted a malformed selector")
	}
}

func TestResolvePrecedence(t *testing.T) {
	s, _ := open(t)
	if err := s.Set("dev", "git"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("all", "*"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("dev"); err != nil {
		t.Fatal(err)
	}

	// An explicit selector beats everything.
	sel, name, err := s.Resolve("dev", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches([]string{"json"}) || name != "" {
		t.Errorf("explicit selector lost: %v %q", sel, name)
	}

	// A named view beats the active one.
	sel, name, err = s.Resolve("all", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches([]string{"anything"}) || name != "all" {
		t.Errorf("named view lost: %v %q", sel, name)
	}

	// The active view applies when nothing is named.
	sel, name, err = s.Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches([]string{"git"}) || sel.Matches([]string{"json"}) || name != "dev" {
		t.Errorf("active view not applied: %v %q", sel, name)
	}
}

// TestResolveRefusesAnUnknownView is a security-shaped choice, not a
// convenience one. On MCP the view is the only boundary, so a typo in --view
// must not quietly fall back to exposing everything.
func TestResolveRefusesAnUnknownView(t *testing.T) {
	s, _ := open(t)
	_, _, err := s.Resolve("nosuchview", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound rather than a silent fall back to everything", err)
	}
}

func TestResolveWithNothingConfiguredMeansEverything(t *testing.T) {
	s, _ := open(t)
	sel, name, err := s.Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(nil) || name != "" {
		t.Errorf("a fresh install should see all its tools: %v %q", sel, name)
	}
}

func TestDeletingTheActiveViewClearsIt(t *testing.T) {
	// A dangling active view would make every later command fail with "view
	// not found" until the user worked out why.
	s, _ := open(t)
	if err := s.Set("dev", "git"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("dev"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("dev"); err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveName(); got != "" {
		t.Errorf("active view = %q after deleting it", got)
	}
	if _, _, err := s.Resolve("", ""); err != nil {
		t.Errorf("resolving after deleting the active view failed: %v", err)
	}
}

func TestSetActiveRejectsAnUnknownView(t *testing.T) {
	s, _ := open(t)
	if err := s.SetActive("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListIsOrdered(t *testing.T) {
	s, _ := open(t)
	for _, n := range []string{"zed", "alpha", "mid"} {
		if err := s.Set(n, "*"); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for _, v := range s.List() {
		names = append(names, v.Name)
	}
	if strings.Join(names, ",") != "alpha,mid,zed" {
		t.Errorf("names = %v, want them ordered", names)
	}
}

func TestOpenReportsACorruptFile(t *testing.T) {
	// Unlike a tool record, there is only one views file: silently starting
	// empty would look like every saved view had vanished.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "views.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Error("a corrupt views file was accepted")
	}
}
