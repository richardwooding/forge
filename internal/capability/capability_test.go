package capability

import "testing"

func TestAllowFilesystemScopeIsSegmentWise(t *testing.T) {
	s := NewSet(Grant{Kind: FSRead, Scope: []string{"/srv/app"}})

	tests := []struct {
		subject string
		want    bool
	}{
		{"/srv/app", true},
		{"/srv/app/config.yaml", true},
		{"/srv/app/deep/nested/file", true},
		{"/srv/app/", true},
		{"/srv/application", false},     // prefix, but not a path segment
		{"/srv/app-backup", false},      // ditto
		{"/srv", false},                 // parent
		{"/etc/passwd", false},          // unrelated
		{"/srv/app/../../etc/x", false}, // cleaned to /etc/x
	}
	for _, tt := range tests {
		if got := s.Allow(FSRead, tt.subject).OK; got != tt.want {
			t.Errorf("Allow(FSRead, %q) = %v, want %v", tt.subject, got, tt.want)
		}
	}
}

func TestAllowHostWildcardDoesNotCoverBareDomain(t *testing.T) {
	s := NewSet(Grant{Kind: NetHTTP, Scope: []string{"*.example.com", "api.other.test"}})

	tests := []struct {
		subject string
		want    bool
	}{
		{"a.example.com", true},
		{"deep.sub.example.com", true},
		{"a.example.com:443", true}, // port is stripped
		{"example.com", false},      // the bare domain is a different server
		{"notexample.com", false},
		{"example.com.evil.test", false},
		{"api.other.test", true},
		{"other.test", false},
	}
	for _, tt := range tests {
		if got := s.Allow(NetHTTP, tt.subject).OK; got != tt.want {
			t.Errorf("Allow(NetHTTP, %q) = %v, want %v", tt.subject, got, tt.want)
		}
	}
}

func TestAllowDistinguishesNoGrantFromOutOfScope(t *testing.T) {
	s := NewSet(Grant{Kind: FSRead, Scope: []string{"/srv"}})

	if d := s.Allow(NetHTTP, "example.com"); d.OK || d.Code != DenyNoGrant {
		t.Errorf("ungranted kind: got %+v, want DenyNoGrant", d)
	}
	if d := s.Allow(FSRead, "/etc/passwd"); d.OK || d.Code != DenyOutOfScope {
		t.Errorf("granted kind, wrong subject: got %+v, want DenyOutOfScope", d)
	}
	// The distinction is the whole point: a guest can report "forge was never
	// given the network" differently from "that host is not on the allowlist".
}

func TestZeroSetGrantsNothing(t *testing.T) {
	var s Set
	for _, k := range Kinds() {
		if d := s.Allow(k, "anything"); d.OK {
			t.Errorf("zero Set allowed %s", k)
		}
		if s.Has(k) {
			t.Errorf("zero Set has %s", k)
		}
	}
	if got := s.Kinds(); len(got) != 0 {
		t.Errorf("zero Set Kinds() = %v, want empty", got)
	}
}

func TestEmptySubjectAsksOnlyAboutTheKind(t *testing.T) {
	s := NewSet(Grant{Kind: Secret, Scope: []string{"token"}})
	if !s.Allow(Secret, "").OK {
		t.Error("empty subject should ask only whether the kind is granted")
	}
	if s.Allow(Secret, "other").OK {
		t.Error("named subject must still be scope-checked")
	}
}

func TestIntersectNeverWidens(t *testing.T) {
	caller := NewSet(
		Grant{Kind: FSRead, Scope: []string{"/srv/app"}},
		Grant{Kind: NetHTTP, Scope: []string{"api.example.com"}},
	)
	callee := NewSet(
		Grant{Kind: FSRead, Scope: []string{"/srv/app", "/etc"}}, // /etc must be dropped
		Grant{Kind: NetHTTP, Scope: []string{"evil.test"}},       // wholly dropped
		Grant{Kind: Secret, Scope: []string{"token"}},            // caller has no Secret at all
	)

	got := Intersect(caller, callee)

	if !got.Allow(FSRead, "/srv/app/x").OK {
		t.Error("shared filesystem scope should survive")
	}
	if got.Allow(FSRead, "/etc/passwd").OK {
		t.Error("callee gained a filesystem scope its caller lacks")
	}
	if got.Has(NetHTTP) {
		t.Error("callee kept a net grant with no overlapping scope")
	}
	if got.Has(Secret) {
		t.Error("callee kept a capability kind the caller never had")
	}
}

func TestIntersectWithEmptyCallerYieldsNothing(t *testing.T) {
	callee := NewSet(Grant{Kind: FSRead, Scope: []string{"/"}}, Grant{Kind: NetHTTP, Scope: []string{"*"}})
	if got := Intersect(Set{}, callee); len(got.Kinds()) != 0 {
		t.Errorf("Intersect(zero, callee) = %v, want nothing", got.Kinds())
	}
}

func TestWildcardScopeMatchesAnySubject(t *testing.T) {
	s := NewSet(Grant{Kind: NetHTTP, Scope: []string{"*"}})
	for _, subject := range []string{"example.com", "127.0.0.1:8080", "anything"} {
		if !s.Allow(NetHTTP, subject).OK {
			t.Errorf(`scope "*" did not match %q`, subject)
		}
	}
}

func TestEveryKindIsValidAndHasABadge(t *testing.T) {
	for _, k := range Kinds() {
		if !k.Valid() {
			t.Errorf("Kinds() returned %q, which Valid() rejects", k)
		}
		if k.Badge() == "?" {
			t.Errorf("kind %q has no badge", k)
		}
	}
	if Kind("fs.reed").Valid() {
		t.Error("a typo'd capability must not validate")
	}
}
