package toolkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/richardwooding/forge/internal/bundle"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/manifest"
	"github.com/richardwooding/forge/internal/store"
)

// Export writes the tools matching sel as a bundle.
func (tk *Toolkit) Export(w io.Writer, sel labels.Selector) (int, error) {
	records, err := tk.List(sel)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, errors.New("nothing to export: no tools match")
	}

	bw, err := bundle.NewWriter(w)
	if err != nil {
		return 0, err
	}
	for _, rec := range records {
		wasm, err := tk.store.Blob(rec.WasmDigest)
		if err != nil {
			return 0, fmt.Errorf("exporting %s: %w", rec.Spec.Name, err)
		}
		// Grants are deliberately absent: see the bundle package. What a tool
		// may do is decided on the machine it runs on.
		if err := bw.Add(bundle.Entry{
			Spec:        rec.Spec,
			WasmDigest:  rec.WasmDigest,
			ExtraLabels: rec.ExtraLabels,
			Build:       rec.Build,
		}, wasm); err != nil {
			return 0, err
		}
	}
	if err := bw.Close(); err != nil {
		return 0, err
	}
	return len(records), nil
}

// OnConflict decides what an import does about a name already in use.
type OnConflict string

const (
	// ConflictSkip leaves the installed tool alone. The default, because an
	// import should not silently replace something that is working.
	ConflictSkip OnConflict = "skip"
	// ConflictReplace overwrites it.
	ConflictReplace OnConflict = "replace"
	// ConflictRename installs alongside under a suffixed name.
	ConflictRename OnConflict = "rename"
	// ConflictFail stops at the first collision without installing anything.
	ConflictFail OnConflict = "fail"
)

// Valid reports whether the policy is one forge knows.
func (c OnConflict) Valid() bool {
	switch c {
	case ConflictSkip, ConflictReplace, ConflictRename, ConflictFail:
		return true
	}
	return false
}

// ImportResult reports what an import did.
type ImportResult struct {
	Installed []string
	Replaced  []string
	Renamed   map[string]string
	Skipped   []string
}

// Import installs the tools in a bundle.
//
// Every manifest is re-validated rather than trusted. The index was written by
// another machine and may have been written by another version of forge, or by
// something that is not forge at all; a bundle is ordinary untrusted input.
//
// Grants are not imported and are not preserved for a replaced tool either: a
// module arriving from elsewhere is a different module, whatever it calls
// itself, so the question of what it may do is asked again.
func (tk *Toolkit) Import(ctx context.Context, r io.Reader, policy OnConflict) (*ImportResult, error) {
	if policy == "" {
		policy = ConflictSkip
	}
	if !policy.Valid() {
		return nil, fmt.Errorf("unknown conflict policy %q", policy)
	}

	br, err := bundle.Read(r)
	if err != nil {
		return nil, err
	}

	// Validate everything before installing anything, so a bundle with one bad
	// tool does not leave half of itself behind.
	type pending struct {
		entry bundle.Entry
		name  string
		wasm  []byte
		// existed records whether the name was already taken, which is what
		// distinguishes a replacement from a plain install. Tracked separately
		// from skip, because "replace" on a name nobody was using is an
		// install, not a replacement.
		existed bool
		skip    bool
	}
	var plan []pending

	for _, e := range br.Index.Tools {
		if _, err := manifest.Validate(e.Spec); err != nil {
			return nil, fmt.Errorf("bundle contains an invalid tool %q: %w", e.Spec.Name, err)
		}
		wasm, ok := br.Blob(e.WasmDigest)
		if !ok {
			return nil, fmt.Errorf("bundle is missing the module for %q", e.Spec.Name)
		}
		// Compile it here, so a module that cannot run is refused at import
		// rather than at the first call, when whoever imported it has moved on.
		if _, err := tk.engine.Compile(ctx, wasm); err != nil {
			return nil, fmt.Errorf("tool %q does not load: %w", e.Spec.Name, err)
		}

		name := e.Spec.Name
		_, getErr := tk.store.Get(name)
		exists := getErr == nil

		if exists {
			switch policy {
			case ConflictFail:
				return nil, fmt.Errorf("%q is already installed; choose --on-conflict skip, replace or rename", name)
			case ConflictSkip:
				plan = append(plan, pending{entry: e, name: name, wasm: wasm, existed: true, skip: true})
				continue
			case ConflictRename:
				name, err = tk.freeName(e.Spec.Name)
				if err != nil {
					return nil, err
				}
			}
		}
		plan = append(plan, pending{entry: e, name: name, wasm: wasm, existed: exists})
	}

	res := &ImportResult{Renamed: map[string]string{}}
	for _, p := range plan {
		if p.skip {
			res.Skipped = append(res.Skipped, p.entry.Spec.Name)
			continue
		}

		digest, err := tk.store.PutBlob(p.wasm)
		if err != nil {
			return nil, err
		}
		spec := p.entry.Spec
		spec.Name = p.name

		rec := store.Record{
			Spec:        spec,
			WasmDigest:  digest,
			ExtraLabels: p.entry.ExtraLabels,
			Build:       p.entry.Build,
			Source:      "imported",
			Added:       time.Now().UTC(),
		}
		if err := tk.store.Put(rec); err != nil {
			return nil, err
		}
		tk.invalidate(p.name)

		switch {
		case p.name != p.entry.Spec.Name:
			res.Renamed[p.entry.Spec.Name] = p.name
		case p.existed:
			res.Replaced = append(res.Replaced, p.name)
		default:
			res.Installed = append(res.Installed, p.name)
		}
	}
	return res, nil
}

// freeName finds an unused name for a renamed import.
func (tk *Toolkit) freeName(base string) (string, error) {
	for i := 2; i < 100; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		// Not-found is the answer being looked for here, so the error is the
		// success condition rather than a failure.
		if _, err := tk.store.Get(candidate); errors.Is(err, store.ErrNotFound) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot find a free name for %q", base)
}
