package labels

import "testing"

// FuzzParse checks that the selector parser cannot be made to panic, and that
// anything it accepts has a canonical form it also accepts with the same
// meaning.
//
// Selectors arrive from a config file, a command line and -- on the MCP surface
// -- potentially from a model, so a panic here would take down whichever server
// was holding it. The round-trip half matters because forge persists the
// canonical form: if String produced something Parse read differently, a saved
// view would change meaning on reload.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"", "*", "!*", "git", "git && json", "git || json", "!git",
		"(a || b) && !c", "a && b && c", "((((a))))", "a&&b", "a  ||  b",
		"!!a", "!(a)", "(", ")", "&&", "a b", "\x00", "日本語", "a-b_c1",
	} {
		f.Add(seed)
	}

	sets := [][]string{nil, {"a"}, {"b"}, {"a", "b"}, {"git"}, {"git", "json"}}

	f.Fuzz(func(t *testing.T, expr string) {
		sel, err := Parse(expr)
		if err != nil {
			return // rejecting input is always a valid outcome
		}
		if sel == nil {
			t.Fatalf("Parse(%q) returned nil with no error", expr)
		}

		canon := sel.String()
		again, err := Parse(canon)
		if err != nil {
			t.Fatalf("Parse(%q) produced %q, which Parse rejects: %v", expr, canon, err)
		}
		if again.String() != canon {
			t.Errorf("canonical form is not stable: %q -> %q -> %q", expr, canon, again.String())
		}
		for _, set := range sets {
			if a, b := sel.Matches(set), again.Matches(set); a != b {
				t.Errorf("meaning changed through the canonical form for %v: %q=%v, %q=%v", set, expr, a, canon, b)
			}
		}
	})
}
