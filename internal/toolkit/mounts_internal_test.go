package toolkit

import (
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/capability/rootfs"
)

func set(grants ...capability.Grant) capability.Set { return capability.NewSet(grants...) }

func TestMountsForModes(t *testing.T) {
	got, err := mountsFor(set(
		capability.Grant{Kind: capability.FSRead, Scope: []string{"/srv/in"}},
		capability.Grant{Kind: capability.FSWrite, Scope: []string{"/srv/out"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got), got)
	}
	// Sorted, so the order is the same on every run.
	if got[0].HostPath != "/srv/in" || got[1].HostPath != "/srv/out" {
		t.Errorf("mounts are not in sorted order: %+v", got)
	}
	if got[0].Mode != rootfs.ReadOnly {
		t.Errorf("fs.read produced mode %v, want ReadOnly", got[0].Mode)
	}
	if got[1].Mode != rootfs.ReadWrite {
		t.Errorf("fs.write produced mode %v, want ReadWrite", got[1].Mode)
	}
	for _, m := range got {
		if m.GuestPath != m.HostPath {
			t.Errorf("%s is mounted at %s; a scope keeps its host spelling so the paths a "+
				"caller passes still mean what they say", m.HostPath, m.GuestPath)
		}
	}
}

// TestMountsForUpgradesToWritable: a directory granted both ways is mounted
// once, writable. Mounting it twice is not expressible, and read-only would
// make the write grant a lie.
func TestMountsForUpgradesToWritable(t *testing.T) {
	for _, order := range [][]capability.Grant{
		{
			{Kind: capability.FSRead, Scope: []string{"/srv/both"}},
			{Kind: capability.FSWrite, Scope: []string{"/srv/both"}},
		},
		{
			{Kind: capability.FSWrite, Scope: []string{"/srv/both"}},
			{Kind: capability.FSRead, Scope: []string{"/srv/both"}},
		},
	} {
		got, err := mountsFor(set(order...))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d mounts for one directory: %+v", len(got), got)
		}
		if got[0].Mode != rootfs.ReadWrite {
			t.Errorf("a directory granted read and write is mounted %v, want ReadWrite", got[0].Mode)
		}
	}
}

func TestMountsForCleansAndDeduplicates(t *testing.T) {
	got, err := mountsFor(set(capability.Grant{
		Kind:  capability.FSRead,
		Scope: []string{"/srv/data", "/srv/data/", "/srv/x/../data"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("three spellings of one directory produced %d mounts: %+v", len(got), got)
	}
	if got[0].HostPath != "/srv/data" {
		t.Errorf("got %q, want the cleaned path", got[0].HostPath)
	}
}

// TestMountsForRefusesUnmountableScopes: a scope that cannot be mounted is an
// error, never a skip. Skipping is precisely what produced the bug this
// package now guards against -- a grant that reads as held everywhere it is
// displayed and does nothing where it is used.
func TestMountsForRefusesUnmountableScopes(t *testing.T) {
	for _, scope := range []string{"*", "relative/path", "./here", ""} {
		_, err := mountsFor(set(capability.Grant{
			Kind: capability.FSRead, Scope: []string{scope},
		}))
		if err == nil {
			t.Errorf("the scope %q was accepted as a mount", scope)
			continue
		}
		if scope == "*" && !strings.Contains(err.Error(), "entire filesystem") {
			t.Errorf("the message for %q should say what it would mean: %v", scope, err)
		}
	}
}

func TestMountsForNoFilesystemGrants(t *testing.T) {
	got, err := mountsFor(set(capability.Grant{
		Kind: capability.NetHTTP, Scope: []string{"example.com"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a tool with no filesystem grant got %d mounts; it must get none, so "+
			"path_open reports unsupported and os.Open returns an ordinary error", len(got))
	}
}
