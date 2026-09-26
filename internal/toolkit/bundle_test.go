package toolkit_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/toolkit"
)

const toolSrc = `package main

import (
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	Text string ` + "`json:\"text\"`" + `
}

var _ = tool.Register(
	tool.Spec{Name: "NAME", Summary: "SUMMARY", Labels: []string{"demo"}},
	tool.Op("run", run, tool.Text(), tool.ReadOnly()),
)

func main() {}

func run(ctx *tool.Context, a Args) (string, error) { return "RESULT:" + strings.ToUpper(a.Text), nil }
`

// newHome returns a toolkit with its own, empty forge directories -- the
// stand-in for a second machine.
func newHome(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm tools with the Go toolchain; skipped in -short")
	}
	sdk, err := filepath.Abs(filepath.Join("..", "..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	tk, err := toolkit.New(context.Background(), toolkit.Config{
		Paths: toolkit.Paths{
			Data:   filepath.Join(home, "data"),
			Cache:  filepath.Join(os.TempDir(), forgeTestCache),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })
	return tk
}

func install(t *testing.T, tk *toolkit.Toolkit, summary string) {
	t.Helper()
	src := strings.ReplaceAll(toolSrc, "NAME", "greeter")
	src = strings.ReplaceAll(src, "SUMMARY", summary)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Add(context.Background(), dir); err != nil {
		t.Skipf("cannot build the fixture: %v", err)
	}
}

func export(t *testing.T, tk *toolkit.Toolkit, sel labels.Selector) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := tk.Export(&buf, sel); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestARoundTripBetweenTwoMachines is what the format is for.
func TestARoundTripBetweenTwoMachines(t *testing.T) {
	from := newHome(t)
	install(t, from, "Greets someone")

	raw := export(t, from, labels.All)

	to := newHome(t)
	res, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictSkip)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 1 || res.Installed[0] != "greeter" {
		t.Fatalf("installed %v", res.Installed)
	}

	// And it runs on the far side, which is the only thing that settles it.
	out, err := to.Invoke(context.Background(), toolkit.Call{
		Tool: "greeter", Op: "run", Input: []byte(`{"text":"world"}`),
	})
	if err != nil {
		t.Fatalf("the imported tool does not run: %v", err)
	}
	if out.Rendition.Text != "RESULT:WORLD" {
		t.Errorf("text = %q", out.Rendition.Text)
	}
}

func TestExportHonoursTheView(t *testing.T) {
	from := newHome(t)
	install(t, from, "in the view")

	// A tool with a different label, which the selector excludes.
	src := strings.ReplaceAll(toolSrc, "NAME", "other")
	src = strings.ReplaceAll(src, "SUMMARY", "out of the view")
	src = strings.ReplaceAll(src, `"demo"`, `"excluded"`)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := from.Add(context.Background(), dir); err != nil {
		t.Fatal(err)
	}

	raw := export(t, from, labels.MustParse("demo"))

	to := newHome(t)
	res, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictSkip)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 1 || res.Installed[0] != "greeter" {
		t.Errorf("installed %v, want only the in-view tool", res.Installed)
	}
}

// TestImportingGrantsNothing is the property the format exists to protect. A
// module from elsewhere is a different module whatever it calls itself, so the
// question of what it may do is asked again on the machine it lands on.
func TestImportingGrantsNothing(t *testing.T) {
	from := newHome(t)
	install(t, from, "x")
	if err := from.Policy().Grant("greeter", []capability.Grant{
		{Kind: capability.NetHTTP, Scope: []string{"example.com"}},
	}); err != nil {
		t.Fatal(err)
	}

	raw := export(t, from, labels.All)
	if bytes.Contains(raw, []byte("example.com")) {
		t.Error("the bundle carries a granted scope")
	}

	to := newHome(t)
	if _, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictSkip); err != nil {
		t.Fatal(err)
	}
	if got := to.Policy().Granted("greeter"); len(got.Kinds()) != 0 {
		t.Errorf("importing granted %v", got.Kinds())
	}
}

// TestReplacingDoesNotInheritGrants: the same reasoning, in the case where it
// would be most tempting to keep them.
func TestReplacingDoesNotInheritGrants(t *testing.T) {
	to := newHome(t)
	install(t, to, "the local one")
	if err := to.Policy().Grant("greeter", []capability.Grant{
		{Kind: capability.NetHTTP, Scope: []string{"example.com"}},
	}); err != nil {
		t.Fatal(err)
	}

	from := newHome(t)
	install(t, from, "a different one, same name")
	raw := export(t, from, labels.All)

	res, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictReplace)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Replaced) != 1 {
		t.Fatalf("replaced %v", res.Replaced)
	}
	// Assert on the policy, which is what actually decides what a tool may
	// do. This test used to check store.Record.Grants -- a field nothing read
	// at invoke time -- so it passed for the whole time the bug was live: a
	// stranger's module, imported over a granted name, kept the grant and was
	// never asked about, while the command printed "nothing is granted by
	// importing".
	if got := to.Policy().Granted("greeter"); len(got.Kinds()) != 0 {
		t.Errorf("the replacement inherited grants: %v", got.Kinds())
	}
}

func TestConflictPolicies(t *testing.T) {
	makeBundle := func(t *testing.T, summary string) []byte {
		from := newHome(t)
		install(t, from, summary)
		return export(t, from, labels.All)
	}

	t.Run("skip leaves the installed one alone", func(t *testing.T) {
		to := newHome(t)
		install(t, to, "the local one")
		raw := makeBundle(t, "the incoming one")

		res, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictSkip)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Skipped) != 1 {
			t.Fatalf("skipped %v", res.Skipped)
		}
		rec, _ := to.Get("greeter")
		if rec.Spec.Summary != "the local one" {
			t.Errorf("skip overwrote the installed tool: %q", rec.Spec.Summary)
		}
	})

	t.Run("replace overwrites", func(t *testing.T) {
		to := newHome(t)
		install(t, to, "the local one")
		raw := makeBundle(t, "the incoming one")

		if _, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictReplace); err != nil {
			t.Fatal(err)
		}
		rec, _ := to.Get("greeter")
		if rec.Spec.Summary != "the incoming one" {
			t.Errorf("replace did not overwrite: %q", rec.Spec.Summary)
		}
	})

	t.Run("rename installs alongside", func(t *testing.T) {
		to := newHome(t)
		install(t, to, "the local one")
		raw := makeBundle(t, "the incoming one")

		res, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictRename)
		if err != nil {
			t.Fatal(err)
		}
		renamed, ok := res.Renamed["greeter"]
		if !ok {
			t.Fatalf("nothing renamed: %+v", res)
		}
		if local, _ := to.Get("greeter"); local.Spec.Summary != "the local one" {
			t.Error("rename disturbed the installed tool")
		}
		incoming, err := to.Get(renamed)
		if err != nil {
			t.Fatal(err)
		}
		if incoming.Spec.Summary != "the incoming one" {
			t.Errorf("%s has the wrong contents", renamed)
		}
	})

	t.Run("fail installs nothing", func(t *testing.T) {
		to := newHome(t)
		install(t, to, "the local one")
		raw := makeBundle(t, "the incoming one")

		if _, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictFail); err == nil {
			t.Fatal("fail policy accepted a collision")
		}
		rec, _ := to.Get("greeter")
		if rec.Spec.Summary != "the local one" {
			t.Error("a failed import still changed something")
		}
	})
}

func TestImportRejectsRubbish(t *testing.T) {
	to := newHome(t)
	for name, data := range map[string][]byte{
		"empty":      {},
		"plain text": []byte("not a bundle at all"),
	} {
		if _, err := to.Import(context.Background(), bytes.NewReader(data), toolkit.ConflictSkip); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestExportRefusesWhenNothingMatches(t *testing.T) {
	// Writing an empty bundle would look like it worked and produce a file
	// that installs nothing.
	from := newHome(t)
	install(t, from, "x")
	var buf bytes.Buffer
	if _, err := from.Export(&buf, labels.MustParse("nosuchlabel")); err == nil {
		t.Error("exported an empty bundle")
	}
}

func TestUserLabelsSurviveTheTrip(t *testing.T) {
	// They are part of how someone organised their tools; losing them on every
	// transfer would make labelling not worth doing.
	from := newHome(t)
	install(t, from, "x")
	rec, err := from.Get("greeter")
	if err != nil {
		t.Fatal(err)
	}
	rec.ExtraLabels = []string{"mine"}
	if err := from.Store().Put(rec); err != nil {
		t.Fatal(err)
	}

	raw := export(t, from, labels.All)
	to := newHome(t)
	if _, err := to.Import(context.Background(), bytes.NewReader(raw), toolkit.ConflictSkip); err != nil {
		t.Fatal(err)
	}
	got, err := to.Get("greeter")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range got.Labels() {
		if l == "mine" {
			found = true
		}
	}
	if !found {
		t.Errorf("labels = %v, want the user's own to survive", got.Labels())
	}
}
