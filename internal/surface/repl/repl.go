// Package repl is forge's interactive shell.
//
// It is line-oriented and inline: it prints above the prompt rather than
// taking over the screen. That is a deliberate choice twice over. Tool output
// is arbitrary -- megabytes of JSON, or binary -- and the terminal's own
// scrollback handles that better than any pane forge could draw, while an
// alt-screen would destroy the scrollback and break terminal-native copy.
//
// More importantly, every line is handed to the same cobra tree the CLI uses,
// with its output captured. The REPL is "run the CLI in-process", not a second
// front end: a pane-based form would build input from widget state, bypassing
// flag parsing, and that second input path is exactly where the surfaces would
// start disagreeing.
package repl

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/policy"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/ui"
)

// Dispatcher runs one command line and returns what it printed.
//
// The CLI supplies this by handing its cobra tree a line's arguments with the
// streams pointed at buffers. Keeping it an interface means the REPL cannot
// reach past it and invoke a tool some other way.
type Dispatcher interface {
	Dispatch(ctx context.Context, args []string) (stdout, stderr string, err error)

	// Complete offers completions for a partial word, which is how the REPL
	// reuses cobra's own completion rather than keeping its own list of tools.
	Complete(ctx context.Context, args []string, partial string) []string

	// Rebuild picks up a changed view, so :view takes effect immediately.
	Rebuild(sel labels.Selector, viewName string) error
}

// Options configure the shell.
type Options struct {
	Toolkit    *toolkit.Toolkit
	Dispatcher Dispatcher
	Selector   labels.Selector
	ViewName   string
}

// Run starts the shell and returns when the user leaves.
func Run(ctx context.Context, opts Options) error {
	m := newModel(opts)
	m.ctx = ctx
	p := tea.NewProgram(m, tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

type model struct {
	opts  Options
	theme ui.Theme
	input textinput.Model

	// ctx is the shell's own context, used for the commands it runs. Running
	// them on context.Background() meant anything long-lived -- `serve`, `mcp`
	// -- could not be cancelled by leaving the shell.
	ctx context.Context

	history []string
	histPos int // len(history) means "not browsing"

	// width is the terminal's, from the last WindowSizeMsg. It has to reach
	// the text input: with a width of zero, bubbles sizes its placeholder
	// buffer to one rune and renders only the first letter of it, and turns
	// off horizontal scrolling so a long line wraps instead.
	width int

	busy bool
	quit bool
}

// fallbackWidth is used until the first WindowSizeMsg arrives, so the input is
// never left at zero width even for one frame.
const fallbackWidth = 80

// newPlainTheme is an unstyled theme, so a test asserts on text rather than on
// escape sequences.
func newPlainTheme() ui.Theme { return ui.New(false, true) }

func newModel(opts Options) *model {
	ti := textinput.New()
	// textinput.New defaults Prompt to "> ". The shell draws its own prompt,
	// so leaving that set renders both: `forge› > `.
	ti.Prompt = ""
	ti.Placeholder = "a tool name, or :help"
	ti.Focus()
	// The real terminal cursor is placed by View. Disabling the virtual one
	// without doing that leaves no cursor at all.
	ti.SetVirtualCursor(false)
	ti.SetWidth(fallbackWidth)

	return &model{
		opts:  opts,
		theme: ui.ForWriter(os.Stdout),
		input: ti,
		width: fallbackWidth,
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		tea.Println(m.banner()),
	)
}

func (m *model) banner() string {
	var b strings.Builder
	b.WriteString(m.theme.Name.Render("forge") + m.theme.Subtle.Render(" interactive shell"))
	b.WriteString("\n" + m.theme.Subtle.Render("  a tool name runs it · :help for the rest · ctrl-d to leave"))
	if m.opts.Toolkit == nil {
		return b.String()
	}
	if n, err := m.opts.Toolkit.List(m.selector()); err == nil {
		b.WriteString(m.theme.Subtle.Render(fmt.Sprintf("\n  %d tool(s) in %s", len(n), m.viewLabel())))
	}
	return b.String()
}

func (m *model) selector() labels.Selector {
	if m.opts.Selector == nil {
		return labels.All
	}
	return m.opts.Selector
}

func (m *model) viewLabel() string {
	if m.opts.ViewName != "" {
		return "view " + m.opts.ViewName
	}
	if s := m.selector().String(); s != "*" {
		return s
	}
	return "all tools"
}

// ranMsg carries the result of a dispatched line back to the update loop.
type ranMsg struct {
	stdout, stderr string
	err            error
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Forwarding this to the input is not optional: it is what gives the
		// placeholder room to render in full and what turns on horizontal
		// scrolling for lines longer than the terminal.
		m.width = msg.Width
		m.resizeInput()
		return m, nil

	case tea.KeyPressMsg:
		return m.onKey(msg)

	case ranMsg:
		m.busy = false
		var out []string
		if s := strings.TrimRight(msg.stderr, "\n"); s != "" {
			out = append(out, m.theme.Subtle.Render(s))
		}
		if s := strings.TrimRight(msg.stdout, "\n"); s != "" {
			out = append(out, s)
		}
		if msg.err != nil {
			out = append(out, m.theme.Hot.Render(msg.err.Error()))
		}
		if len(out) == 0 {
			return m, nil
		}
		return m, tea.Println(strings.Join(out, "\n"))
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.input.Value() != "" {
			// Clear the line rather than leaving, which is what every other
			// shell does and what a hand on ctrl-c expects.
			m.input.SetValue("")
			return m, nil
		}
		m.quit = true
		return m, tea.Quit

	case "ctrl+d":
		m.quit = true
		return m, tea.Quit

	case "enter":
		return m.submit()

	case "tab":
		return m.complete()

	case "up":
		return m.browseHistory(-1)

	case "down":
		return m.browseHistory(1)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// plan decides what a typed line means, without doing any of it.
//
// Separated from submit so the decision can be tested without going through
// Bubble Tea's command plumbing: tea.Sequence returns an opaque value a test
// cannot unwrap, and asserting on parsing through it would mean asserting on
// the framework instead of on forge.
type plan struct {
	Args    []string
	Builtin bool
	Err     error
}

func (m *model) planFor(line string) plan {
	args, err := splitLine(line)
	if err != nil {
		return plan{Err: err}
	}
	if len(args) == 0 {
		return plan{}
	}
	return plan{Args: args, Builtin: strings.HasPrefix(args[0], ":")}
}

func (m *model) submit() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	m.input.SetValue("")
	if line == "" {
		return m, nil
	}
	m.history = append(m.history, line)
	m.histPos = len(m.history)

	echo := tea.Println(m.theme.Subtle.Render("› ") + line)

	p := m.planFor(line)
	if p.Err != nil {
		return m, tea.Sequence(echo, tea.Println(m.theme.Hot.Render(p.Err.Error())))
	}
	args := p.Args
	if len(args) == 0 {
		return m, echo
	}

	if p.Builtin {
		out, quit := m.builtin(args)
		if quit {
			m.quit = true
			return m, tea.Sequence(echo, tea.Println(out), tea.Quit)
		}
		if out == "" {
			return m, echo
		}
		return m, tea.Sequence(echo, tea.Println(out))
	}

	m.busy = true
	return m, tea.Sequence(echo, m.dispatch(args))
}

// dispatch runs a line through the CLI's own command tree.
func (m *model) dispatch(args []string) tea.Cmd {
	return func() tea.Msg {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}

		// A capability prompt must not be attempted from in here. The terminal
		// prompter writes to os.Stderr and reads os.Stdin directly, and Bubble
		// Tea holds that same descriptor in raw mode -- so the question would
		// be drawn straight past the renderer and the two would race for the
		// keystroke.
		//
		// Reporting "nobody to ask" rather than "denied" is the part that
		// matters: a refusal is recorded, so answering for the user here would
		// permanently disable the tool on every surface. That was issue #2.
		// Unavailable is not recorded, and its message already names the
		// command that fixes it.
		ctx = toolkit.WithPrompter(ctx, policy.Answered(policy.Unavailable))

		stdout, stderr, err := m.opts.Dispatcher.Dispatch(ctx, args)
		return ranMsg{stdout: stdout, stderr: stderr, err: err}
	}
}

func (m *model) complete() (tea.Model, tea.Cmd) {
	line := m.input.Value()
	args, err := splitLine(line)
	if err != nil {
		return m, nil
	}

	partial := ""
	if !strings.HasSuffix(line, " ") && len(args) > 0 {
		partial = args[len(args)-1]
		args = args[:len(args)-1]
	}

	options := m.opts.Dispatcher.Complete(context.Background(), args, partial)
	switch len(options) {
	case 0:
		return m, nil
	case 1:
		// Rebuild the line from the parsed arguments so that quoting stays
		// correct: completing inside a quoted word must not produce something
		// the splitter would then read differently.
		var parts []string
		for _, a := range args {
			parts = append(parts, quote(a))
		}
		parts = append(parts, quote(options[0]))
		m.input.SetValue(strings.Join(parts, " ") + " ")
		m.input.CursorEnd()
		return m, nil
	default:
		return m, tea.Println(m.theme.Subtle.Render("  " + strings.Join(options, "  ")))
	}
}

func (m *model) browseHistory(delta int) (tea.Model, tea.Cmd) {
	if len(m.history) == 0 {
		return m, nil
	}
	pos := max(m.histPos+delta, 0)
	if pos >= len(m.history) {
		m.histPos = len(m.history)
		m.input.SetValue("")
		return m, nil
	}
	m.histPos = pos
	m.input.SetValue(m.history[pos])
	m.input.CursorEnd()
	return m, nil
}

// prompt is the text drawn before the input. View and resizeInput share it so
// the cursor offset and the input's usable width can never disagree about how
// wide it is.
func (m *model) prompt() string {
	p := m.theme.Name.Render("forge")
	if m.opts.ViewName != "" {
		p += m.theme.Subtle.Render(":" + m.opts.ViewName)
	}
	if m.busy {
		p += m.theme.Warm.Render(" …")
	}
	return p + m.theme.Subtle.Render("› ")
}

// resizeInput gives the input whatever the prompt leaves.
func (m *model) resizeInput() {
	w := max(m.width-lipgloss.Width(m.prompt()), 1)
	m.input.SetWidth(w)
}

func (m *model) View() tea.View {
	if m.quit {
		// No cursor on the way out, so the shell leaves no caret behind.
		return tea.NewView("")
	}

	prompt := m.prompt()
	v := tea.NewView(prompt + m.input.View())

	// The input's own cursor was turned off in newModel, so the real one has
	// to be placed here, shifted past the prompt. lipgloss.Width, not len:
	// the prompt carries ANSI styling and counting its bytes would put the
	// caret many columns to the right of where the text is.
	if c := m.input.Cursor(); c != nil {
		c.X += lipgloss.Width(prompt)
		v.Cursor = c
	}
	return v
}
