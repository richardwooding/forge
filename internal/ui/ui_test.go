package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// TestTableAlignsWithStyledCells is the reason this package has a table at all.
// text/tabwriter measures cells in bytes, so every escape sequence counts
// towards the column width and a coloured table comes out visibly crooked.
func TestTableAlignsWithStyledCells(t *testing.T) {
	th := New(true, true)

	tbl := th.NewTable("NAME", "LABELS", "SUMMARY")
	tbl.Row(th.Name.Render("alpha"), th.Chips([]string{"git", "vcs"}), "first")
	tbl.Row(th.Name.Render("b"), th.Chips([]string{"json"}), "second")
	tbl.Row("plain", "", "third")

	var buf bytes.Buffer
	tbl.Render(&buf)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}

	// Strip the styling and check the last column starts in the same place on
	// every line. That is the property colour must not disturb.
	var col int
	for i, line := range lines {
		plain := ansi.ReplaceAllString(line, "")
		want := []string{"SUMMARY", "first", "second", "third"}[i]
		idx := strings.LastIndex(plain, want)
		if idx < 0 {
			t.Fatalf("line %d does not contain %q: %q", i, want, plain)
		}
		if i == 0 {
			col = idx
			continue
		}
		if idx != col {
			t.Errorf("line %d starts its last column at %d, want %d:\n%q", i, idx, col, plain)
		}
	}
}

func TestTableLeavesNoTrailingSpaces(t *testing.T) {
	// Invisible trailing spaces show up the moment someone diffs or greps the
	// output.
	th := New(false, true)
	tbl := th.NewTable("A", "B")
	tbl.Row("x", "y")
	tbl.Row("longer", "")

	var buf bytes.Buffer
	tbl.Render(&buf)
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line has trailing spaces: %q", line)
		}
	}
}

func TestDisabledThemeProducesPlainText(t *testing.T) {
	// The same code path has to produce plain output, or there would be a
	// second set of format strings to keep in step with the first.
	th := New(false, true)
	for name, got := range map[string]string{
		"Name":  th.Name.Render("x"),
		"Chip":  th.Chip("git"),
		"OK":    th.OK("fine"),
		"Fail":  th.Fail("bad"),
		"Chips": th.Chips([]string{"a", "b"}),
	} {
		if ansi.MatchString(got) {
			t.Errorf("%s produced escape sequences with the theme disabled: %q", name, got)
		}
	}
}

// TestStatusWordsSurviveColourStripping is the rule that matters for output
// that gets piped: meaning must never depend on colour alone.
func TestStatusWordsSurviveColourStripping(t *testing.T) {
	th := New(true, true)
	for _, got := range []string{th.OK("all present"), th.Fail("missing"), th.Warn("careful")} {
		plain := ansi.ReplaceAllString(got, "")
		if strings.TrimSpace(plain) == "" {
			t.Errorf("nothing survives stripping: %q", got)
		}
		if !strings.ContainsAny(plain, "✓✗!") {
			t.Errorf("no glyph survives stripping: %q", plain)
		}
	}
}

func TestChipColourIsStableForALabel(t *testing.T) {
	// A label's colour comes from a hash of its name, so "git" looks the same
	// in every listing and on every machine with nothing to configure.
	th := New(true, true)
	if th.Chip("git") != th.Chip("git") {
		t.Error("the same label rendered two different colours")
	}
	if th.Chip("git") == th.Chip("json") {
		t.Error("two different labels rendered identically; the hash is not spreading them")
	}
}

func TestEmptyLabelSetRendersAPlaceholder(t *testing.T) {
	th := New(false, true)
	if got := th.Chips(nil); got != "—" {
		t.Errorf("Chips(nil) = %q, want a placeholder so the column is not blank", got)
	}
}
