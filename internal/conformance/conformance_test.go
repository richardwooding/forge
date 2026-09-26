package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/conformance"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"
)

// harness holds every driver, all pointed at one store.
type harness struct {
	drivers []conformance.Driver
	setup   conformance.Setup
}

func newHarness(t *testing.T, selector labels.Selector) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm tools with the Go toolchain; skipped in -short")
	}
	ctx := context.Background()

	sdk, err := filepath.Abs(filepath.Join("..", "..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	tk, err := toolkit.New(ctx, toolkit.Config{
		Paths: toolkit.Paths{
			Data:   filepath.Join(home, "data"),
			Cache:  filepath.Join(os.TempDir(), "forge-test-conformance"),
			Config: filepath.Join(home, "config"),
		},
		SDKReplace: sdk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close(context.Background()) })

	for _, name := range []string{"corpus", "other"} {
		src, err := os.ReadFile(filepath.Join("testdata", name+".go.txt"))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.Add(ctx, dir); err != nil {
			t.Skipf("cannot build the %s fixture: %v", name, err)
		}
	}

	views, err := view.Open(tk.Paths().Config)
	if err != nil {
		t.Fatal(err)
	}
	setup := conformance.Setup{Toolkit: tk, Views: views, Selector: selector}

	h := &harness{setup: setup}
	h.drivers = append(h.drivers, conformance.NewCLIDriver(setup))

	mcpD, closeMCP, err := conformance.NewMCPDriver(ctx, setup)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeMCP)
	h.drivers = append(h.drivers, mcpD)

	restD, closeREST := conformance.NewRESTDriver(setup)
	t.Cleanup(closeREST)
	h.drivers = append(h.drivers, restD)

	grpcD, closeGRPC, err := conformance.NewGRPCDriver(setup)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeGRPC)
	h.drivers = append(h.drivers, grpcD)

	return h
}

// The REPL is not a fifth driver here, and that is the point rather than an
// omission: it dispatches through the CLI's own cobra tree, so a driver for it
// would be testing the same code twice. What has to hold instead -- that the
// two produce identical output for identical arguments -- is asserted directly
// in cli's TestDispatcherRunsTheSameTreeAsTheCLI.

// TestEverySurfaceExposesTheSameTools is the first clause. Surfaces that
// disagree about what exists cannot agree about anything else.
func TestEverySurfaceExposesTheSameTools(t *testing.T) {
	h := newHarness(t, labels.All)
	ctx := context.Background()

	var want []string
	for _, d := range h.drivers {
		got, err := d.List(ctx)
		if err != nil {
			t.Fatalf("%s: List: %v", d.Name(), err)
		}
		if want == nil {
			want = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s lists %v\n%s lists %v", d.Name(), got, h.drivers[0].Name(), want)
		}
	}
	if len(want) < 8 {
		t.Errorf("only %d operations listed; the corpus should be larger: %v", len(want), want)
	}
}

// TestEverySurfaceAdvertisesTheSameSchema is the clause that catches the
// nastiest drift: every surface working, and the four disagreeing about what
// the tool accepts.
func TestEverySurfaceAdvertisesTheSameSchema(t *testing.T) {
	h := newHarness(t, labels.All)
	ctx := context.Background()

	names, err := h.drivers[0].List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var want string
			var from string
			for _, d := range h.drivers {
				raw, err := d.Describe(ctx, name)
				if err != nil {
					t.Fatalf("%s: Describe(%s): %v", d.Name(), name, err)
				}
				got := canon(t, raw)
				if want == "" {
					want, from = got, d.Name()
					continue
				}
				if got != want {
					t.Errorf("%s advertises a different schema from %s\n %s: %s\n %s: %s",
						d.Name(), from, d.Name(), got, from, want)
				}
			}
		})
	}
}

// call is one row of the cross-product.
type call struct {
	name  string
	input string
	why   string
}

func corpusCalls() []call {
	return []call{
		{"corpus_echo", `{"text":"plain"}`, "the simplest thing that can work"},
		{"corpus_echo", `{"text":""}`, "an empty string is a value, not an absence"},
		{"corpus_echo", `{"text":"naïve 🌍 日本語"}`, "text that is not ASCII"},
		{"corpus_echo", `{"text":"line\nbreak\ttab"}`, "whitespace that survives quoting"},
		{"corpus_echo", `{"text":"{\"looks\":\"like json\"}"}`, "text that looks like JSON but is not"},

		{"corpus_nest", `{"outer":{"inner":{"value":"deep"}},"count":3}`, "three levels down"},
		{"corpus_nest", `{"outer":{"inner":{"value":""}}}`, "a nested empty value with an absent sibling"},

		{"corpus_tags", `{"tags":["a,b","c"]}`, "the comma trap: one value contains a separator"},
		{"corpus_tags", `{"tags":["only"]}`, "a single-element array"},
		{"corpus_tags", `{}`, "an absent array is not an empty one"},

		{"corpus_big", `{"n":9007199254740993}`, "2^53+1 must survive exactly"},
		{"corpus_big", `{"n":-9007199254740993}`, "and negatively"},
		{"corpus_big", `{"n":0}`, "zero"},

		{"corpus_mode", `{"mode":"fast"}`, "an accepted value"},
		{"corpus_blob", `{"text":"x"}`, "binary output with a zero byte"},

		// Failures have to agree too. A surface that reports a bad input
		// differently from its neighbours is as broken as one that computes a
		// different answer.
		{"corpus_echo", `{}`, "a missing required property"},
		{"corpus_echo", `{"text":123}`, "the wrong type"},
		{"corpus_nest", `{"outer":{"inner":{"value":7}}}`, "the wrong type, nested"},
		{"corpus_fail", `{"text":"why"}`, "a tool that runs and fails"},
		{"corpus_panics", `{"text":"boom"}`, "a tool that panics"},
		{"corpus_mode", `{"mode":"sideways"}`, "a value the handler rejects"},
		{"nosuchtool", `{}`, "a tool that does not exist"},
	}
}

// TestEverySurfaceProducesTheSameOutcome is the invariant itself.
//
// Reported as a matrix rather than pairwise, because a pairwise failure tells
// you two surfaces differ and a matrix tells you which one is the odd one out.
func TestEverySurfaceProducesTheSameOutcome(t *testing.T) {
	h := newHarness(t, labels.All)
	ctx := context.Background()

	for _, c := range corpusCalls() {
		t.Run(c.name+" "+c.why, func(t *testing.T) {
			got := map[string]conformance.Outcome{}
			for _, d := range h.drivers {
				out, err := d.Invoke(ctx, c.name, json.RawMessage(c.input))
				if err != nil {
					t.Fatalf("%s: Invoke: %v", d.Name(), err)
				}
				got[d.Name()] = out
			}

			first := h.drivers[0].Name()
			want := got[first]
			var disagree bool
			for _, d := range h.drivers {
				o := got[d.Name()]
				if o.Class != want.Class || o.Output != want.Output {
					disagree = true
				}
			}
			if disagree {
				t.Errorf("surfaces disagree for %s %s\n%s", c.name, c.input, matrix(got))
			}
		})
	}
}

// TestAToolsOwnMessageSurvivesEverySurface: a failure a caller cannot read is
// a failure they cannot act on.
func TestAToolsOwnMessageSurvivesEverySurface(t *testing.T) {
	h := newHarness(t, labels.All)
	ctx := context.Background()

	for _, c := range []struct{ name, input, want string }{
		{"corpus_fail", `{"text":"the reason"}`, "the reason"},
		{"corpus_panics", `{"text":"the reason"}`, "the reason"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, d := range h.drivers {
				out, err := d.Invoke(ctx, c.name, json.RawMessage(c.input))
				if err != nil {
					t.Fatalf("%s: %v", d.Name(), err)
				}
				if out.Class != conformance.ClassToolError {
					t.Errorf("%s classified a failing tool as %s", d.Name(), out.Class)
				}
				if !strings.Contains(out.Message, c.want) {
					t.Errorf("%s lost the tool's own message: %q", d.Name(), out.Message)
				}
			}
		})
	}
}

// TestAViewHidesTheSameToolsEverywhere. The CLI is the deliberate exception to
// how an out-of-view tool reads, but it must still refuse to run it.
func TestAViewHidesTheSameToolsEverywhere(t *testing.T) {
	h := newHarness(t, labels.MustParse("conformance"))
	ctx := context.Background()

	for _, d := range h.drivers {
		names, err := d.List(ctx)
		if err != nil {
			t.Fatalf("%s: %v", d.Name(), err)
		}
		for _, n := range names {
			if strings.HasPrefix(n, "other") {
				t.Errorf("%s exposes %q, which the view excludes", d.Name(), n)
			}
		}

		out, err := d.Invoke(ctx, "other", json.RawMessage(`{"n":1}`))
		if err != nil {
			t.Fatalf("%s: %v", d.Name(), err)
		}
		if out.Class != conformance.ClassNotFound {
			t.Errorf("%s answered %s for an out-of-view tool, want %s",
				d.Name(), out.Class, conformance.ClassNotFound)
		}
	}
}

// TestLargeIntegersSurviveEverySurface is called out separately because it is
// the guarantee most likely to be lost by an innocuous change, and because a
// failure here is silent: the call succeeds and the number is wrong.
func TestLargeIntegersSurviveEverySurface(t *testing.T) {
	h := newHarness(t, labels.All)
	ctx := context.Background()

	const big = "9007199254740993"
	for _, d := range h.drivers {
		out, err := d.Invoke(ctx, "corpus_big", json.RawMessage(`{"n":`+big+`}`))
		if err != nil {
			t.Fatalf("%s: %v", d.Name(), err)
		}
		if !strings.Contains(out.Output, big) {
			t.Errorf("%s rounded it: %s", d.Name(), out.Output)
		}
	}
}

func canon(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// matrix renders every surface's answer, so a failure names the odd one out
// rather than just saying two of them differ.
func matrix(got map[string]conformance.Outcome) string {
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		o := got[n]
		fmt.Fprintf(&b, "  %-6s %-14s %s", n, o.Class, truncate(o.Output, 90))
		if o.Message != "" {
			fmt.Fprintf(&b, "  (%s)", truncate(o.Message, 60))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
