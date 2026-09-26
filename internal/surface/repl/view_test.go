package repl

// Tests for what the shell actually draws.
//
// The package had thorough tests for its behaviour -- history, completion,
// quoting, colon routing, ctrl-c -- and not one that called View(). So three
// rendering defects shipped together and made a working shell feel broken:
// the text input kept its own default "> " prompt on top of forge's, its
// placeholder collapsed to the single letter "a" because it was never given a
// width, and no cursor was drawn at all. Every behaviour was tested; nothing
// was rendered.

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/ui"
)

// ansiPattern strips styling so an assertion is about what a person sees
// rather than about which escape sequences lipgloss happened to emit. The
// placeholder in particular is rendered with its first character styled
// separately, since that cell is where the cursor would sit.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func plain(s string) string { return ansiPattern.ReplaceAllString(s, "") }

func viewModel(t *testing.T) *model {
	t.Helper()
	m := newTestModel(&fakeDispatcher{})
	// The size the terminal would report on startup.
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m
}

// TestViewDrawsACursor is the direct assertion for the bug. newModel turns off
// the input's virtual cursor, which obliges View to supply the real one; doing
// only the first half leaves the shell with no caret anywhere, so typing looks
// like characters landing at random.
func TestViewDrawsACursor(t *testing.T) {
	m := viewModel(t)
	if v := m.View(); v.Cursor == nil {
		t.Fatal("View draws no cursor; the input's virtual cursor is disabled, so nothing renders one")
	}
}

// TestCursorSitsAfterTheTypedText pins the offset rather than merely its
// presence. A cursor at the wrong column is as unusable as none.
func TestCursorSitsAfterTheTypedText(t *testing.T) {
	m := viewModel(t)
	base := lipgloss.Width(m.prompt())

	for _, n := range []int{0, 1, 5} {
		m.input.SetValue(strings.Repeat("x", n))
		m.input.CursorEnd()

		v := m.View()
		if v.Cursor == nil {
			t.Fatalf("no cursor with %d characters typed", n)
		}
		if got, want := v.Cursor.X, base+n; got != want {
			t.Errorf("with %d characters the cursor is at column %d, want %d", n, got, want)
		}
	}
}

// TestCursorFollowsALongerPrompt: a view name widens the prompt, and an offset
// computed once or hard-coded would leave the caret behind the text.
func TestCursorFollowsALongerPrompt(t *testing.T) {
	plain := viewModel(t)
	plainX := plain.View().Cursor.X

	named := newTestModel(&fakeDispatcher{})
	named.opts.ViewName = "dev"
	named.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	gotX := named.View().Cursor.X
	if wantX := lipgloss.Width(named.prompt()); gotX != wantX {
		t.Errorf("with a view name the cursor is at %d, want %d", gotX, wantX)
	}
	if gotX <= plainX {
		t.Errorf("a longer prompt did not move the cursor: %d then %d", plainX, gotX)
	}
}

// TestPromptIsNotDoubled: textinput.New defaults its own Prompt to "> ", and
// the shell draws a prompt of its own. Leaving both on rendered `forge› > `,
// which is what made typed text ambiguous with the prompt.
func TestPromptIsNotDoubled(t *testing.T) {
	m := viewModel(t)
	content := plain(m.View().Content)

	if strings.Contains(content, "> ") {
		t.Errorf("the input's default prompt is still set; the line renders as %q", content)
	}
	if n := strings.Count(content, "›"); n != 1 {
		t.Errorf("expected exactly one prompt marker, found %d in %q", n, content)
	}
}

// TestPlaceholderRendersInFull is the stray "a".
//
// bubbles sizes its placeholder buffer to Width()+1 runes, so an input left at
// width zero renders only the first letter of the placeholder -- the bare "a"
// of "a tool name, or :help", sitting on the line looking like a character
// that would not go away.
func TestPlaceholderRendersInFull(t *testing.T) {
	m := viewModel(t)
	content := plain(m.View().Content)

	if !strings.Contains(content, "a tool name, or :help") {
		t.Errorf("the placeholder is truncated; the line renders as %q", content)
	}
}

// TestZeroWidthNeverReachesTheInput guards the cause rather than the symptom:
// the placeholder collapsed because the input's width was zero, and it was
// zero because nothing ever set it.
func TestZeroWidthNeverReachesTheInput(t *testing.T) {
	m := newTestModel(&fakeDispatcher{})
	if m.input.Width() < 1 {
		t.Error("the input starts at zero width, before any WindowSizeMsg arrives")
	}

	// A terminal narrower than the prompt must still leave the input usable
	// rather than going negative.
	m.Update(tea.WindowSizeMsg{Width: 2, Height: 24})
	if m.input.Width() < 1 {
		t.Errorf("a narrow terminal gave the input width %d", m.input.Width())
	}
}

// TestQuitFrameLeavesNoCursor: the shell should not leave a caret parked on
// the last line after it exits.
func TestQuitFrameLeavesNoCursor(t *testing.T) {
	m := viewModel(t)
	m.quit = true

	v := m.View()
	if v.Content != "" {
		t.Errorf("the quit frame still draws %q", v.Content)
	}
	if v.Cursor != nil {
		t.Error("the quit frame leaves a cursor behind")
	}
}

var _ = labels.All

// TestHelpColumnsAlignWhenStyled covers the padding defect.
//
// The colon commands padded with %-18s over strings carrying ANSI styling.
// Because %-Ns counts bytes, every escape sequence pushed the description
// column further right, so `:help` and `:tools` came out ragged the moment
// colour was on -- and looked like another rendering fault. ui.Table measures
// with lipgloss.Width, which is what the terminal actually shows.
func TestHelpColumnsAlignWhenStyled(t *testing.T) {
	m := newTestModel(&fakeDispatcher{})
	// The styled theme, since this bug is invisible without colour.
	m.theme = ui.New(true, true)

	var starts []int
	for line := range strings.SplitSeq(m.help(), "\n") {
		stripped := plain(line)
		// The description begins after the run of spaces following the first
		// column's text.
		trimmed := strings.TrimLeft(stripped, " ")
		lead := len(stripped) - len(trimmed)
		gap := strings.Index(trimmed, "  ")
		if gap < 0 {
			t.Fatalf("no column break in %q", stripped)
		}
		rest := strings.TrimLeft(trimmed[gap:], " ")
		starts = append(starts, lead+gap+(len(trimmed[gap:])-len(rest)))
	}

	for i, s := range starts {
		if s != starts[0] {
			t.Errorf("row %d starts its description at column %d, row 0 at %d; the columns are ragged",
				i, s, starts[0])
		}
	}
}
