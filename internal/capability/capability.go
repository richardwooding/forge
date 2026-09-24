// Package capability is forge's permission model.
//
// A tool declares what it needs in its manifest; the user's policy decides what
// it gets; [Intersect] resolves the two. A [Set] is immutable once built and
// travels in the invocation's context.Context alongside a [Budget], so every
// host function can answer "is this allowed, and is there any quota left?"
// without reaching back into the registry.
//
// The package deliberately has no dependencies beyond the standard library:
// everything above it in forge references these types, so a cycle here would be
// expensive to unpick later.
package capability

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Kind names one class of thing a tool may be permitted to do.
type Kind string

const (
	FSRead    Kind = "fs.read"     // read files under a path
	FSWrite   Kind = "fs.write"    // create and modify files under a path
	NetHTTP   Kind = "net.http"    // make HTTP requests to named hosts
	Env       Kind = "env"         // read named environment variables
	Secret    Kind = "secret"      // read named secrets
	ClockWall Kind = "clock.wall"  // read the real wall clock
	KV        Kind = "kv"          // use a key/value namespace
	Invoke    Kind = "tool.invoke" // call other forge tools
)

// Kinds lists every capability, in the order forge displays them.
func Kinds() []Kind {
	return []Kind{FSRead, FSWrite, NetHTTP, Env, Secret, ClockWall, KV, Invoke}
}

// Prompts reports whether first use of this capability requires the user's
// consent. Reading the wall clock does not; reaching the network does.
func (k Kind) Prompts() bool {
	switch k {
	case FSWrite, NetHTTP, Secret, Invoke:
		return true
	case FSRead:
		return true
	default:
		return false
	}
}

// Valid reports whether k is a capability forge knows about. Anything else in a
// manifest is rejected at load time rather than silently ignored — a typo'd
// capability must not read as "wants nothing".
func (k Kind) Valid() bool { return slices.Contains(Kinds(), k) }

// Badge is the glyph forge shows for this capability in tool listings.
func (k Kind) Badge() string {
	switch k {
	case FSRead:
		return "📁"
	case FSWrite:
		return "📝"
	case NetHTTP:
		return "🌐"
	case Env:
		return "🔧"
	case Secret:
		return "🔑"
	case ClockWall:
		return "🕐"
	case KV:
		return "🗄"
	case Invoke:
		return "🔗"
	}
	return "?"
}

// Request is a tool's declared need for a capability, as harvested from its
// manifest. Reason is shown to the user at the consent prompt: a tool that
// cannot explain why it wants the network is one worth refusing.
type Request struct {
	Kind   Kind     `json:"kind"`
	Scope  []string `json:"scope,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// Grant is what the user's policy actually allows. Scope is interpreted per
// Kind: filesystem paths for FSRead/FSWrite, host patterns for NetHTTP,
// variable names for Env, secret names for Secret, namespaces for KV, and tool
// references for Invoke.
type Grant struct {
	Kind  Kind     `json:"kind"`
	Scope []string `json:"scope,omitempty"`
}

// Set is an immutable collection of grants. The zero Set grants nothing, which
// is the correct default for every code path that forgets to populate one.
type Set struct {
	grants map[Kind][]Grant
}

// NewSet builds a Set from grants. Grants of the same Kind are merged.
func NewSet(grants ...Grant) Set {
	s := Set{grants: make(map[Kind][]Grant, len(grants))}
	for _, g := range grants {
		s.grants[g.Kind] = append(s.grants[g.Kind], g)
	}
	return s
}

// Has reports whether the set carries any grant of this kind. It answers "could
// this ever be allowed", not "is this particular subject allowed" — use [Allow]
// for that.
func (s Set) Has(k Kind) bool { return len(s.grants[k]) > 0 }

// Kinds returns the granted kinds, in [Kinds] order.
func (s Set) Kinds() []Kind {
	var out []Kind
	for _, k := range Kinds() {
		if s.Has(k) {
			out = append(out, k)
		}
	}
	return out
}

// Scopes returns every scope string granted for k, in grant order.
func (s Set) Scopes(k Kind) []string {
	var out []string
	for _, g := range s.grants[k] {
		out = append(out, g.Scope...)
	}
	return out
}

// DenyCode classifies a refusal so that a guest, and forge's audit log, can
// tell "you never asked for this" from "you asked, and the user said no".
type DenyCode string

const (
	DenyNone       DenyCode = ""
	DenyNoGrant    DenyCode = "no_grant"     // no grant of this kind at all
	DenyOutOfScope DenyCode = "out_of_scope" // granted, but not for this subject
	DenyBudget     DenyCode = "budget"       // allowed, but the quota is spent
)

// Decision is the result of a capability check.
type Decision struct {
	OK     bool
	Code   DenyCode
	Detail string
}

func (d Decision) Error() string {
	if d.OK {
		return ""
	}
	return d.Detail
}

func allow() Decision { return Decision{OK: true} }

func deny(code DenyCode, format string, args ...any) Decision {
	return Decision{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Allow reports whether the set permits k for a specific subject: a path for
// the filesystem kinds, a host for NetHTTP, a name for Env and Secret, and so
// on. An empty subject asks only whether the kind is granted at all.
func (s Set) Allow(k Kind, subject string) Decision {
	gs := s.grants[k]
	if len(gs) == 0 {
		return deny(DenyNoGrant, "capability %s not granted", k)
	}
	if subject == "" {
		return allow()
	}
	for _, g := range gs {
		for _, sc := range g.Scope {
			if matchScope(k, sc, subject) {
				return allow()
			}
		}
	}
	return deny(DenyOutOfScope, "capability %s granted, but not for %q", k, subject)
}

// matchScope applies the matching rule appropriate to the kind. Filesystem
// scopes are path prefixes; network scopes allow a single leading "*." wildcard
// label; everything else is an exact name match, with "*" meaning all.
func matchScope(k Kind, scope, subject string) bool {
	if scope == "*" {
		return true
	}
	switch k {
	case FSRead, FSWrite:
		return underPath(scope, subject)
	case NetHTTP:
		return matchHost(scope, subject)
	default:
		return scope == subject
	}
}

// underPath reports whether subject lies at or below scope. Both are cleaned
// first, and the comparison is on whole path segments so that a grant for
// "/srv/app" does not cover "/srv/application".
func underPath(scope, subject string) bool {
	scope, subject = path.Clean(scope), path.Clean(subject)
	if scope == subject {
		return true
	}
	if !strings.HasSuffix(scope, "/") {
		scope += "/"
	}
	return strings.HasPrefix(subject, scope)
}

// matchHost compares a granted host pattern against a subject host. A leading
// "*." matches exactly one or more leading labels, and never the bare domain:
// a grant for "*.example.com" does not cover "example.com", because those are
// routinely different servers under different control.
func matchHost(scope, subject string) bool {
	scope, subject = strings.ToLower(scope), strings.ToLower(subject)
	if host, _, ok := strings.Cut(subject, ":"); ok {
		subject = host
	}
	if rest, found := strings.CutPrefix(scope, "*."); found {
		return strings.HasSuffix(subject, "."+rest)
	}
	return scope == subject
}

// Intersect returns the grants a callee may use when invoked by a caller: the
// callee's own grants, narrowed to what the caller itself holds. A tool can
// never gain capability by delegating, which is what makes tool.invoke safe to
// expose at all.
func Intersect(caller, callee Set) Set {
	out := Set{grants: map[Kind][]Grant{}}
	for k, cgs := range callee.grants {
		if !caller.Has(k) {
			continue
		}
		for _, g := range cgs {
			var scope []string
			for _, sc := range g.Scope {
				if caller.Allow(k, sc).OK {
					scope = append(scope, sc)
				}
			}
			if len(scope) > 0 {
				out.grants[k] = append(out.grants[k], Grant{Kind: k, Scope: scope})
			}
		}
	}
	return out
}
