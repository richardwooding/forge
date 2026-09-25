package labels

import (
	"strings"
	"testing"
)

func TestMatches(t *testing.T) {
	tools := map[string][]string{
		"gitstat": {"git", "vcs"},
		"jsonfmt": {"json", "text"},
		"slowsql": {"sql", "slow"},
		"gitslow": {"git", "slow"},
		"bare":    nil,
	}

	tests := []struct {
		expr string
		want []string // tools that should match
	}{
		{"*", []string{"gitstat", "jsonfmt", "slowsql", "gitslow", "bare"}},
		{"", []string{"gitstat", "jsonfmt", "slowsql", "gitslow", "bare"}},
		{"git", []string{"gitstat", "gitslow"}},
		{"git && !slow", []string{"gitstat"}},
		{"git || json", []string{"gitstat", "jsonfmt", "gitslow"}},
		{"(git || json) && !slow", []string{"gitstat", "jsonfmt"}},
		{"!slow", []string{"gitstat", "jsonfmt", "bare"}},
		{"!*", nil},
		{"nosuchlabel", nil},
		{"git && json", nil},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			sel, err := Parse(tt.expr)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			var got []string
			for _, name := range []string{"gitstat", "jsonfmt", "slowsql", "gitslow", "bare"} {
				if sel.Matches(tools[name]) {
					got = append(got, name)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("matched %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCanonicalFormRoundTrips is the property that keeps a saved view meaning
// what it meant. forge stores selectors by their String form, so if parsing
// that back produced a different expression, a view would quietly change the
// next time it was loaded.
func TestCanonicalFormRoundTrips(t *testing.T) {
	exprs := []string{
		"git",
		"!git",
		"git && json",
		"git || json",
		"git && !json",
		"(git || vcs) && !slow",
		"a && b && c",
		"a || b || c",
		"a && (b || c)",
		"(a || b) && (c || d)",
		"!(a && b)",
		"!(a || b)",
		"*",
		"!*",
	}
	sets := [][]string{
		nil, {"a"}, {"b"}, {"c"}, {"d"}, {"a", "b"}, {"a", "c"}, {"b", "c"},
		{"a", "b", "c", "d"}, {"git"}, {"json"}, {"git", "json"}, {"vcs"},
		{"git", "slow"}, {"vcs", "slow"},
	}

	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			first, err := Parse(expr)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			second, err := Parse(first.String())
			if err != nil {
				t.Fatalf("Parse(%q): %v", first.String(), err)
			}
			if second.String() != first.String() {
				t.Errorf("canonical form is not stable: %q then %q", first, second)
			}
			for _, set := range sets {
				if a, b := first.Matches(set), second.Matches(set); a != b {
					t.Errorf("meaning changed for %v: %q says %v, %q says %v", set, first, a, second, b)
				}
			}
		})
	}
}

func TestPrecedence(t *testing.T) {
	// && binds tighter than ||, as in every language a user is likely to know.
	sel, err := Parse("a || b && c")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches([]string{"a"}) {
		t.Error("a should match a || (b && c)")
	}
	if sel.Matches([]string{"b"}) {
		t.Error("b alone should not match a || (b && c)")
	}
	if !sel.Matches([]string{"b", "c"}) {
		t.Error("b and c should match a || (b && c)")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, expr := range []string{
		"(", ")", "(a", "a)", "&&", "a &&", "|| a", "!", "a b",
		"a &", "a |", "A", "a!!", "()",
	} {
		t.Run(expr, func(t *testing.T) {
			if sel, err := Parse(expr); err == nil {
				t.Errorf("accepted %q as %v", expr, sel)
			}
		})
	}
}

func TestParseErrorsNameTheSelector(t *testing.T) {
	// A selector is typed at a prompt, so the error has to show what was typed
	// and where it went wrong.
	_, err := Parse("git &&")
	if err == nil {
		t.Fatal("accepted a trailing &&")
	}
	if !strings.Contains(err.Error(), "git &&") {
		t.Errorf("error %q does not quote the selector", err)
	}
}

func TestNotStarIsNone(t *testing.T) {
	sel, err := Parse("!*")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Matches([]string{"anything"}) || sel.Matches(nil) {
		t.Error("!* matched something")
	}
}
