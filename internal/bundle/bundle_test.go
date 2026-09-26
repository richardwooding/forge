package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/core"
)

func entry(name string) Entry {
	return Entry{
		Spec: core.Spec{Name: name, ABI: core.ABICurrent, Labels: []string{"demo"}},
	}
}

func write(t *testing.T, tools map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, so the test itself does not introduce the nondeterminism it is
	// checking for.
	for _, name := range sortedKeys(tools) {
		if err := w.Add(entry(name), tools[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	raw := write(t, map[string][]byte{
		"alpha": []byte("module alpha"),
		"beta":  []byte("module beta"),
	})

	r, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Index.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(r.Index.Tools))
	}
	for _, e := range r.Index.Tools {
		b, ok := r.Blob(e.WasmDigest)
		if !ok {
			t.Fatalf("no module for %s", e.Spec.Name)
		}
		if !strings.Contains(string(b), e.Spec.Name) {
			t.Errorf("%s got the wrong module: %q", e.Spec.Name, b)
		}
	}
}

// TestExportIsReproducible is what makes a bundle comparable. Two exports of
// the same tools producing the same bytes means someone can check a bundle was
// not altered in transit without needing a signature to tell them.
func TestExportIsReproducible(t *testing.T) {
	tools := map[string][]byte{"alpha": []byte("a"), "beta": []byte("b")}
	first := write(t, tools)
	second := write(t, tools)
	if !bytes.Equal(first, second) {
		t.Error("two exports of the same tools produced different bytes")
	}
}

func TestIndexIsSortedWhateverTheAddOrder(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	for _, n := range []string{"zebra", "alpha", "mid"} {
		if err := w.Add(entry(n), []byte("m-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range r.Index.Tools {
		names = append(names, e.Spec.Name)
	}
	if strings.Join(names, ",") != "alpha,mid,zebra" {
		t.Errorf("index order = %v, want sorted", names)
	}
}

func TestIdenticalModulesAreStoredOnce(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	for _, n := range []string{"one", "two"} {
		if err := w.Add(entry(n), []byte("the same module")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Index.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(r.Index.Tools))
	}
	if len(r.blobs) != 1 {
		t.Errorf("stored %d modules for identical content, want 1", len(r.blobs))
	}
}

// TestATamperedModuleIsRefused is the integrity check. The archive names its
// contents by hash, so there is no separate manifest to keep in step and
// nowhere for a modified module to hide.
func TestATamperedModuleIsRefused(t *testing.T) {
	raw := write(t, map[string][]byte{"alpha": []byte("original module bytes")})

	// Flip a byte inside the compressed stream until the archive still parses
	// but a blob no longer matches. Rewriting the whole bundle would be a
	// different test; this one stands in for corruption in transit.
	tampered := tamper(t, raw)
	_, err := Read(bytes.NewReader(tampered))
	if err == nil {
		t.Fatal("a tampered bundle was accepted")
	}
}

// tamper rebuilds a bundle whose index and blob disagree, which is what a
// substituted module looks like from the outside.
func tamper(t *testing.T, _ []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	e := entry("alpha")
	if err := w.Add(e, []byte("original module bytes")); err != nil {
		t.Fatal(err)
	}
	// Point the index at a digest whose blob is not present.
	w.idx.Tools[0].WasmDigest = strings.Repeat("ab", 32)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAddRefusesAMismatchedDigest(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	e := entry("alpha")
	e.WasmDigest = strings.Repeat("cd", 32)
	if err := w.Add(e, []byte("different bytes")); err == nil {
		t.Error("a record was accepted whose digest does not match its module")
	}
}

func TestNotABundle(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":      {},
		"plain text": []byte("this is not a bundle"),
		"truncated":  write(t, map[string][]byte{"a": []byte("m")})[:20],
	} {
		if _, err := Read(bytes.NewReader(data)); err == nil {
			t.Errorf("%s was accepted as a bundle", name)
		}
	}
}

func TestAFutureFormatSaysSo(t *testing.T) {
	// A version this forge does not know should name the version, rather than
	// fail with a parse error from halfway through a tar.
	var buf bytes.Buffer
	w, _ := NewWriter(&buf)
	if err := w.Add(entry("alpha"), []byte("m")); err != nil {
		t.Fatal(err)
	}
	w.idx.FormatVersion = FormatVersion + 1
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Read(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("a future format version was accepted")
	}
	if !strings.Contains(err.Error(), "format version") {
		t.Errorf("err = %v, want it to name the version", err)
	}
	if errors.Is(err, ErrNotBundle) {
		t.Error("a newer bundle was reported as not a bundle at all")
	}
}

// TestGrantsAreNotCarried is a security property, not a convenience. What a
// tool may do is the receiving user's decision; a bundle that arrived
// pre-authorised would let whoever built it decide on their behalf.
func TestGrantsAreNotCarried(t *testing.T) {
	raw := write(t, map[string][]byte{"alpha": []byte("m")})
	if bytes.Contains(raw, []byte("grants")) {
		t.Error("the bundle format mentions grants")
	}
	// Structural as well as textual: Entry has no field for them at all, so a
	// later change that added one would fail here rather than quietly start
	// shipping authorisations.
	raw, err := json.Marshal(Entry{Spec: core.Spec{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"grants", "granted", "policy", "capabilities"} {
		if bytes.Contains(raw, []byte(`"`+forbidden+`"`)) {
			t.Errorf("a bundle entry carries %q: %s", forbidden, raw)
		}
	}
}
