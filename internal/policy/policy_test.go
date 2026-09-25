package policy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
)

type answering struct {
	answer Answer
	asked  []Prompt
}

func (a *answering) Ask(_ context.Context, p Prompt) (Answer, error) {
	a.asked = append(a.asked, p)
	return a.answer, nil
}

func open(t *testing.T) *Policy {
	t.Helper()
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func netRequest() []capability.Request {
	return []capability.Request{{
		Kind:   capability.NetHTTP,
		Scope:  []string{"api.example.com"},
		Reason: "fetch the pages you ask it to summarise",
	}}
}

func TestATooWithNoRequestsIsNeverAsked(t *testing.T) {
	p := open(t)
	ask := &answering{answer: Deny}
	r := &Resolver{Policy: p, Prompter: ask}

	set, err := r.Resolve(context.Background(), "quiet", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Kinds()) != 0 {
		t.Errorf("granted %v to a tool that asked for nothing", set.Kinds())
	}
	if len(ask.asked) != 0 {
		t.Error("prompted for a tool that wanted nothing")
	}
}

func TestAllowAlwaysIsRememberedAndNotAskedTwice(t *testing.T) {
	p := open(t)
	ask := &answering{answer: AllowAlways}
	r := &Resolver{Policy: p, Prompter: ask}

	set, err := r.Resolve(context.Background(), "fetch", "", netRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !set.Allow(capability.NetHTTP, "api.example.com").OK {
		t.Error("the grant was not applied")
	}
	if _, err := r.Resolve(context.Background(), "fetch", "", netRequest()); err != nil {
		t.Fatal(err)
	}
	if len(ask.asked) != 1 {
		t.Errorf("asked %d times, want once", len(ask.asked))
	}
}

// TestAllowOnceIsNotRemembered is the whole meaning of "once": the next
// invocation must ask again.
func TestAllowOnceIsNotRemembered(t *testing.T) {
	p := open(t)
	ask := &answering{answer: AllowOnce}
	r := &Resolver{Policy: p, Prompter: ask}

	for i := 0; i < 2; i++ {
		set, err := r.Resolve(context.Background(), "fetch", "", netRequest())
		if err != nil {
			t.Fatal(err)
		}
		if !set.Allow(capability.NetHTTP, "api.example.com").OK {
			t.Fatal("allow-once did not grant")
		}
	}
	if len(ask.asked) != 2 {
		t.Errorf("asked %d times, want twice", len(ask.asked))
	}
	if len(p.Granted("fetch").Kinds()) != 0 {
		t.Error("allow-once was persisted")
	}
}

func TestDenyIsRememberedSoTheToolCannotReAsk(t *testing.T) {
	p := open(t)
	ask := &answering{answer: Deny}
	r := &Resolver{Policy: p, Prompter: ask}

	if _, err := r.Resolve(context.Background(), "fetch", "", netRequest()); !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	// A second attempt must not re-prompt: a tool that can ask on every
	// invocation is a tool that can wear someone down.
	if _, err := r.Resolve(context.Background(), "fetch", "", netRequest()); !errors.Is(err, ErrDenied) {
		t.Fatalf("second err = %v, want ErrDenied", err)
	}
	if len(ask.asked) != 1 {
		t.Errorf("asked %d times after a refusal, want once", len(ask.asked))
	}
}

func TestNoPrompterMeansDenied(t *testing.T) {
	// Running non-interactively must not silently grant everything.
	p := open(t)
	r := &Resolver{Policy: p}
	if _, err := r.Resolve(context.Background(), "fetch", "", netRequest()); !errors.Is(err, ErrDenied) {
		t.Errorf("err = %v, want ErrDenied when there is nobody to ask", err)
	}
}

// TestTheFloorIsCheckedBeforeAnyPrompt is the rule the whole package turns on.
// A policy the user can be talked into lifting is not a policy -- and being
// asked a question whose only real answer is no teaches people to stop reading
// the questions.
func TestTheFloorIsCheckedBeforeAnyPrompt(t *testing.T) {
	p := open(t)
	ask := &answering{answer: AllowAlways}
	r := &Resolver{Policy: p, Floor: DefaultFloor(), Prompter: ask}

	for _, path := range []string{"/etc", "/etc/ssh", "/", "/proc/self", "/usr/bin"} {
		_, err := r.Resolve(context.Background(), "nosy", "", []capability.Request{
			{Kind: capability.FSRead, Scope: []string{path}},
		})
		if !errors.Is(err, ErrDenied) {
			t.Errorf("%s: err = %v, want ErrDenied", path, err)
		}
	}
	if len(ask.asked) != 0 {
		t.Errorf("prompted %d times for something the floor refuses", len(ask.asked))
	}
}

func TestTheFloorProtectsForgesOwnState(t *testing.T) {
	// A tool that can write forge's state can grant itself anything next run.
	dir := t.TempDir()
	p := open(t)
	r := &Resolver{Policy: p, Floor: DefaultFloor(dir), Prompter: &answering{answer: AllowAlways}}

	_, err := r.Resolve(context.Background(), "sneaky", "", []capability.Request{
		{Kind: capability.FSWrite, Scope: []string{filepath.Join(dir, "policy.json")}},
	})
	if !errors.Is(err, ErrDenied) {
		t.Errorf("err = %v, want the floor to refuse forge's own state", err)
	}
}

func TestTheFloorComparesWholeSegments(t *testing.T) {
	// /etc must not match /etcetera.
	p := open(t)
	f := DefaultFloor()
	if why := f.Check(capability.FSRead, []string{"/etcetera"}); why != "" {
		t.Errorf("/etcetera was refused as %q", why)
	}
	if why := f.Check(capability.FSRead, []string{"/etc/passwd"}); why == "" {
		t.Error("/etc/passwd was permitted")
	}
	_ = p
}

func TestTheFloorOnlyAppliesToFilesystemCapabilities(t *testing.T) {
	f := DefaultFloor()
	if why := f.Check(capability.NetHTTP, []string{"/etc"}); why != "" {
		t.Errorf("a network scope was judged as a path: %q", why)
	}
}

func TestRequestsArePresentedTogether(t *testing.T) {
	// The combination is what carries the risk. Asked separately, reading
	// secrets and reaching the network are two easy yeses; shown together, one
	// can notice the tool could take one and send it to the other.
	p := open(t)
	ask := &answering{answer: Deny}
	r := &Resolver{Policy: p, Prompter: ask}

	_, _ = r.Resolve(context.Background(), "exfil", "", []capability.Request{
		{Kind: capability.Secret, Scope: []string{"token"}},
		{Kind: capability.NetHTTP, Scope: []string{"*"}},
	})
	if len(ask.asked) != 1 {
		t.Fatalf("asked %d times, want one prompt carrying both", len(ask.asked))
	}
	if len(ask.asked[0].Requests) != 2 {
		t.Errorf("the prompt carried %d requests, want both", len(ask.asked[0].Requests))
	}
}

func TestGrantsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Policy: p, Prompter: &answering{answer: AllowAlways}}
	if _, err := r.Resolve(context.Background(), "fetch", "", netRequest()); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Granted("fetch").Allow(capability.NetHTTP, "api.example.com").OK {
		t.Error("the grant did not survive a reopen")
	}
	if got := reopened.Tools(); len(got) != 1 || got[0] != "fetch" {
		t.Errorf("Tools() = %v", got)
	}
}

func TestRevokeClearsBothGrantAndRefusal(t *testing.T) {
	p := open(t)
	r := &Resolver{Policy: p, Prompter: &answering{answer: Deny}}
	_, _ = r.Resolve(context.Background(), "fetch", "", netRequest())

	if err := p.Revoke("fetch"); err != nil {
		t.Fatal(err)
	}
	// After revoking, the tool may be asked about again.
	ask := &answering{answer: AllowAlways}
	r2 := &Resolver{Policy: p, Prompter: ask}
	if _, err := r2.Resolve(context.Background(), "fetch", "", netRequest()); err != nil {
		t.Fatal(err)
	}
	if len(ask.asked) != 1 {
		t.Error("revoking did not clear the recorded refusal")
	}
}

func TestGrantsMergeRatherThanReplace(t *testing.T) {
	p := open(t)
	if err := p.Grant("t", []capability.Grant{{Kind: capability.NetHTTP, Scope: []string{"a.test"}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Grant("t", []capability.Grant{{Kind: capability.NetHTTP, Scope: []string{"b.test"}}}); err != nil {
		t.Fatal(err)
	}
	set := p.Granted("t")
	for _, host := range []string{"a.test", "b.test"} {
		if !set.Allow(capability.NetHTTP, host).OK {
			t.Errorf("%s was lost when the second grant was added", host)
		}
	}
	scopes := strings.Join(set.Scopes(capability.NetHTTP), ",")
	if scopes != "a.test,b.test" {
		t.Errorf("scopes = %q, want them merged and ordered", scopes)
	}
}
