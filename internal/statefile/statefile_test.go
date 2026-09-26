package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOfMissingIsZero(t *testing.T) {
	// "Not there yet" is an ordinary state for a config forge has never
	// written, not an error.
	if s := Of(filepath.Join(t.TempDir(), "nope.json")); s.Exists() {
		t.Error("a missing file stamped as existing")
	}
}

func TestOfNoticesAContentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	write(t, path, "one")
	before := Of(path)

	write(t, path, "two")
	if !Changed(before, Of(path)) {
		t.Error("a rewrite was not noticed")
	}
}

// TestSameSecondLengthChangeIsNoticed is why size is in the stamp. Two writes
// can land inside one filesystem timestamp tick -- a grant and a revoke, or
// two tools installed in quick succession -- and modification time alone would
// call them identical.
func TestSameSecondLengthChangeIsNoticed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	write(t, path, "short")
	before := Of(path)

	// Force the timestamps equal, leaving size as the only difference.
	write(t, path, "considerably longer")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = fi
	if err := os.Chtimes(path, before.mod, before.mod); err != nil {
		t.Fatal(err)
	}

	if !Changed(before, Of(path)) {
		t.Error("a same-timestamp change of length was not noticed")
	}
}

func TestOfDirNoticesFilesComingAndGoing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.json"), "a")
	before := OfDir(dir, ".json")

	write(t, filepath.Join(dir, "b.json"), "b")
	added := OfDir(dir, ".json")
	if !Changed(before, added) {
		t.Error("a new file was not noticed")
	}

	if err := os.Remove(filepath.Join(dir, "b.json")); err != nil {
		t.Fatal(err)
	}
	if !Changed(added, OfDir(dir, ".json")) {
		t.Error("a removed file was not noticed")
	}
}

// TestOfDirNoticesARenameThatPreservesTotals is why the names are folded in.
// Removing one record and adding another of the same length leaves the count
// and the total size untouched.
func TestOfDirNoticesARenameThatPreservesTotals(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "aaa.json"), "same length")
	before := OfDir(dir, ".json")

	if err := os.Rename(filepath.Join(dir, "aaa.json"), filepath.Join(dir, "bbb.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "bbb.json"), before.mod, before.mod); err != nil {
		t.Fatal(err)
	}

	if !Changed(before, OfDir(dir, ".json")) {
		t.Error("a rename that preserved size and count was not noticed")
	}
}

// TestEmptyIsDistinctFromMissing: a store whose last tool was removed must not
// stamp the same as one that was never created, or the removal goes unseen.
func TestEmptyIsDistinctFromMissing(t *testing.T) {
	dir := t.TempDir()
	empty := OfDir(dir, ".json")
	missing := OfDir(filepath.Join(dir, "nope"), ".json")
	if !Changed(empty, missing) {
		t.Error("an empty directory stamped the same as a missing one")
	}
}

func TestOfDirIgnoresOtherSuffixesAndSubdirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.json"), "a")
	before := OfDir(dir, ".json")

	write(t, filepath.Join(dir, "notes.txt"), "irrelevant")
	write(t, filepath.Join(dir, "sub", "b.json"), "nested")
	if Changed(before, OfDir(dir, ".json")) {
		t.Error("an unrelated file or a nested one changed the stamp")
	}
}

func TestStampIsStableWhenNothingChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.json"), "a")
	if Changed(OfDir(dir, ".json"), OfDir(dir, ".json")) {
		t.Error("two stamps of an unchanged directory differ")
	}
}
