package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/core"
)

func objectSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string"},
		},
	}
}

func goodSpec() core.Spec {
	return core.Spec{
		Name:    "jsonfmt",
		Summary: "Pretty-print JSON",
		Labels:  []string{"text", "json"},
		ABI:     core.ABICurrent,
		Ops: []core.OpSpec{{
			Name:       "format",
			Input:      objectSchema(),
			OutputKind: core.OutputText,
		}},
	}
}

func TestValidateAcceptsAGoodSpec(t *testing.T) {
	l, err := Validate(goodSpec())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if l.Resolved["format"] == nil {
		t.Error("no resolved schema for the operation")
	}
	if len(l.CanonJSON["format"]) == 0 {
		t.Error("no canonical schema bytes")
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*core.Spec)
		wantSub string
	}{
		{"no ABI", func(s *core.Spec) { s.ABI = 0 }, "did not report an ABI"},
		{"wrong ABI", func(s *core.Spec) { s.ABI = 99 }, "this forge speaks"},
		{"bad tool name", func(s *core.Spec) { s.Name = "JSON Fmt" }, "name"},
		{"empty name", func(s *core.Spec) { s.Name = "" }, "name"},
		{"no ops", func(s *core.Spec) { s.Ops = nil }, "init(), not main()"},
		{"duplicate op", func(s *core.Spec) { s.Ops = append(s.Ops, s.Ops[0]) }, "declared twice"},
		{"bad op name", func(s *core.Spec) { s.Ops[0].Name = "Format!" }, "operation name"},
		{"nil input schema", func(s *core.Spec) { s.Ops[0].Input = nil }, "missing"},
		{
			"non-object input",
			func(s *core.Spec) { s.Ops[0].Input = &jsonschema.Schema{Type: "string"} },
			`type "object"`,
		},
		{
			"bytes without media type",
			func(s *core.Spec) { s.Ops[0].OutputKind = core.OutputBytes },
			"requires outputMediaType",
		},
		{"unknown output kind", func(s *core.Spec) { s.Ops[0].OutputKind = "video" }, "unknown outputKind"},
		{
			"unknown capability",
			func(s *core.Spec) {
				s.Requires = []capability.Request{{Kind: "fs.reed", Scope: []string{"/tmp"}}}
			},
			"unknown capability",
		},
		{
			"capability with no scope",
			func(s *core.Spec) {
				s.Requires = []capability.Request{{Kind: capability.FSRead}}
			},
			"no scope",
		},
		{"duplicate label", func(s *core.Spec) { s.Labels = []string{"json", "json"} }, "appears twice"},
		{"bad label", func(s *core.Spec) { s.Labels = []string{"Not A Label"} }, "labels"},
		{"long summary", func(s *core.Spec) { s.Summary = strings.Repeat("x", MaxSummaryLen+1) }, "summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := goodSpec()
			tt.mutate(&spec)
			_, err := Validate(spec)
			if err == nil {
				t.Fatal("accepted a spec it should have refused")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

// TestEmptyOpsBlamesTheRightThing guards the message, not just the rejection.
// In a reactor build main() never runs, so registering a tool from there is the
// overwhelmingly likely cause of an empty manifest, and a bare "no operations"
// would send the author looking in the wrong place.
func TestEmptyOpsBlamesTheRightThing(t *testing.T) {
	spec := goodSpec()
	spec.Ops = nil
	_, err := Validate(spec)
	if err == nil || !strings.Contains(err.Error(), "init(), not main()") {
		t.Errorf("got %v, want advice about init() vs main()", err)
	}
}

func TestRefsAreRejectedAnywhereInTheSchema(t *testing.T) {
	// $ref is refused because ApplyDefaults does not follow references (see
	// TestApplyDefaultsDoesNotFollowRef), which would make the same input
	// behave differently depending on the surface that sent it.
	nested := []struct {
		name  string
		build func() *jsonschema.Schema
	}{
		{"at the root", func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: "object", Ref: "#/$defs/x"}
		}},
		{"in a property", func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
				"a": {Ref: "#/$defs/x"},
			}}
		}},
		{"in items", func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
				"a": {Type: "array", Items: &jsonschema.Schema{Ref: "#/$defs/x"}},
			}}
		}},
		{"in anyOf", func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
				"a": {AnyOf: []*jsonschema.Schema{{Ref: "#/$defs/x"}}},
			}}
		}},
		{"via $defs", func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: "object", Defs: map[string]*jsonschema.Schema{
				"x": {Type: "string"},
			}}
		}},
	}
	for _, tt := range nested {
		t.Run(tt.name, func(t *testing.T) {
			spec := goodSpec()
			spec.Ops[0].Input = tt.build()
			_, err := Validate(spec)
			if err == nil {
				t.Fatal("accepted a schema containing a reference")
			}
			if !strings.Contains(err.Error(), "$ref") && !strings.Contains(err.Error(), "$defs") {
				t.Errorf("error %q does not explain the reference", err)
			}
		})
	}
}

func TestParseRejectsOversizeAndUnknownFields(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("accepted an empty manifest")
	}
	if _, err := Parse(make([]byte, MaxSize+1)); err == nil {
		t.Error("accepted an oversize manifest")
	}
	// An unknown field usually means the tool was built against a newer forge,
	// and the message should say so rather than only quoting encoding/json.
	raw := []byte(`{"name":"x","abi":1,"ops":[],"futureThing":true}`)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), "newer forge") {
		t.Errorf("got %v, want a hint about a newer forge", err)
	}
}

func TestParseRoundTripsAGoodManifest(t *testing.T) {
	raw, err := json.Marshal(goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if l.Spec.Name != "jsonfmt" || len(l.Spec.Ops) != 1 {
		t.Errorf("round trip lost data: %+v", l.Spec)
	}
}

func TestCanonicalSchemaIsOrderIndependent(t *testing.T) {
	// Two schemas that mean the same thing must produce identical bytes, or the
	// cross-surface schema comparison is meaningless.
	a := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
		"z": {Type: "string"}, "a": {Type: "integer"},
	}}
	b := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
		"a": {Type: "integer"}, "z": {Type: "string"},
	}}
	ca, err := canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Errorf("canonical forms differ:\n a: %s\n b: %s", ca, cb)
	}
}

func TestDefaultsThatViolateTheirOwnSchemaAreRejected(t *testing.T) {
	// ValidateDefaults is on because otherwise forge would hand a tool a
	// default value that its own schema says is invalid.
	spec := goodSpec()
	spec.Ops[0].Input = &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"count": {Type: "integer", Default: json.RawMessage(`"not a number"`)},
		},
	}
	if _, err := Validate(spec); err == nil {
		t.Error("accepted a default that violates its own schema")
	}
}

// TestApplyDefaultsDoesNotFollowRef records the upstream behaviour this package
// works around. If jsonschema-go ever fixes it, this test fails and the $ref
// rejection above can be reconsidered -- which is the point of pinning it here
// rather than only writing it in a comment.
func TestApplyDefaultsDoesNotFollowRef(t *testing.T) {
	s := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"a": {Ref: "#/$defs/withDefault"},
		},
		Defs: map[string]*jsonschema.Schema{
			"withDefault": {Type: "object", Properties: map[string]*jsonschema.Schema{
				"n": {Type: "integer", Default: json.RawMessage(`7`)},
			}},
		},
	}
	res, err := s.Resolve(nil)
	if err != nil {
		t.Skipf("schema did not resolve: %v", err)
	}

	inst := map[string]any{"a": map[string]any{}}
	err = func() (err error) {
		// ApplyDefaults is documented as able to panic; forge recovers around
		// it in binding, and so does this test.
		defer func() {
			if r := recover(); r != nil {
				t.Logf("ApplyDefaults panicked: %v", r)
				err = nil
			}
		}()
		return res.ApplyDefaults(&inst)
	}()
	if err != nil {
		t.Logf("ApplyDefaults returned: %v", err)
	}

	inner, _ := inst["a"].(map[string]any)
	if _, filled := inner["n"]; filled {
		t.Error("ApplyDefaults now follows $ref; revisit the rejection in rejectRefs")
	} else {
		t.Log("confirmed: defaults behind a $ref are not applied, which is why manifests reject $ref")
	}
}
