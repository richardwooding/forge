package repl

import (
	"fmt"
	"strings"
)

// splitLine breaks a typed line into arguments, POSIX-style.
//
// Hand-written rather than pulled from a shell parser. forge does not want a
// shell: there is no globbing, no expansion, no pipelines and no substitution
// here, and a real parser would bring all of that along with the expectation
// that it works. Quotes and backslashes are the whole grammar, which is what a
// person typing a tool invocation actually reaches for.
func splitLine(line string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case ' ', '\t':
			flush()
		case '\'':
			// Single quotes are literal, including backslashes, as in a shell.
			started = true
			j := i + 1
			for ; j < len(runes) && runes[j] != '\''; j++ {
				cur.WriteRune(runes[j])
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unclosed '")
			}
			i = j
		case '"':
			started = true
			j := i + 1
			for ; j < len(runes) && runes[j] != '"'; j++ {
				if runes[j] == '\\' && j+1 < len(runes) {
					j++
				}
				cur.WriteRune(runes[j])
			}
			if j >= len(runes) {
				return nil, fmt.Errorf(`unclosed "`)
			}
			i = j
		case '\\':
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				started = true
			}
		default:
			cur.WriteRune(c)
			started = true
		}
	}
	flush()
	return args, nil
}

// quote renders an argument so that splitLine would read it back unchanged.
//
// It is the inverse of the split, and it exists because the REPL shows the
// user the command line it is about to run: what they are shown has to be
// something they could have typed themselves.
func quote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t'\"\\") {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
