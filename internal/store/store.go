// Package store keeps installed tools on disk.
//
// Layout under the store directory:
//
//	blobs/sha256/<ab>/<digest>   the wasm modules, content addressed
//	tools/<name>.json            one record per installed tool
//
// Content addressing means two tools built from the same source share a blob,
// and that a record can be verified against what it claims to point at. Records
// are small JSON files rather than a database because they are worth being able
// to read, diff and delete by hand when something has gone wrong.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/richardwooding/forge/internal/build"
	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/statefile"
)

// ErrNotFound is returned for a tool the store does not hold.
var ErrNotFound = errors.New("tool not found")

// Record is everything the store knows about one installed tool.
type Record struct {
	Spec core.Spec `json:"spec"`

	// WasmDigest identifies the blob, as sha256 hex.
	WasmDigest string `json:"wasmDigest"`

	// ExtraLabels are labels the user added after installing, kept apart from
	// the tool's own so that reinstalling it does not discard them.
	ExtraLabels []string `json:"extraLabels,omitempty"`

	Build  build.Provenance `json:"build,omitzero"`
	Source string           `json:"source,omitempty"`
	Added  time.Time        `json:"added"`
}

// Labels is the tool's own labels plus the user's, deduplicated and sorted.
func (r Record) Labels() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range slices.Concat(r.Spec.Labels, r.ExtraLabels) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// Store is a directory of installed tools.
type Store struct{ dir string }

// Open prepares a store directory, creating it if necessary.
func Open(dir string) (*Store, error) {
	for _, sub := range []string{"blobs", "tools"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("preparing the store: %w", err)
		}
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's root.
func (s *Store) Dir() string { return s.dir }

// Stamp summarises the installed set cheaply, so a caller can tell that
// something changed without reading every record.
//
// The store itself never caches -- List and Get read from disk every time --
// so this is not about the store being stale. It is for the surfaces built
// from it: an MCP server holds a set of registered tools that only changes
// when something tells it to, and this is what tells it.
func (s *Store) Stamp() statefile.Stamp {
	return statefile.OfDir(filepath.Join(s.dir, "tools"), ".json")
}

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.dir, "blobs", "sha256", digest[:2], digest)
}

func (s *Store) recordPath(name string) string {
	return filepath.Join(s.dir, "tools", name+".json")
}

// PutBlob stores a wasm module and returns its digest.
func (s *Store) PutBlob(wasm []byte) (string, error) {
	sum := sha256.Sum256(wasm)
	digest := hex.EncodeToString(sum[:])
	path := s.blobPath(digest)

	if _, err := os.Stat(path); err == nil {
		// Content addressed, so identical bytes are already correct.
		return digest, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeAtomic(path, wasm, 0o600); err != nil {
		return "", err
	}
	return digest, nil
}

// Blob reads a module by digest, checking it still hashes to its name.
//
// The check is cheap next to compiling the module, and it turns a corrupted or
// tampered store into a clear error instead of a puzzling wasm validation
// failure.
func (s *Store) Blob(digest string) ([]byte, error) {
	b, err := os.ReadFile(s.blobPath(digest))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no module with digest %s", ErrNotFound, digest)
		}
		return nil, err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("module %s is corrupt: it hashes to %s", digest, got)
	}
	return b, nil
}

// Put writes a tool record. The blob must already be stored.
func (s *Store) Put(rec Record) error {
	if rec.Spec.Name == "" {
		return errors.New("record has no tool name")
	}
	if rec.WasmDigest == "" {
		return fmt.Errorf("record for %q has no module digest", rec.Spec.Name)
	}
	if _, err := os.Stat(s.blobPath(rec.WasmDigest)); err != nil {
		return fmt.Errorf("record for %q points at a module that is not stored: %w", rec.Spec.Name, err)
	}
	if rec.Added.IsZero() {
		rec.Added = time.Now().UTC()
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.recordPath(rec.Spec.Name), append(b, '\n'), 0o600)
}

// Get reads one tool record.
func (s *Store) Get(name string) (Record, error) {
	b, err := os.ReadFile(s.recordPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("record for %q is unreadable: %w", name, err)
	}
	return rec, nil
}

// List returns every record, ordered by name.
//
// A record that cannot be read is skipped rather than failing the whole
// listing: one corrupt file must not make every other tool disappear.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "tools"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := s.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}

// Remove deletes a tool record. The blob is left for [GC], since another record
// may share it.
func (s *Store) Remove(name string) error {
	err := os.Remove(s.recordPath(name))
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return err
}

// GC deletes blobs no record points at, returning how many went and how much
// space they took.
func (s *Store) GC() (removed int, freed int64, err error) {
	records, err := s.List()
	if err != nil {
		return 0, 0, err
	}
	live := make(map[string]bool, len(records))
	for _, r := range records {
		live[r.WasmDigest] = true
	}

	root := filepath.Join(s.dir, "blobs", "sha256")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || live[d.Name()] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A blob that cannot be stat'd cannot be accounted for; leaving it
			// is safe, and failing the whole sweep for one unreadable file
			// would mean no space was ever reclaimed.
			return nil //nolint:nilerr // GC is best effort by design
		}
		if err := os.Remove(path); err != nil {
			// Another process may hold it, or the store may be read-only.
			// The next sweep will try again.
			return nil //nolint:nilerr // GC is best effort by design
		}
		removed++
		freed += info.Size()
		return nil
	})
	return removed, freed, err
}

// writeAtomic writes through a temporary file in the same directory, so a
// crash or a full disk leaves either the old contents or the new, never a
// half-written record.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	// Flush before the rename: on a crash the rename could otherwise land
	// while the contents are still in the page cache, leaving an empty file
	// where a valid one used to be.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
