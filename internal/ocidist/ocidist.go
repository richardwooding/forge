// Package ocidist pushes and pulls tool bundles through an OCI registry.
//
// A bundle becomes an ordinary OCI artifact: the index is the config blob and
// each tool's module is a layer. Any registry that speaks OCI will store and
// serve it -- ghcr, a company's Artifactory, a registry running on a laptop --
// without knowing anything about forge, which is the point of using the format
// rather than inventing a protocol.
//
// The media types say what the thing is, so a registry UI and a human reading
// `crane manifest` both get an honest answer instead of seeing something
// mislabelled as a container image that would fail confusingly if anyone tried
// to run it.
package ocidist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/richardwooding/forge/internal/bundle"
)

// Media types for a forge artifact.
const IndexName = bundle.IndexName

const (
	// ArtifactType identifies the whole thing.
	ArtifactType = "application/vnd.forge.bundle.v1+json"

	// ConfigMediaType is the index blob.
	ConfigMediaType = types.MediaType(ArtifactType)

	// LayerMediaType is one tool's WebAssembly module.
	LayerMediaType = types.MediaType("application/vnd.forge.tool.v1+wasm")

	// IndexMediaType is the bundle index, carried as its own layer.
	//
	// Not as the config blob, which is where an artifact's metadata would
	// naturally go: go-containerregistry's Image requires the config to parse
	// as an OCI image config, and stuffing the index into one of its label
	// fields both fought the abstraction and silently dropped the layer
	// diff_ids. A typed layer is what the format already has for "a blob that
	// is not a filesystem", and a registry UI shows it honestly.
	IndexMediaType = types.MediaType("application/vnd.forge.index.v1+json")
)

// Annotation keys forge sets on the manifest, so a registry listing says what
// is inside without anyone pulling it.
const (
	AnnotationTools  = "dev.forge.tools"
	AnnotationLabels = "dev.forge.labels"
	AnnotationCount  = "dev.forge.tool-count"
)

// Build turns a bundle into an OCI artifact.
func Build(r *bundle.Reader) (v1.Image, error) {
	index, err := json.MarshalIndent(r.Index, "", "  ")
	if err != nil {
		return nil, err
	}

	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, ConfigMediaType)

	// The index first, so a reader knows what it is holding before it reaches
	// the modules, and so the layer order is fixed.
	adds := []mutate.Addendum{{
		Layer:     static.NewLayer(index, IndexMediaType),
		MediaType: IndexMediaType,
		Annotations: map[string]string{
			"org.opencontainers.image.title": IndexName,
		},
	}}

	// One layer per distinct module, in digest order so that pushing the same
	// bundle twice produces the same manifest and the registry stores one copy.
	seen := map[string]bool{}
	var digests []string
	for _, e := range r.Index.Tools {
		if !seen[e.WasmDigest] {
			seen[e.WasmDigest] = true
			digests = append(digests, e.WasmDigest)
		}
	}
	sort.Strings(digests)

	names := map[string][]string{}
	for _, e := range r.Index.Tools {
		names[e.WasmDigest] = append(names[e.WasmDigest], e.Spec.Name)
	}

	for _, d := range digests {
		blob, ok := r.Blob(d)
		if !ok {
			return nil, fmt.Errorf("bundle is missing module %s", d)
		}
		sort.Strings(names[d])
		adds = append(adds, mutate.Addendum{
			Layer:     static.NewLayer(blob, LayerMediaType),
			MediaType: LayerMediaType,
			Annotations: map[string]string{
				// The conventional key, so a registry UI shows something
				// meaningful rather than a bare digest.
				"org.opencontainers.image.title": strings.Join(names[d], ","),
			},
		})
	}

	img, err = mutate.Append(img, adds...)
	if err != nil {
		return nil, err
	}

	return withAnnotations(img, r), nil
}

func withAnnotations(img v1.Image, r *bundle.Reader) v1.Image {
	var tools []string
	labelSet := map[string]bool{}
	for _, e := range r.Index.Tools {
		tools = append(tools, e.Spec.Name)
		for _, l := range e.Spec.Labels {
			labelSet[l] = true
		}
		for _, l := range e.ExtraLabels {
			labelSet[l] = true
		}
	}
	sort.Strings(tools)

	labels := make([]string, 0, len(labelSet))
	for l := range labelSet {
		labels = append(labels, l)
	}
	sort.Strings(labels)

	anns := map[string]string{
		AnnotationTools: strings.Join(tools, ","),
		AnnotationCount: fmt.Sprint(len(tools)),
		"org.opencontainers.image.description": fmt.Sprintf(
			"forge tools: %s", strings.Join(tools, ", ")),
	}
	if len(labels) > 0 {
		anns[AnnotationLabels] = strings.Join(labels, ",")
	}
	out, ok := mutate.Annotations(img, anns).(v1.Image)
	if !ok {
		return img
	}
	return out
}

// Extract reads a forge artifact back into bundle form.
func Extract(img v1.Image) (*bundle.Index, map[string][]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, err
	}

	var (
		index  *bundle.Index
		blobs  = map[string][]byte{}
		hadAny bool
	)
	for _, l := range layers {
		mt, err := l.MediaType()
		if err != nil {
			return nil, nil, err
		}
		data, err := readLayer(l)
		if err != nil {
			return nil, nil, err
		}
		hadAny = true

		switch mt {
		case IndexMediaType:
			var idx bundle.Index
			if err := json.Unmarshal(data, &idx); err != nil {
				return nil, nil, fmt.Errorf("unreadable index: %w", err)
			}
			index = &idx
		case LayerMediaType:
			blobs[sha256Hex(data)] = data
		default:
			// Something forge did not write. Ignored rather than fatal, so an
			// artifact that gains a layer later stays readable for the parts
			// this forge does understand.
			continue
		}
	}

	if index == nil {
		if !hadAny {
			return nil, nil, fmt.Errorf("this is not a forge bundle: the artifact has no layers")
		}
		return nil, nil, fmt.Errorf("this is not a forge bundle: no %s layer", IndexMediaType)
	}
	if index.FormatVersion != bundle.FormatVersion {
		return nil, nil, fmt.Errorf("bundle is format version %d; this forge reads version %d",
			index.FormatVersion, bundle.FormatVersion)
	}

	for _, e := range index.Tools {
		if _, ok := blobs[e.WasmDigest]; !ok {
			// The layer digests are the registry's own; this checks the
			// modules are the ones the index names, which the registry has no
			// opinion about.
			return nil, nil, fmt.Errorf("the artifact is missing the module for %q", e.Spec.Name)
		}
	}
	return index, blobs, nil
}

// Options configure a registry call.
type Options struct {
	// Insecure allows plain HTTP, for a registry running locally.
	Insecure bool
}

func (o Options) remote() []remote.Option {
	return []remote.Option{remote.WithAuthFromKeychain(keychain())}
}

func (o Options) name() []name.Option {
	if o.Insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// Push writes an artifact to a registry.
func Push(ref string, img v1.Image, opts Options) (string, error) {
	r, err := name.ParseReference(ref, opts.name()...)
	if err != nil {
		return "", fmt.Errorf("%q is not a registry reference: %w", ref, err)
	}
	if err := remote.Write(r, img, opts.remote()...); err != nil {
		return "", err
	}
	d, err := img.Digest()
	if err != nil {
		// The push succeeded; only the digest for the confirmation line is
		// missing, and failing here would tell the user nothing was published
		// when it was.
		return r.String(), nil //nolint:nilerr // the push already succeeded
	}
	return r.Context().Name() + "@" + d.String(), nil
}

// Pull fetches an artifact from a registry.
func Pull(ref string, opts Options) (v1.Image, error) {
	r, err := name.ParseReference(ref, opts.name()...)
	if err != nil {
		return nil, fmt.Errorf("%q is not a registry reference: %w", ref, err)
	}
	return remote.Image(r, opts.remote()...)
}

// keychain finds registry credentials.
//
// go-containerregistry looks for Docker's config directory. podman writes
// somewhere else entirely, so on a machine with only podman installed the
// default keychain finds nothing and every private pull fails as unauthorized
// with no hint as to why. When podman's auth file exists and Docker's does
// not, DOCKER_CONFIG is pointed at a directory holding it under the name
// go-containerregistry expects, so that `podman login` is enough.
func keychain() authn.Keychain {
	if os.Getenv("DOCKER_CONFIG") != "" {
		return authn.DefaultKeychain
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".docker", "config.json")); err == nil {
			return authn.DefaultKeychain
		}
	}

	auth := podmanAuthFile()
	if auth == "" {
		return authn.DefaultKeychain
	}
	dir, err := os.MkdirTemp("", "forge-registry-auth-")
	if err != nil {
		return authn.DefaultKeychain
	}
	data, err := os.ReadFile(auth)
	if err != nil {
		return authn.DefaultKeychain
	}
	// 0600, because this is a copy of the credentials.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		return authn.DefaultKeychain
	}
	_ = os.Setenv("DOCKER_CONFIG", dir)
	return authn.DefaultKeychain
}

// podmanAuthFile is where podman keeps credentials, or "" if there are none.
func podmanAuthFile() string {
	if p := os.Getenv("REGISTRY_AUTH_FILE"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		p := filepath.Join(run, "containers", "auth.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ToBundle turns a pulled artifact back into bundle bytes.
//
// Rather than installing from the artifact directly: a registry and a file
// then take exactly the same route into the store, through the same
// revalidation and the same conflict handling, and cannot diverge in what they
// check.
func ToBundle(img v1.Image) ([]byte, error) {
	index, blobs, err := Extract(img)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	w, err := bundle.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	for _, e := range index.Tools {
		blob, ok := blobs[e.WasmDigest]
		if !ok {
			return nil, fmt.Errorf("the artifact is missing the module for %q", e.Spec.Name)
		}
		if err := w.Add(e, blob); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
