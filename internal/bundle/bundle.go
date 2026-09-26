// Package bundle moves installed tools between machines.
//
// A bundle is a zstd-compressed tar holding an index and the wasm modules it
// names. The blobs are content addressed, so the archive verifies itself:
// there is no separate checksum to keep in step, and a truncated or tampered
// bundle fails at the blob that does not hash to its own name.
//
// What a bundle deliberately does NOT carry is grants. What a tool is allowed
// to do is the receiving user's decision, made on their machine about their
// data; a bundle that could arrive pre-authorised would let whoever built it
// decide on their behalf. An imported tool asks on first use, exactly as a
// locally built one does.
package bundle

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/richardwooding/forge/internal/build"
	"github.com/richardwooding/forge/internal/core"
)

// FormatVersion is the bundle layout this package writes.
//
// Checked on import so that a future format fails with a message naming the
// version rather than with a parse error from halfway through a tar.
const FormatVersion = 1

// IndexName is the index's path inside the archive. It is written first so a
// reader can learn what it is holding before streaming the blobs.
const IndexName = "forge-bundle.json"

// MaxIndexBytes caps the index. A bundle is attacker-controlled input the
// moment it comes from anyone else.
const MaxIndexBytes = 8 << 20

// MaxBlobBytes caps one module.
const MaxBlobBytes = 256 << 20

// Entry is one tool in a bundle.
type Entry struct {
	Spec core.Spec `json:"spec"`

	// WasmDigest names the blob, as sha256 hex.
	WasmDigest string `json:"wasmDigest"`

	// ExtraLabels are the labels the exporting user added by hand. Carried
	// because they are part of how that person organised their tools, and
	// losing them on every transfer would make labelling not worth doing.
	ExtraLabels []string `json:"extraLabels,omitempty"`

	Build build.Provenance `json:"build,omitzero"`
}

// Index is a bundle's table of contents.
//
// No creation timestamp: an export must be reproducible, and a clock reading
// would make two exports of the same tools differ. Provenance lives on each
// entry's Build, where it describes something that actually happened.
type Index struct {
	FormatVersion int     `json:"formatVersion"`
	Tools         []Entry `json:"tools"`
}

// Blobs is where a bundle keeps modules, by digest.
func blobPath(digest string) string { return "blobs/sha256/" + digest }

// ErrNotBundle is returned for a file that is not a forge bundle.
var ErrNotBundle = errors.New("not a forge bundle")

// Writer builds a bundle.
type Writer struct {
	zw   *zstd.Encoder
	tw   *tar.Writer
	seen map[string]bool
	idx  Index
	out  io.Writer
}

// NewWriter starts a bundle.
func NewWriter(w io.Writer) (*Writer, error) {
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return nil, err
	}
	return &Writer{
		zw:   zw,
		tw:   tar.NewWriter(zw),
		seen: map[string]bool{},
		idx:  Index{FormatVersion: FormatVersion},
		out:  w,
	}, nil
}

// Add records a tool and its module. Adding the same module twice stores one
// copy, which is what content addressing is for: several tools built from one
// source share a blob.
func (w *Writer) Add(e Entry, wasm []byte) error {
	sum := sha256.Sum256(wasm)
	digest := hex.EncodeToString(sum[:])
	if e.WasmDigest == "" {
		e.WasmDigest = digest
	}
	if e.WasmDigest != digest {
		return fmt.Errorf("tool %q: the module does not match its recorded digest", e.Spec.Name)
	}
	w.idx.Tools = append(w.idx.Tools, e)

	if w.seen[digest] {
		return nil
	}
	w.seen[digest] = true
	return writeFile(w.tw, blobPath(digest), wasm)
}

// Close finishes the bundle.
//
// The index is written last in the tar but the entries are sorted by name
// first, so that exporting the same tools twice produces the same bytes. A
// bundle you can compare is a bundle you can check was not altered in transit
// without needing a signature to tell you.
func (w *Writer) Close() error {
	sort.Slice(w.idx.Tools, func(i, j int) bool {
		return w.idx.Tools[i].Spec.Name < w.idx.Tools[j].Spec.Name
	})
	raw, err := json.MarshalIndent(w.idx, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(w.tw, IndexName, append(raw, '\n')); err != nil {
		return err
	}
	if err := w.tw.Close(); err != nil {
		return err
	}
	return w.zw.Close()
}

// writeFile adds one entry with fixed metadata.
//
// Zeroed times, fixed ownership and a constant mode: none of it is meaningful
// on the other machine, and all of it would otherwise make two exports of the
// same tools differ.
func writeFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(data)),
		Mode:     0o644,
		Format:   tar.FormatPAX,
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// Reader reads a bundle.
type Reader struct {
	Index Index
	blobs map[string][]byte
}

// Read loads a whole bundle into memory, verifying every blob against the
// digest that names it.
//
// Whole, rather than streamed: the index is written last, so a streaming
// reader would have to hold the blobs anyway to know which of them it wanted.
// The size caps are what keep that honest.
func Read(r io.Reader) (*Reader, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotBundle, err)
	}
	defer zr.Close()

	out := &Reader{blobs: map[string][]byte{}}
	tr := tar.NewReader(zr)
	var haveIndex bool

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotBundle, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}

		switch {
		case h.Name == IndexName:
			if h.Size > MaxIndexBytes {
				return nil, fmt.Errorf("%w: index is %d bytes", ErrNotBundle, h.Size)
			}
			raw, err := io.ReadAll(io.LimitReader(tr, MaxIndexBytes))
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(raw, &out.Index); err != nil {
				return nil, fmt.Errorf("%w: unreadable index: %v", ErrNotBundle, err)
			}
			haveIndex = true

		case strings.HasPrefix(h.Name, "blobs/sha256/"):
			if h.Size > MaxBlobBytes {
				return nil, fmt.Errorf("%w: a module is %d bytes", ErrNotBundle, h.Size)
			}
			data, err := io.ReadAll(io.LimitReader(tr, MaxBlobBytes))
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(data)
			got := hex.EncodeToString(sum[:])
			want := strings.TrimPrefix(h.Name, "blobs/sha256/")
			if got != want {
				// The archive names its own contents by hash, so this is the
				// integrity check: no separate manifest to keep in step, and
				// nowhere for a tampered module to hide.
				return nil, fmt.Errorf("%w: module %s hashes to %s", ErrNotBundle, want, got)
			}
			out.blobs[got] = data

		default:
			// An unexpected path is ignored rather than fatal, so a future
			// format that adds files stays readable by an older forge for the
			// parts it does understand.
			continue
		}
	}

	if !haveIndex {
		return nil, fmt.Errorf("%w: no %s", ErrNotBundle, IndexName)
	}
	if out.Index.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("bundle is format version %d; this forge reads version %d",
			out.Index.FormatVersion, FormatVersion)
	}
	for _, e := range out.Index.Tools {
		if _, ok := out.blobs[e.WasmDigest]; !ok {
			return nil, fmt.Errorf("%w: the index names tool %q whose module is missing",
				ErrNotBundle, e.Spec.Name)
		}
	}
	return out, nil
}

// Blob returns a module by digest.
func (r *Reader) Blob(digest string) ([]byte, bool) {
	b, ok := r.blobs[digest]
	return b, ok
}
