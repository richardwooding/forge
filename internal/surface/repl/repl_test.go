package repl

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/forge/internal/labels"
)

// fakeDispatcher records what the shell asked for, so the model can be tested
// without building wasm tools.
type fakeDispatcher struct {
	lines    [][]string
	stdout   string
	err      error
	options  []string
	rebuilds []string
}

func (f *fakeDispatcher) Dispatch(_ context.Context, args []string) (string, string, error) {
	f.lines = append(f.lines, args)
	return f.stdout, "", f.err
}

func (f *fakeDispatcher) Complete(_ context.Context, args []string, partial string) []string {
	return f.options
}

func (f *fakeDispatcher) Rebuild(sel labels.Selector, name string) error {
	f.rebuilds = append(f.rebuilds, name)
	return nil
}

// typeLine feeds a line and Enter, then performs whatever the line planned.
//
// It drives planFor and the dispatcher directly rather than executing the
// Bubble Tea commands submit returns: tea.Sequence is opaque, and unwrapping
// it would mean asserting on the framework rather than on forge.
func typeLine(t *testing.T, m *model, line string) {
	t.Helper()
	m.input.SetValue(line)
	m.onKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	p := m.planFor(strings.TrimSpace(line))
	switch {
	case p.Err != nil, len(p.Args) == 0:
		return
	case p.Builtin:
		m.builtin(p.Args)
	default:
		_, _, _ = m.opts.Dispatcher.Dispatch(context.Background(), p.Args)
	}
}

func newTestModel(d *fakeDispatcher) *model {
	m := newModel(Options{Dispatcher: d, Selector: labels.All})
	// Deterministic output regardless of where the test runs.
	m.theme = newPlainTheme()
	return m
}

func TestALineBecomesArgvForTheDispatcher(t *testing.T) {
	d := &fakeDispatcher{stdout: "ok"}
	m := newTestModel(d)

	typeLine(t, m, `hello --name "two words" --tag a,b`)

	if len(d.lines) != 1 {
		t.Fatalf("dispatched %d lines, want 1", len(d.lines))
	}
	want := []string{"hello", "--name", "two words", "--tag", "a,b"}
	if strings.Join(d.lines[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", d.lines[0], want)
	}
}

func TestColonCommandsNeverReachTheDispatcher(t *testing.T) {
	// The colon prefix is what stops a tool named "help" or "view" shadowing a
	// shell command. Tool names are not forge's to reserve.
	d := &fakeDispatcher{}
	m := newTestModel(d)

	for _, line := range []string{":help", ":tools", ":views", ":nonsense"} {
		typeLine(t, m, line)
	}
	if len(d.lines) != 0 {
		t.Errorf("a colon command was dispatched as a tool: %q", d.lines)
	}
}

func TestAnUnclosedQuoteIsReportedNotDispatched(t *testing.T) {
	d := &fakeDispatcher{}
	m := newTestModel(d)
	typeLine(t, m, `hello --name "unclosed`)
	if len(d.lines) != 0 {
		t.Errorf("a malformed line was dispatched: %q", d.lines)
	}
}

func TestHistoryWalksBackAndForward(t *testing.T) {
	d := &fakeDispatcher{}
	m := newTestModel(d)
	typeLine(t, m, "first")
	typeLine(t, m, "second")

	m.onKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.input.Value(); got != "second" {
		t.Errorf("one up = %q, want the most recent line", got)
	}
	m.onKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.input.Value(); got != "first" {
		t.Errorf("two up = %q", got)
	}
	m.onKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.input.Value(); got != "second" {
		t.Errorf("back down = %q", got)
	}
	// Past the end is an empty line, ready to type something new.
	m.onKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.input.Value(); got != "" {
		t.Errorf("past the end = %q, want empty", got)
	}
}

func TestTabCompletesASingleCandidate(t *testing.T) {
	d := &fakeDispatcher{options: []string{"jsonfmt"}}
	m := newTestModel(d)
	m.input.SetValue("json")

	m.onKey(tea.KeyPressMsg{Code: tea.KeyTab})

	if got := m.input.Value(); got != "jsonfmt " {
		t.Errorf("completed to %q, want %q", got, "jsonfmt ")
	}
}

// TestCompletionRequotesTheLine: completing inside a quoted word must leave a
// line the splitter reads the same way, or what the user sees stops being what
// they could have typed.
func TestCompletionRequotesTheLine(t *testing.T) {
	d := &fakeDispatcher{options: []string{"value with spaces"}}
	m := newTestModel(d)
	m.input.SetValue(`hello --name `)

	m.onKey(tea.KeyPressMsg{Code: tea.KeyTab})

	got, err := splitLine(m.input.Value())
	if err != nil {
		t.Fatalf("completion produced an unparseable line %q: %v", m.input.Value(), err)
	}
	want := []string{"hello", "--name", "value with spaces"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("line %q splits to %q, want %q", m.input.Value(), got, want)
	}
}

func TestTabWithSeveralCandidatesLeavesTheLineAlone(t *testing.T) {
	d := &fakeDispatcher{options: []string{"jsonfmt_format", "jsonfmt_minify"}}
	m := newTestModel(d)
	m.input.SetValue("json")

	m.onKey(tea.KeyPressMsg{Code: tea.KeyTab})

	if got := m.input.Value(); got != "json" {
		t.Errorf("an ambiguous completion changed the line to %q", got)
	}
}

func TestCtrlCClearsThenQuits(t *testing.T) {
	// What every other shell does, and what a hand already on ctrl-c expects.
	d := &fakeDispatcher{}
	m := newTestModel(d)
	m.input.SetValue("half typed")

	m.onKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.input.Value() != "" {
		t.Error("ctrl-c did not clear the line")
	}
	if m.quit {
		t.Error("ctrl-c quit while there was something to clear")
	}

	m.onKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !m.quit {
		t.Error("ctrl-c on an empty line did not quit")
	}
}

func TestViewCommandRebuildsTheTree(t *testing.T) {
	// :view has to reach the dispatcher, or the shell would keep offering the
	// old view's tools.
	d := &fakeDispatcher{}
	m := newTestModel(d)

	out, quit := m.builtin([]string{":view", "git && !slow"})
	if quit {
		t.Fatal(":view quit the shell")
	}
	if len(d.rebuilds) != 1 {
		t.Fatalf("the dispatcher was rebuilt %d times, want once", len(d.rebuilds))
	}
	if !strings.Contains(out, "git") {
		t.Errorf("output %q does not name the new view", out)
	}
}

func TestQuitCommands(t *testing.T) {
	m := newTestModel(&fakeDispatcher{})
	for _, cmd := range []string{":quit", ":q", ":exit"} {
		if _, quit := m.builtin([]string{cmd}); !quit {
			t.Errorf("%s did not quit", cmd)
		}
	}
}
