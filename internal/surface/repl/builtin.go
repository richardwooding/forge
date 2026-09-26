package repl

import (
	"fmt"
	"strings"

	"github.com/richardwooding/forge/internal/labels"
)

// builtin handles the colon commands: everything that is about the shell
// rather than about a tool.
//
// The colon prefix is what keeps the two apart. Without it, installing a tool
// called "help" or "view" would shadow a shell command, and forge's whole
// premise is that tool names are not forge's to reserve.
func (m *model) builtin(args []string) (out string, quit bool) {
	switch args[0] {
	case ":quit", ":q", ":exit":
		return m.theme.Subtle.Render("bye"), true

	case ":help", ":h", ":?":
		return m.help(), false

	case ":view":
		return m.setView(args[1:]), false

	case ":views":
		return m.listViews(), false

	case ":tools", ":ls":
		return m.listTools(), false

	default:
		return m.theme.Hot.Render(fmt.Sprintf("unknown command %s; try :help", args[0])), false
	}
}

func (m *model) help() string {
	rows := [][2]string{
		{"<tool> [flags]", "run a tool, exactly as on the command line"},
		{":tools", "list the tools in the current view"},
		{":views", "list saved views"},
		{":view <name>", "switch view; :view with no name shows everything"},
		{":help", "this"},
		{":quit", "leave (or ctrl-d)"},
		{"tab", "complete a tool name or a flag"},
		{"up / down", "walk back through what you have typed"},
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-18s %s\n", m.theme.Bold.Render(r[0]), m.theme.Subtle.Render(r[1]))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *model) setView(args []string) string {
	sel := labels.All
	var name string
	if len(args) > 0 {
		name = args[0]
		resolved, resolvedName, err := m.resolveView(name)
		if err != nil {
			// Not a saved view; try it as a selector, since typing
			// ":view git && !slow" is the obvious thing to reach for.
			parsed, perr := labels.Parse(strings.Join(args, " "))
			if perr != nil {
				return m.theme.Hot.Render(err.Error())
			}
			sel, name = parsed, ""
		} else {
			sel, name = resolved, resolvedName
		}
	}

	m.opts.Selector, m.opts.ViewName = sel, name
	if err := m.opts.Dispatcher.Rebuild(sel, name); err != nil {
		return m.theme.Hot.Render(err.Error())
	}

	if m.opts.Toolkit == nil {
		return m.theme.Subtle.Render(m.viewLabel())
	}
	records, err := m.opts.Toolkit.List(sel)
	if err != nil {
		return m.theme.Hot.Render(err.Error())
	}
	return m.theme.Subtle.Render(fmt.Sprintf("%s — %d tool(s)", m.viewLabel(), len(records)))
}

// resolveView looks a name up among the saved views.
//
// Guarded because the model is usable without a store attached -- the shell
// should report that rather than crash, and it lets the model be tested
// without building a wasm tool first.
func (m *model) resolveView(name string) (labels.Selector, string, error) {
	if m.opts.Toolkit == nil {
		return nil, "", fmt.Errorf("no tool store is attached")
	}
	return m.opts.Toolkit.Views().Resolve(name, "")
}

func (m *model) listViews() string {
	if m.opts.Toolkit == nil {
		return m.theme.Hot.Render("no tool store is attached")
	}
	views := m.opts.Toolkit.Views().List()
	if len(views) == 0 {
		return m.theme.Subtle.Render("no views saved — forge view set dev 'git || json'")
	}
	active := m.opts.Toolkit.Views().ActiveName()
	var b strings.Builder
	for _, v := range views {
		marker := " "
		if v.Name == active {
			marker = "*"
		}
		fmt.Fprintf(&b, "%s %-14s %s\n", marker, m.theme.Name.Render(v.Name), m.theme.Subtle.Render(v.Selector))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *model) listTools() string {
	if m.opts.Toolkit == nil {
		return m.theme.Hot.Render("no tool store is attached")
	}
	records, err := m.opts.Toolkit.List(m.selector())
	if err != nil {
		return m.theme.Hot.Render(err.Error())
	}
	if len(records) == 0 {
		return m.theme.Subtle.Render("no tools in " + m.viewLabel())
	}
	var b strings.Builder
	for _, rec := range records {
		fmt.Fprintf(&b, "  %-16s %s  %s\n",
			m.theme.Name.Render(rec.Spec.Name),
			m.theme.Chips(rec.Labels()),
			m.theme.Subtle.Render(rec.Spec.Summary))
	}
	return strings.TrimRight(b.String(), "\n")
}
