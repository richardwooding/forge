package mcpsrv_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"
)

// installFrom writes src into a temp dir and installs it through tk, which is
// what `forge tool add` does.
func installFrom(t *testing.T, tk *toolkit.Toolkit, src string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Add(context.Background(), dir); err != nil {
		t.Fatalf("installing the fixture: %v", err)
	}
}

// waitFor polls until cond holds or the deadline passes. The watcher ticks on
// a timer, so a test has to allow for at least one interval.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hasTool(t *testing.T, sess *mcp.ClientSession, name string) bool {
	t.Helper()
	return slices.Contains(listNames(t, sess), name)
}

// TestAToolInstalledElsewhereReachesARunningServer is issue #4. Before the
// watch, a server's tool set only changed when forge_add_tool ran inside the
// same process, so `forge tool add` from a terminal was invisible to an agent
// already connected -- and no list_changed was sent either.
func TestAToolInstalledElsewhereReachesARunningServer(t *testing.T) {
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})

	ctx := t.Context()
	mgr.StartWatch(ctx)

	srv, err := mgr.Server("", labels.All)
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	if hasTool(t, sess, "upper") {
		t.Fatal("setup: upper was already installed")
	}

	// What another process does.
	installFrom(t, tk, strings.ReplaceAll(counterSrc, "counter", "upper"))

	waitFor(t, "the new tool to reach the running server", func() bool {
		return hasTool(t, sess, "upper")
	})
}

func TestAToolRemovedElsewhereLeavesARunningServer(t *testing.T) {
	// The other direction matters more: a server still advertising a tool that
	// has been uninstalled will fail every call to it.
	tk := fixture(t)
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk})

	ctx := t.Context()
	mgr.StartWatch(ctx)

	srv, _ := mgr.Server("", labels.All)
	sess := connect(t, srv)
	if !hasTool(t, sess, "counter") {
		t.Fatal("setup: counter is not installed")
	}

	if err := tk.Remove("counter"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the removed tool to disappear", func() bool {
		return !hasTool(t, sess, "counter")
	})
}

// TestEditingAViewReachesARunningServer is the half of #4 the issue flagged
// alongside the store: a view's meaning used to be fixed when its server was
// first built, so `forge view set` never reached a running `forge mcp --http`.
func TestEditingAViewReachesARunningServer(t *testing.T) {
	tk := fixture(t)
	views, err := view.Open(tk.Paths().Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := views.Set("work", "demo"); err != nil {
		t.Fatal(err)
	}

	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, Views: views})
	ctx := t.Context()
	mgr.StartWatch(ctx)

	sel, _, err := views.Resolve("work", "")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := mgr.Server("work", sel)
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	if hasTool(t, sess, "counter") {
		t.Fatal("setup: counter should be outside the demo view")
	}

	// What `forge view set work 'demo || math'` does from a terminal.
	other, err := view.Open(tk.Paths().Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Set("work", "demo || math"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the widened view to reach the running server", func() bool {
		return hasTool(t, sess, "counter")
	})
}

// TestDeletingAViewServesNothing: the safe reading of a deleted view is an
// empty one. Falling back to everything would widen exposure on a deletion, on
// the one surface where the view is the only access boundary.
func TestDeletingAViewServesNothing(t *testing.T) {
	tk := fixture(t)
	views, err := view.Open(tk.Paths().Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := views.Set("work", "demo"); err != nil {
		t.Fatal(err)
	}

	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: tk, Views: views})
	ctx := t.Context()
	mgr.StartWatch(ctx)

	sel, _, _ := views.Resolve("work", "")
	srv, _ := mgr.Server("work", sel)
	sess := connect(t, srv)
	if len(listNames(t, sess)) == 0 {
		t.Fatal("setup: the view exposed nothing to begin with")
	}

	if err := views.Delete("work"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deleted view to stop serving", func() bool {
		return len(listNames(t, sess)) == 0
	})
}

func TestWatchStopsWithItsContext(t *testing.T) {
	// A watcher outliving its server would keep a toolkit alive and keep
	// polling for nothing.
	tk := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	events := tk.Watch(ctx)
	cancel()

	select {
	case _, open := <-events:
		if open {
			// Drain one buffered event, then it must close.
			if _, open := <-events; open {
				t.Error("the watch channel did not close after its context ended")
			}
		}
	case <-time.After(5 * time.Second):
		t.Error("the watch channel did not close after its context ended")
	}
}
