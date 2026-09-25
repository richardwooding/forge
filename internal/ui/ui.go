// Package ui is the single source of colour for forge's terminal output.
//
// The palette is the gloam token set used across these tools, expressed as
// light/dark pairs so the same code reads on either background. It follows the
// shape of wright's theme package deliberately: two terminal tools by the same
// author showing the same purple is worth more than either of them being
// individually clever.
//
// One rule runs through everything here. Colour is never the only carrier of
// meaning: a capability is a glyph and a word, a failure is a mark and a
// message. forge's output is piped into files and pagers and other tools
// constantly, and it has to survive being stripped of every escape sequence.
package ui

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// Palette holds the resolved colours for one background.
type Palette struct {
	Accent    color.Color // gloam purple: names, headings
	DimAccent color.Color // muted purple: rules, chrome
	Subtle    color.Color // grey: secondary text
	Hot       color.Color // red: refusals, failures
	Warm      color.Color // amber: warnings, capabilities worth noticing
	Good      color.Color // green: success
}

// Theme is a palette plus the styles forge composes from.
type Theme struct {
	Palette
	Enabled bool

	Name   lipgloss.Style
	Head   lipgloss.Style
	Subtle lipgloss.Style
	Good   lipgloss.Style
	Warm   lipgloss.Style
	Hot    lipgloss.Style
	Bold   lipgloss.Style
}

// New builds a theme. When enabled is false every style is a no-op, so the same
// code path produces plain text without a second set of format strings to keep
// in step.
func New(enabled, dark bool) Theme {
	pick := lipgloss.LightDark(dark)
	p := Palette{
		Accent:    pick(lipgloss.Color("#7c3aed"), lipgloss.Color("#a78bfa")),
		DimAccent: pick(lipgloss.Color("#c4b5fd"), lipgloss.Color("#4c1d95")),
		Subtle:    pick(lipgloss.Color("#6b7280"), lipgloss.Color("#9ca3af")),
		Hot:       pick(lipgloss.Color("#dc2626"), lipgloss.Color("#f87171")),
		Warm:      pick(lipgloss.Color("#d97706"), lipgloss.Color("#fbbf24")),
		Good:      pick(lipgloss.Color("#059669"), lipgloss.Color("#34d399")),
	}
	t := Theme{Palette: p, Enabled: enabled}
	if !enabled {
		plain := lipgloss.NewStyle()
		t.Name, t.Head, t.Subtle = plain, plain, plain
		t.Good, t.Warm, t.Hot, t.Bold = plain, plain, plain, plain
		return t
	}
	t.Name = lipgloss.NewStyle().Bold(true).Foreground(p.Accent)
	t.Head = lipgloss.NewStyle().Bold(true).Foreground(p.Subtle)
	t.Subtle = lipgloss.NewStyle().Foreground(p.Subtle)
	t.Good = lipgloss.NewStyle().Foreground(p.Good)
	t.Warm = lipgloss.NewStyle().Foreground(p.Warm)
	t.Hot = lipgloss.NewStyle().Foreground(p.Hot)
	t.Bold = lipgloss.NewStyle().Bold(true)
	return t
}

// ForWriter builds the theme appropriate to where output is going.
//
// NO_COLOR is honoured, and anything that is not a terminal gets plain text:
// forge's output is piped constantly, and escape sequences in a file someone
// later greps are worse than no colour at all.
func ForWriter(w io.Writer) Theme {
	return New(isTerminal(w), true)
}

func isTerminal(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// chipColours are the hues a label may be painted.
//
// A label's colour comes from a hash of its name, so "git" is the same colour
// in every listing and on every machine without anyone configuring anything.
// The set excludes red and amber, which are reserved for saying something is
// wrong.
var chipColours = []struct{ light, dark string }{
	{"#7c3aed", "#a78bfa"}, // purple
	{"#0891b2", "#22d3ee"}, // cyan
	{"#059669", "#34d399"}, // green
	{"#2563eb", "#60a5fa"}, // blue
	{"#c026d3", "#e879f9"}, // magenta
	{"#0d9488", "#2dd4bf"}, // teal
}

// Chip renders a label.
func (t Theme) Chip(label string) string {
	if !t.Enabled {
		return label
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(label))
	c := chipColours[int(h.Sum32())%len(chipColours)]
	return lipgloss.NewStyle().Foreground(lipgloss.LightDark(true)(lipgloss.Color(c.light), lipgloss.Color(c.dark))).Render(label)
}

// Chips renders a set of labels.
func (t Theme) Chips(labels []string) string {
	if len(labels) == 0 {
		return t.Subtle.Render("—")
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = t.Chip(l)
	}
	return strings.Join(out, " ")
}

// Status renders a check, a cross or a warning, always with its word alongside
// so the meaning survives being stripped of colour.
func (t Theme) OK(format string, args ...any) string {
	return t.Good.Render("✓") + " " + fmt.Sprintf(format, args...)
}

func (t Theme) Fail(format string, args ...any) string {
	return t.Hot.Render("✗") + " " + fmt.Sprintf(format, args...)
}

func (t Theme) Warn(format string, args ...any) string {
	return t.Warm.Render("!") + " " + fmt.Sprintf(format, args...)
}

// Step renders one line of a multi-step operation with its timing.
func (t Theme) Step(name string, detail string, elapsed string) string {
	return fmt.Sprintf("  %s %-9s %s %s",
		t.Good.Render("✓"), name, detail, t.Subtle.Render(elapsed))
}

// Table lays out columns that may contain styled text.
//
// text/tabwriter cannot be used for this: it measures cells in bytes, so every
// escape sequence counts towards the column width and a coloured table comes
// out visibly crooked. lipgloss.Width measures what the terminal will actually
// show, which is the only measurement that means anything here.
type Table struct {
	header []string
	rows   [][]string
	gap    int
}

// NewTable starts a table with the given column headings.
func (t Theme) NewTable(header ...string) *Table {
	styled := make([]string, len(header))
	for i, h := range header {
		styled[i] = t.Head.Render(h)
	}
	return &Table{header: styled, gap: 2}
}

// Row adds a row. Short rows are padded; long ones widen the table.
func (tb *Table) Row(cells ...string) { tb.rows = append(tb.rows, cells) }

// Render writes the table.
func (tb *Table) Render(w io.Writer) {
	cols := len(tb.header)
	for _, r := range tb.rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return
	}

	widths := make([]int, cols)
	measure := func(cells []string) {
		for i, c := range cells {
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	measure(tb.header)
	for _, r := range tb.rows {
		measure(r)
	}

	write := func(cells []string) {
		var b strings.Builder
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(cell)
			// No trailing padding on the last column: it would put invisible
			// spaces at the end of every line, which show up the moment
			// someone diffs or greps the output.
			if i < cols-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(cell)+tb.gap))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}

	if len(tb.header) > 0 {
		write(tb.header)
	}
	for _, r := range tb.rows {
		write(r)
	}
}
