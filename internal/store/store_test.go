package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/core"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *Store, name string, wasm []byte, labels ...string) Record {
	t.Helper()
	digest, err := s.PutBlob(wasm)
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{
		Spec:       core.Spec{Name: name, ABI: core.ABICurrent, Labels: labels},
		WasmDigest: digest,
	}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestPutAndGet(t *testing.T) {
	s := open(t)
	put(t, s, "hello", []byte("fake wasm"), "demo")

	got, err := s.Get("hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Name != "hello" {
		t.Errorf("name = %q", got.Spec.Name)
	}
	if got.Added.IsZero() {
		t.Error("Added was not stamped")
	}
	wasm, err := s.Blob(got.WasmDigest)
	if err != nil {
		t.Fatal(err)
	}
	if string(wasm) != "fake wasm" {
		t.Errorf("blob = %q", wasm)
	}
}

func TestIdenticalModulesShareOneBlob(t *testing.T) {
	s := open(t)
	a := put(t, s, "one", []byte("same bytes"))
	b := put(t, s, "two", []byte("same bytes"))
	if a.WasmDigest != b.WasmDigest {
		t.Fatal("identical bytes produced different digests")
	}

	var blobs int
	_ = filepath.WalkDir(filepath.Join(s.Dir(), "blobs"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Errorf("stored %d blobs for identical content, want 1", blobs)
	}
}

// TestBlobDetectsCorruption is cheap next to compiling a module, and turns a
// damaged store into a clear message instead of an obscure wasm validation
// failure.
func TestBlobDetectsCorruption(t *testing.T) {
	s := open(t)
	rec := put(t, s, "hello", []byte("original"))

	path := filepath.Join(s.Dir(), "blobs", "sha256", rec.WasmDigest[:2], rec.WasmDigest)
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Blob(rec.WasmDigest)
	if err == nil {
		t.Fatal("corrupt blob was accepted")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("err = %v, want it to say the module is corrupt", err)
	}
}

func TestPutRejectsARecordWithNoBlob(t *testing.T) {
	// A record pointing at a module that is not there would fail at the first
	// invocation instead of at the write that made it wrong.
	s := open(t)
	err := s.Put(Record{
		Spec:       core.Spec{Name: "ghost"},
		WasmDigest: strings.Repeat("ab", 32),
	})
	if err == nil {
		t.Fatal("accepted a record with no module")
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	s := open(t)
	_, err := s.Get("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if err := s.Remove("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove err = %v, want ErrNotFound", err)
	}
}

func TestListIsOrderedAndSurvivesACorruptRecord(t *testing.T) {
	s := open(t)
	put(t, s, "zebra", []byte("a"))
	put(t, s, "alpha", []byte("b"))
	put(t, s, "middle", []byte("c"))

	// One unreadable record must not make every other tool disappear.
	if err := os.WriteFile(filepath.Join(s.Dir(), "tools", "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Spec.Name)
	}
	if strings.Join(names, ",") != "alpha,middle,zebra" {
		t.Errorf("names = %v, want them ordered and complete", names)
	}
}

func TestLabelsMergeSpecAndUser(t *testing.T) {
	// User labels are kept apart from the tool's own so that reinstalling the
	// tool does not silently discard them.
	rec := Record{
		Spec:        core.Spec{Name: "x", Labels: []string{"git", "vcs"}},
		ExtraLabels: []string{"fast", "git"},
	}
	got := strings.Join(rec.Labels(), ",")
	if got != "fast,git,vcs" {
		t.Errorf("labels = %q, want them merged, deduped and sorted", got)
	}
}

func TestGCRemovesOnlyUnreferencedBlobs(t *testing.T) {
	s := open(t)
	keep := put(t, s, "keep", []byte("keep me"))
	drop := put(t, s, "drop", []byte("drop me"))

	if err := s.Remove("drop"); err != nil {
		t.Fatal(err)
	}
	removed, freed, err := s.GC()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d blobs, want 1", removed)
	}
	if freed != int64(len("drop me")) {
		t.Errorf("freed %d bytes, want %d", freed, len("drop me"))
	}
	if _, err := s.Blob(keep.WasmDigest); err != nil {
		t.Errorf("GC removed a blob still in use: %v", err)
	}
	if _, err := s.Blob(drop.WasmDigest); err == nil {
		t.Error("GC kept an unreferenced blob")
	}
}

func TestGCKeepsABlobTwoRecordsShare(t *testing.T) {
	s := open(t)
	put(t, s, "one", []byte("shared"))
	shared := put(t, s, "two", []byte("shared"))

	if err := s.Remove("one"); err != nil {
		t.Fatal(err)
	}
	if removed, _, err := s.GC(); err != nil || removed != 0 {
		t.Fatalf("GC removed %d blobs (err %v), want 0: the blob is still referenced", removed, err)
	}
	if _, err := s.Blob(shared.WasmDigest); err != nil {
		t.Errorf("GC removed a shared blob: %v", err)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	// A record is rewritten in place on every grant change, so a partial write
	// would lose an installed tool.
	s := open(t)
	put(t, s, "hello", []byte("v1"))
	for i := range 20 {
		put(t, s, "hello", []byte("v1"))
		if _, err := s.Get("hello"); err != nil {
			t.Fatalf("record unreadable after rewrite %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir(), "tools"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
