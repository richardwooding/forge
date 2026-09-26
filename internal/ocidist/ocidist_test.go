package ocidist

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/richardwooding/forge/internal/bundle"
	"github.com/richardwooding/forge/internal/core"
)

func makeBundle(t *testing.T, tools map[string]string) *bundle.Reader {
	t.Helper()
	var buf bytes.Buffer
	w, err := bundle.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedNames(tools) {
		e := bundle.Entry{Spec: core.Spec{
			Name: name, ABI: core.ABICurrent, Labels: []string{"demo"},
		}}
		if err := w.Add(e, []byte(tools[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := bundle.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sortedNames(m map[string]string) []string {
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

func TestRoundTripThroughAnArtifact(t *testing.T) {
	br := makeBundle(t, map[string]string{"alpha": "module alpha", "beta": "module beta"})

	img, err := Build(br)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ToBundle(img)
	if err != nil {
		t.Fatal(err)
	}
	back, err := bundle.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Index.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(back.Index.Tools))
	}
	for _, e := range back.Index.Tools {
		blob, ok := back.Blob(e.WasmDigest)
		if !ok {
			t.Fatalf("no module for %s", e.Spec.Name)
		}
		if !strings.Contains(string(blob), e.Spec.Name) {
			t.Errorf("%s got the wrong module", e.Spec.Name)
		}
	}
}

// TestTheMediaTypesSayWhatItIs. A registry UI and a person running
// `crane manifest` both get an honest answer, rather than seeing something
// mislabelled as a container image that would fail confusingly if run.
func TestTheMediaTypesSayWhatItIs(t *testing.T) {
	img, err := Build(makeBundle(t, map[string]string{"alpha": "m"}))
	if err != nil {
		t.Fatal(err)
	}

	mt, err := img.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	if mt != types.OCIManifestSchema1 {
		t.Errorf("manifest media type = %s", mt)
	}

	manifest, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Config.MediaType != ConfigMediaType {
		t.Errorf("config media type = %s, want %s", manifest.Config.MediaType, ConfigMediaType)
	}

	var sawIndex, sawTool bool
	for _, l := range manifest.Layers {
		switch l.MediaType {
		case IndexMediaType:
			sawIndex = true
		case LayerMediaType:
			sawTool = true
			if l.Annotations["org.opencontainers.image.title"] == "" {
				t.Error("a module layer has no title, so a registry listing shows a bare digest")
			}
		}
	}
	if !sawIndex || !sawTool {
		t.Errorf("layers = %+v, want an index and a module", manifest.Layers)
	}
}

// TestTheIndexIsTheFirstLayer, so a reader knows what it is holding before it
// reaches the modules.
func TestTheIndexIsTheFirstLayer(t *testing.T) {
	img, err := Build(makeBundle(t, map[string]string{"alpha": "m", "beta": "n"}))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Layers[0].MediaType != IndexMediaType {
		t.Errorf("first layer is %s, want the index", manifest.Layers[0].MediaType)
	}
}

func TestAnnotationsDescribeTheContents(t *testing.T) {
	img, err := Build(makeBundle(t, map[string]string{"alpha": "m", "beta": "n"}))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Annotations[AnnotationTools]; got != "alpha,beta" {
		t.Errorf("%s = %q", AnnotationTools, got)
	}
	if got := manifest.Annotations[AnnotationCount]; got != "2" {
		t.Errorf("%s = %q", AnnotationCount, got)
	}
}

// TestPushingTheSameBundleTwiceIsIdentical: the registry then stores one copy,
// and two pushes can be compared.
func TestPushingTheSameBundleTwiceIsIdentical(t *testing.T) {
	tools := map[string]string{"alpha": "m", "beta": "n"}

	first, err := Build(makeBundle(t, tools))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(makeBundle(t, tools))
	if err != nil {
		t.Fatal(err)
	}
	d1, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("two builds of the same bundle differ: %s vs %s", d1, d2)
	}
}

func TestIdenticalModulesBecomeOneLayer(t *testing.T) {
	img, err := Build(makeBundle(t, map[string]string{"one": "same", "two": "same"}))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var modules int
	for _, l := range manifest.Layers {
		if l.MediaType == LayerMediaType {
			modules++
		}
	}
	if modules != 1 {
		t.Errorf("%d module layers for identical content, want 1", modules)
	}
	// ...and both tools still name it.
	if got := manifest.Layers[1].Annotations["org.opencontainers.image.title"]; got != "one,two" {
		t.Errorf("shared layer title = %q, want both tools named", got)
	}
}

// TestSomethingThatIsNotAForgeArtifactIsRefused, with a message that says so
// rather than a parse failure from halfway through.
func TestSomethingThatIsNotAForgeArtifactIsRefused(t *testing.T) {
	notForge, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:     static.NewLayer([]byte("a tarball, probably"), types.OCILayer),
		MediaType: types.OCILayer,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = Extract(notForge)
	if err == nil {
		t.Fatal("a container image was accepted as a forge bundle")
	}
	if !strings.Contains(err.Error(), "not a forge bundle") {
		t.Errorf("err = %v", err)
	}
}

func TestAnEmptyArtifactIsRefused(t *testing.T) {
	if _, _, err := Extract(empty.Image); err == nil {
		t.Error("an empty artifact was accepted")
	}
}

func TestAnIndexNamingAMissingModuleIsRefused(t *testing.T) {
	// The layer digests are the registry's own concern; this is what checks
	// the modules are the ones the index names.
	br := makeBundle(t, map[string]string{"alpha": "m"})
	br.Index.Tools[0].WasmDigest = strings.Repeat("ab", 32)

	img, err := Build(br)
	if err == nil {
		if _, _, err = Extract(img); err == nil {
			t.Fatal("an index naming a missing module was accepted")
		}
		return
	}
	// Build may refuse it first, which is equally acceptable.
}
