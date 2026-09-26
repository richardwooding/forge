package repl

import (
	"strings"
	"testing"
)

func TestSplitLine(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"hello", []string{"hello"}},
		{"greet --name world", []string{"greet", "--name", "world"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{`greet --name "two words"`, []string{"greet", "--name", "two words"}},
		{`greet --name 'two words'`, []string{"greet", "--name", "two words"}},
		{`greet --name a\ b`, []string{"greet", "--name", "a b"}},
		// A single-quoted string is literal, as in a shell.
		{`echo 'a\nb'`, []string{"echo", `a\nb`}},
		{`echo "a\"b"`, []string{"echo", `a"b`}},
		// An empty argument is a real argument.
		{`greet --name ""`, []string{"greet", "--name", ""}},
		// Commas must survive: a repeatable flag's value can contain one, and
		// splitting on it is the trap pflag's StringSlice falls into.
		{`tags --tag a,b --tag c`, []string{"tags", "--tag", "a,b", "--tag", "c"}},
		{`json '{"a":1}'`, []string{"json", `{"a":1}`}},
	}
	for _, tt := range tests {
		got, err := splitLine(tt.in)
		if err != nil {
			t.Errorf("splitLine(%q): %v", tt.in, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
			t.Errorf("splitLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSplitLineRejectsUnclosedQuotes(t *testing.T) {
	for _, in := range []string{`greet --name "unclosed`, `greet --name 'unclosed`} {
		if _, err := splitLine(in); err == nil {
			t.Errorf("splitLine(%q) accepted an unclosed quote", in)
		}
	}
}

// TestQuoteRoundTrips is the property the REPL depends on when it shows a
// command back: what the user is shown must be something they could have
// typed.
func TestQuoteRoundTrips(t *testing.T) {
	args := []string{
		"plain", "two words", "", `has"quote`, `has'apostrophe`,
		`back\slash`, "a,b", `{"json":true}`, "  leading", "trailing  ",
		"both'kinds\"here", "tab\there",
	}
	var line strings.Builder
	for i, a := range args {
		if i > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(quote(a))
	}

	got, err := splitLine(line.String())
	if err != nil {
		t.Fatalf("splitLine(%q): %v", line.String(), err)
	}
	if len(got) != len(args) {
		t.Fatalf("round trip produced %d args, want %d: %q", len(got), len(args), got)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("arg %d: got %q, want %q (quoted as %s)", i, got[i], args[i], quote(args[i]))
		}
	}
}
