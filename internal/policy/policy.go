// Package policy decides what a tool is allowed to do.
//
// A tool declares what it wants; the user decides what it gets. This package
// holds the middle: the floor of things nothing may ever have, the prompt that
// asks about everything else, and the record of what was answered so the same
// question is not asked twice.
//
// The shape follows wright's permission layer, and so does the one rule that
// matters most: the floor is checked before any grant, and no answer to any
// prompt can lift it. A policy where the user can be talked into anything is
// not a policy.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/forge/internal/capability"
)

// Answer is what a person said when asked.
type Answer int

const (
	// Deny is a person saying no. It is recorded, so the tool cannot re-ask on
	// every invocation and wear them down.
	Deny Answer = iota
	// AllowOnce permits this invocation and asks again next time.
	AllowOnce
	// AllowAlways permits and records the grant.
	AllowAlways
	// Unavailable means there was nobody to ask.
	//
	// This is emphatically NOT a refusal, and keeping the two apart is the
	// whole reason it exists. Recording "I could not ask" as "they said no"
	// makes the first call from any surface without a human attached -- a
	// server, a script, an MCP session -- permanently kill the tool, with no
	// prompt ever shown to anyone and no hint as to why. The call still fails,
	// because nothing may be granted unasked; it just fails recoverably.
	Unavailable
)

func (a Answer) String() string {
	switch a {
	case AllowOnce:
		return "allow once"
	case AllowAlways:
		return "allow always"
	case Unavailable:
		return "nobody to ask"
	default:
		return "deny"
	}
}

// Request is one capability a tool wants, as put to the user.
type Request struct {
	Tool   string
	Kind   capability.Kind
	Scope  []string
	Reason string
}

// Prompt is everything a tool is asking for at once.
//
// Requests are presented together rather than one at a time, because the
// combination is what carries the risk. Being asked separately about reading
// secrets and about reaching the network invites two easy yeses; being shown
// both together is what lets someone notice that a tool can take one and send
// it to the other.
type Prompt struct {
	Tool     string
	Summary  string
	Requests []Request
}

// Prompter asks a person.
type Prompter interface {
	Ask(ctx context.Context, p Prompt) (Answer, error)
}

// ErrDenied is returned when a capability was refused.
var ErrDenied = errors.New("capability denied")

// DenyAll is a Prompter that always refuses, as though a person had said no.
// It exists for tests; production code that has nobody to ask should say so
// with Unavailable instead, so that the refusal is not recorded.
type DenyAll struct{}

// Ask implements Prompter.
func (DenyAll) Ask(context.Context, Prompt) (Answer, error) { return Deny, nil }

// NoPrompter is the Prompter for a context with nobody to ask.
//
// It is the default when none is configured. It withholds the capability --
// nothing may be granted without being asked for -- while making clear that
// the question was never put, so the refusal is not written down.
type NoPrompter struct{}

// Ask implements Prompter.
func (NoPrompter) Ask(context.Context, Prompt) (Answer, error) { return Unavailable, nil }

// ErrCannotAsk reports that a capability was withheld because there was nobody
// to ask, as distinct from someone having refused.
var ErrCannotAsk = errors.New("no way to ask for approval")

// NeedsApproval is returned when a tool needs a capability and this session
// could not put the question. It carries the unanswered prompt.
//
// A surface that can ask, but not synchronously, uses it to do so: MCP forbids
// a server from raising an elicitation while it is serving a request, so the
// question has to be handed back as part of the reply and re-entered with the
// answer attached. Carrying the prompt in the error is what lets the surface
// do that without reaching into the policy layer to work out what to ask.
type NeedsApproval struct {
	Tool     string
	Summary  string
	Requests []Request
}

func (e *NeedsApproval) Error() string {
	return fmt.Sprintf("%s: %s needs %s, and this session has no way to ask you; approve it with `forge grant allow %s`",
		ErrCannotAsk, e.Tool, describe(e.Requests), e.Tool)
}

func (e *NeedsApproval) Unwrap() error { return ErrCannotAsk }

// Answered is a Prompter that replays an answer already collected elsewhere.
//
// It exists for the multi-round-trip case: the second call knows what the
// person said, and replaying it through the ordinary path means persistence,
// merging and the floor all behave exactly as they do for a prompt answered
// on a terminal, rather than being reimplemented per surface.
type Answered Answer

// Ask implements Prompter.
func (a Answered) Ask(context.Context, Prompt) (Answer, error) { return Answer(a), nil }

// Policy holds the recorded decisions.
//
// The file on disk is the source of truth, not this process's copy of it.
// forge is routinely several processes at once -- a long-lived `forge mcp`
// serving an agent while someone runs `forge grant allow` in a terminal -- and
// a server that cached its answers at startup would go on refusing a tool the
// user had just approved, for as long as it stayed up. So every read checks
// whether the file has changed underneath it first.
type Policy struct {
	path string

	mu    sync.RWMutex
	state state
	// stamp identifies the file contents this state was read from.
	stamp fileStamp
}

// fileStamp is what Policy compares to decide whether its copy is still
// current. Modification time alone is too coarse -- two writes within one
// filesystem timestamp tick are entirely possible here, since a grant and a
// revoke can land in the same second -- so the size is part of it, and a write
// through this process records its own stamp directly rather than inferring
// one.
type fileStamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) (fileStamp, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, false
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size()}, true
}

// refresh re-reads the file when it has changed since this copy was made.
//
// Callers hold no lock. A failed re-read leaves the last good state in place:
// a policy file that is briefly unreadable -- mid-write by another process,
// say -- must not be treated as a policy that grants nothing, which would turn
// a transient filesystem state into a wave of spurious refusals.
func (p *Policy) refresh() {
	stamp, ok := stampOf(p.path)

	p.mu.RLock()
	current := p.stamp
	p.mu.RUnlock()
	if !ok || stamp == current {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stamp == stamp {
		return // another goroutine got there first
	}
	if err := p.loadLocked(); err != nil {
		return
	}
}

type state struct {
	// Grants are what the user agreed to, keyed by tool name.
	Grants map[string][]capability.Grant `json:"grants,omitempty"`
	// Denied records refusals so a tool cannot re-ask on every invocation.
	Denied map[string][]string `json:"denied,omitempty"`
}

// Open reads the policy file.
func Open(dir string) (*Policy, error) {
	p := &Policy{path: filepath.Join(dir, "policy.json")}
	if err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}

// Floor lists what no tool may have, whatever anyone answers.
//
// These are paths where a grant would not mean what the user thought it meant.
// forge's own state is here because a tool that can write it can grant itself
// anything on the next run; the system directories are here because a tool
// asking for them is either broken or hostile, and there is no version of
// "yes" that makes it safe.
type Floor struct {
	// DeniedPaths are prefixes no filesystem grant may cover.
	DeniedPaths []string
}

// DefaultFloor builds the floor for this machine.
func DefaultFloor(forgeDirs ...string) Floor {
	f := Floor{DeniedPaths: []string{
		"/etc", "/proc", "/sys", "/dev", "/boot", "/root",
		"/var/run", "/run", "/usr", "/bin", "/sbin", "/lib",
	}}
	for _, d := range forgeDirs {
		if abs, err := filepath.Abs(d); err == nil {
			f.DeniedPaths = append(f.DeniedPaths, abs)
		}
	}
	return f
}

// Check reports why a request is refused outright, or "" when the floor permits
// it. The check is on the cleaned, symlink-resolved path: a fixed list of names
// compared against values that have not been resolved is a hole, and comparing
// resolved spellings is the only version that holds.
func (f Floor) Check(kind capability.Kind, scope []string) string {
	if kind != capability.FSRead && kind != capability.FSWrite {
		return ""
	}
	for _, s := range scope {
		abs, err := filepath.Abs(s)
		if err != nil {
			return fmt.Sprintf("%s is not a usable path", s)
		}
		resolved := abs
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = r
		}
		if abs == "/" || resolved == "/" {
			return "the whole filesystem"
		}
		for _, denied := range f.DeniedPaths {
			resolvedDenied := denied
			if r, err := filepath.EvalSymlinks(denied); err == nil {
				resolvedDenied = r
			}
			if under(resolved, resolvedDenied) || under(abs, denied) {
				return denied
			}
		}
	}
	return ""
}

// under reports whether path is at or below prefix, comparing whole segments so
// that /etc does not match /etcetera.
func under(path, prefix string) bool {
	path, prefix = filepath.Clean(path), filepath.Clean(prefix)
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+string(filepath.Separator))
}

// Granted returns what a tool currently holds, re-reading the file if another
// process has written it since this copy was made.
func (p *Policy) Granted(tool string) capability.Set {
	p.refresh()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return capability.NewSet(p.state.Grants[tool]...)
}

// Resolver decides a tool's grants for one invocation, asking if it must.
type Resolver struct {
	Policy   *Policy
	Floor    Floor
	Prompter Prompter
}

// Resolve returns the grants a tool may use.
//
// Anything the floor denies is refused before the user is asked, so a prompt is
// never shown for something that could not be allowed anyway -- being asked a
// question whose only real answer is no teaches people to stop reading the
// questions.
func (r *Resolver) Resolve(ctx context.Context, tool, summary string, requests []capability.Request) (capability.Set, error) {
	if len(requests) == 0 {
		return capability.Set{}, nil
	}

	for _, req := range requests {
		if why := r.Floor.Check(req.Kind, req.Scope); why != "" {
			return capability.Set{}, fmt.Errorf("%w: %s asks for %s covering %s, which forge never grants",
				ErrDenied, tool, req.Kind, why)
		}
	}

	held := r.Policy.Granted(tool)
	var missing []Request
	for _, req := range requests {
		for _, scope := range req.Scope {
			if held.Allow(req.Kind, scope).OK {
				continue
			}
			missing = append(missing, Request{
				Tool: tool, Kind: req.Kind, Scope: req.Scope, Reason: req.Reason,
			})
			break
		}
	}
	if len(missing) == 0 {
		return held, nil
	}

	if r.Policy.deniedBefore(tool, missing) {
		return capability.Set{}, fmt.Errorf("%w: %s was refused these capabilities before; run `forge grant allow %s` to change that",
			ErrDenied, tool, tool)
	}

	prompter := r.Prompter
	if prompter == nil {
		prompter = NoPrompter{}
	}
	answer, err := prompter.Ask(ctx, Prompt{Tool: tool, Summary: summary, Requests: missing})
	if err != nil {
		return capability.Set{}, err
	}

	switch answer {
	case AllowAlways:
		grants := grantsFor(requests)
		if err := r.Policy.Grant(tool, grants); err != nil {
			return capability.Set{}, err
		}
		return capability.NewSet(grants...), nil
	case AllowOnce:
		// Not persisted: the whole point of "once" is that the next invocation
		// asks again.
		return capability.NewSet(grantsFor(requests)...), nil
	case Unavailable:
		// Deliberately not recorded. The question was never put, so there is no
		// answer to remember, and remembering one would silently and
		// permanently disable the tool on every surface.
		return capability.Set{}, &NeedsApproval{Tool: tool, Summary: summary, Requests: missing}
	default:
		if err := r.Policy.Refuse(tool, missing); err != nil {
			return capability.Set{}, err
		}
		return capability.Set{}, fmt.Errorf("%w: %s may not %s", ErrDenied, tool, describe(missing))
	}
}

func grantsFor(requests []capability.Request) []capability.Grant {
	out := make([]capability.Grant, 0, len(requests))
	for _, r := range requests {
		out = append(out, capability.Grant{Kind: r.Kind, Scope: r.Scope})
	}
	return out
}

func describe(reqs []Request) string {
	var parts []string
	for _, r := range reqs {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.Kind, strings.Join(r.Scope, ", ")))
	}
	return strings.Join(parts, " or ")
}

// Grant records capabilities for a tool.
func (p *Policy) Grant(tool string, grants []capability.Grant) error {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Grants == nil {
		p.state.Grants = map[string][]capability.Grant{}
	}
	p.state.Grants[tool] = mergeGrants(p.state.Grants[tool], grants)
	delete(p.state.Denied, tool)
	return p.save()
}

// Revoke removes every grant for a tool.
func (p *Policy) Revoke(tool string) error {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state.Grants, tool)
	delete(p.state.Denied, tool)
	return p.save()
}

// Refuse records that a tool was told no, so that it cannot re-ask on every
// invocation.
func (p *Policy) Refuse(tool string, reqs []Request) error {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Denied == nil {
		p.state.Denied = map[string][]string{}
	}
	for _, r := range reqs {
		key := string(r.Kind)
		if !slices.Contains(p.state.Denied[tool], key) {
			p.state.Denied[tool] = append(p.state.Denied[tool], key)
		}
	}
	sort.Strings(p.state.Denied[tool])
	return p.save()
}

func (p *Policy) deniedBefore(tool string, reqs []Request) bool {
	p.refresh()
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range reqs {
		if slices.Contains(p.state.Denied[tool], string(r.Kind)) {
			return true
		}
	}
	return false
}

// Tools lists every tool with a recorded decision.
func (p *Policy) Tools() []string {
	p.refresh()
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for t := range p.state.Grants {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for t := range p.state.Denied {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// mergeGrants combines grants of the same kind, keeping scopes unique.
func mergeGrants(existing, added []capability.Grant) []capability.Grant {
	byKind := map[capability.Kind][]string{}
	order := []capability.Kind{}
	for _, g := range slices.Concat(existing, added) {
		if _, seen := byKind[g.Kind]; !seen {
			order = append(order, g.Kind)
		}
		for _, s := range g.Scope {
			if !slices.Contains(byKind[g.Kind], s) {
				byKind[g.Kind] = append(byKind[g.Kind], s)
			}
		}
	}
	out := make([]capability.Grant, 0, len(order))
	for _, k := range order {
		sort.Strings(byKind[k])
		out = append(out, capability.Grant{Kind: k, Scope: byKind[k]})
	}
	return out
}

func (p *Policy) load() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadLocked()
}

// loadLocked reads the file into state. The caller holds the write lock.
func (p *Policy) loadLocked() error {
	b, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			p.state = state{}
			p.stamp = fileStamp{}
			return nil
		}
		return err
	}
	var fresh state
	if err := json.Unmarshal(b, &fresh); err != nil {
		return fmt.Errorf("reading %s: %w", p.path, err)
	}
	p.state = fresh
	// Stamp after reading, so a write that lands between the stat and the read
	// leaves a stamp that does not match and is picked up next time, rather
	// than one that claims this copy is current when it is not.
	if stamp, ok := stampOf(p.path); ok {
		p.stamp = stamp
	}
	return nil
}

func (p *Policy) save() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(p.path, append(b, '\n')); err != nil {
		return err
	}
	// Record our own write, so the next read does not mistake it for someone
	// else's and reload what it already has.
	if stamp, ok := stampOf(p.path); ok {
		p.stamp = stamp
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
