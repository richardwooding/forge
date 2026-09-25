// Package labels parses and evaluates the selector expressions that decide
// which tools a surface shows.
//
// The grammar is deliberately tiny -- names, !, &&, ||, parentheses and * --
// because a selector is something people type at a prompt and store in a
// config, not a query language. Anything more expressive would need explaining;
// this needs only an example.
//
//	git                  the git label
//	git && !slow         git, but not slow
//	(git || vcs) && json
//	*                    everything
package labels

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Selector decides whether a set of labels is in view.
type Selector interface {
	// Matches reports whether the labels satisfy the selector.
	Matches(labels []string) bool
	// String returns the selector in canonical form, which is what forge
	// persists and shows back to the user.
	String() string
}

// All matches everything. It is the zero configuration: a user who has never
// created a view sees all their tools.
var All Selector = allSel{}

type allSel struct{}

func (allSel) Matches([]string) bool { return true }
func (allSel) String() string        { return "*" }

// None matches nothing. Useful as an explicit empty view.
var None Selector = noneSel{}

type noneSel struct{}

func (noneSel) Matches([]string) bool { return false }
func (noneSel) String() string        { return "!*" }

type labelSel string

func (l labelSel) Matches(labels []string) bool {
	return slices.Contains(labels, string(l))
}
func (l labelSel) String() string { return string(l) }

type notSel struct{ inner Selector }

func (n notSel) Matches(labels []string) bool { return !n.inner.Matches(labels) }
func (n notSel) String() string               { return "!" + parens(n.inner) }

type andSel struct{ left, right Selector }

func (a andSel) Matches(labels []string) bool {
	return a.left.Matches(labels) && a.right.Matches(labels)
}
func (a andSel) String() string { return parens(a.left) + " && " + parens(a.right) }

type orSel struct{ left, right Selector }

func (o orSel) Matches(labels []string) bool {
	return o.left.Matches(labels) || o.right.Matches(labels)
}
func (o orSel) String() string { return parens(o.left) + " || " + parens(o.right) }

// parens brackets a subexpression only when it could otherwise be misread.
// Canonical form has to round-trip: Parse(s.String()) must mean what s meant,
// or a persisted view changes meaning the next time it is loaded.
func parens(s Selector) string {
	switch s.(type) {
	case orSel, andSel:
		return "(" + s.String() + ")"
	}
	return s.String()
}

// Parse reads a selector. An empty string means everything, so that an unset
// view and an explicit "*" behave the same.
func Parse(expr string) (Selector, error) {
	if strings.TrimSpace(expr) == "" {
		return All, nil
	}
	p := &parser{in: expr}
	sel, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos < len(p.in) {
		return nil, p.errorf("unexpected %q", p.in[p.pos:])
	}
	return sel, nil
}

// MustParse is Parse for selectors written in forge's own source.
func MustParse(expr string) Selector {
	s, err := Parse(expr)
	if err != nil {
		panic(err)
	}
	return s
}

type parser struct {
	in  string
	pos int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("selector %q at position %d: %s", p.in, p.pos, fmt.Sprintf(format, args...))
}

func (p *parser) skipSpace() {
	for p.pos < len(p.in) && unicode.IsSpace(rune(p.in[p.pos])) {
		p.pos++
	}
}

func (p *parser) accept(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.in[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *parser) parseOr() (Selector, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept("||") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orSel{left, right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Selector, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.accept("&&") {
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = andSel{left, right}
	}
	return left, nil
}

func (p *parser) parseUnary() (Selector, error) {
	if p.accept("!") {
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if inner == All {
			return None, nil
		}
		return notSel{inner}, nil
	}
	return p.parseAtom()
}

func (p *parser) parseAtom() (Selector, error) {
	p.skipSpace()
	if p.pos >= len(p.in) {
		return nil, p.errorf("expected a label")
	}
	if p.accept("(") {
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(")") {
			return nil, p.errorf("missing closing parenthesis")
		}
		return inner, nil
	}
	if p.accept("*") {
		return All, nil
	}

	start := p.pos
	for p.pos < len(p.in) && isLabelRune(rune(p.in[p.pos])) {
		p.pos++
	}
	if p.pos == start {
		return nil, p.errorf("expected a label, found %q", string(p.in[p.pos]))
	}
	return labelSel(p.in[start:p.pos]), nil
}

// isLabelRune matches the label charset the manifest enforces, so a selector
// cannot name something that could never be a label.
func isLabelRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}
