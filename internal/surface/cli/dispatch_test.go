package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/cli"
)

// TestDispatcherRunsTheSameTreeAsTheCLI is the property the REPL exists on. A
// line typed at the prompt and the same line typed at a shell must do the same
// thing, because they are the same tree -- not because two implementations
// were kept in step.
func TestDispatcherRunsTheSameTreeAsTheCLI(t *testing.T) {
	tk := fixture(t)
	d := cli.NewDispatcher(cli.Options{Toolkit: tk, Selector: labels.All})

	cases := [][]string{
		{"hello", "--name", "world"},
		{"hello", "--name", "x", "--loud"},
		{"hello", "--name", "x", "--tags", "a,b", "--tags", "c"},
	}
	for _, args := range cases {
		viaTree, _, code := run(t, tk, labels.All, args...)
		viaRepl, _, err := d.Dispatch(context.Background(), args)
		if code != 0 {
			t.Fatalf("%v: cli exit %d", args, code)
		}
		if err != nil {
			t.Fatalf("%v: repl err %v", args, err)
		}
		if viaTree != viaRepl {
			t.Errorf("%v: cli produced %q, repl produced %q", args, viaTree, viaRepl)
		}
	}
}

func TestDispatcherReportsFailuresTheSameWay(t *testing.T) {
	tk := fixture(t)
	d := cli.NewDispatcher(cli.Options{Toolkit: tk, Selector: labels.All})

	_, _, err := d.Dispatch(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("a missing required flag was accepted")
	}
	// The same wording the CLI prints, because it is the same renderer.
	if !strings.Contains(err.Error(), "required property is missing") {
		t.Errorf("err = %q", err)
	}
}

func TestDispatcherCompletesToolNames(t *testing.T) {
	// Completion comes from cobra's own __complete, so the shell keeps no
	// second list of tools that could go stale.
	tk := fixture(t)
	d := cli.NewDispatcher(cli.Options{Toolkit: tk, Selector: labels.All})

	got := d.Complete(context.Background(), nil, "hel")
	var found bool
	for _, o := range got {
		if o == "hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("completions %v do not include the installed tool", got)
	}
}

func TestDispatcherCompletesFlags(t *testing.T) {
	tk := fixture(t)
	d := cli.NewDispatcher(cli.Options{Toolkit: tk, Selector: labels.All})

	got := d.Complete(context.Background(), []string{"hello"}, "--na")
	var found bool
	for _, o := range got {
		if strings.HasPrefix(o, "--name") {
			found = true
		}
	}
	if !found {
		t.Errorf("completions %v do not include the schema-derived flag", got)
	}
}

func TestDispatcherHonoursTheView(t *testing.T) {
	tk := fixture(t)
	d := cli.NewDispatcher(cli.Options{Toolkit: tk, Selector: labels.MustParse("sql")})

	_, _, err := d.Dispatch(context.Background(), []string{"hello", "--name", "x"})
	if err == nil {
		t.Fatal("an out-of-view tool ran")
	}
	if !strings.Contains(err.Error(), "not in the current view") {
		t.Errorf("err = %q, want the view explained", err)
	}

	// ...and Rebuild takes effect, which is what :view depends on.
	if err := d.Rebuild(labels.All, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Dispatch(context.Background(), []string{"hello", "--name", "x"}); err != nil {
		t.Errorf("after widening the view: %v", err)
	}
}

func TestDispatcherSeesAToolInstalledWhileOpen(t *testing.T) {
	// The tree is rebuilt per line, so a tool added while the shell is open is
	// callable on the next one -- the same property the MCP surface has.
	tk := fixture(t)
	d := cli.NewDispatcher(cli.Options{Toolkit: tk, Selector: labels.All})

	if _, _, err := d.Dispatch(context.Background(), []string{"second", "--name", "x"}); err == nil {
		t.Fatal("setup: second is already installed")
	}

	installSecond(t, tk)

	if _, _, err := d.Dispatch(context.Background(), []string{"second", "--name", "x"}); err != nil {
		t.Errorf("a tool installed while the shell was open is not callable: %v", err)
	}
}
