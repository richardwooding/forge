package binding

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func obj(props map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required}
}

func TestBuildFlagsMapsScalarsAndNesting(t *testing.T) {
	s := obj(map[string]*jsonschema.Schema{
		"name":    {Type: "string"},
		"count":   {Type: "integer", Default: json.RawMessage(`3`)},
		"ratio":   {Type: "number"},
		"verbose": {Type: "boolean"},
		"db": obj(map[string]*jsonschema.Schema{
			"host": {Type: "string"},
			"port": {Type: "integer"},
		}),
	}, "name")

	fs := BuildFlags(s)
	if !fs.Complete() {
		t.Fatalf("expected a fully expressible schema, got %+v", fs.NotExpressible)
	}

	want := map[string]struct {
		kind    FlagKind
		pointer string
	}{
		"name":    {FlagString, "/name"},
		"count":   {FlagInteger, "/count"},
		"ratio":   {FlagNumber, "/ratio"},
		"verbose": {FlagBool, "/verbose"},
		"db.host": {FlagString, "/db/host"},
		"db.port": {FlagInteger, "/db/port"},
	}
	if len(fs.Flags) != len(want) {
		t.Fatalf("got %d flags, want %d: %+v", len(fs.Flags), len(want), fs.Flags)
	}
	for _, f := range fs.Flags {
		w, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected flag %q", f.Name)
			continue
		}
		if f.Kind != w.kind || f.Pointer != w.pointer {
			t.Errorf("flag %q = (%s, %s), want (%s, %s)", f.Name, f.Kind, f.Pointer, w.kind, w.pointer)
		}
	}

	// A default is shown, never applied: Normalize is the only place defaults
	// are filled in, so that the CLI and MCP cannot disagree about them.
	count, _ := fs.Flag("count")
	if count.DefaultDisplay != "3" {
		t.Errorf("count default display = %q, want %q", count.DefaultDisplay, "3")
	}
	name, _ := fs.Flag("name")
	if !name.Required {
		t.Error("name should be marked required for help text")
	}
}

func TestArraysOfScalarsBecomeRepeatableFlags(t *testing.T) {
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{
		"tag": {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
	}))
	f, ok := fs.Flag("tag")
	if !ok {
		t.Fatal("no tag flag")
	}
	if !f.Repeatable {
		t.Error("array of scalars should be repeatable")
	}
}

// TestCommaContainingValuesSurviveRoundTrip is the pflag StringSlice trap. That
// type splits values on commas, which corrupts any string containing one; this
// asserts forge's own encoding does not, so a CLI-only divergence cannot creep
// in through the flag layer.
func TestCommaContainingValuesSurviveRoundTrip(t *testing.T) {
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{
		"tag": {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
	}))
	values := map[string][]string{"tag": {"a,b", "c", "d,e,f"}}

	doc, fault := fs.Document(values)
	if fault != nil {
		t.Fatalf("Document: %v", fault)
	}
	var got struct {
		Tag []string `json:"tag"`
	}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tag, []string{"a,b", "c", "d,e,f"}) {
		t.Errorf("tags = %q, want the commas preserved", got.Tag)
	}
}

// TestLargeIntegersSurviveTheFlagLayer guards the 2^53 boundary. If any step
// decoded through float64, 9007199254740993 would come back as ...992.
func TestLargeIntegersSurviveTheFlagLayer(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{"n": {Type: "integer"}}))

	doc, fault := fs.Document(map[string][]string{"n": {big}})
	if fault != nil {
		t.Fatalf("Document: %v", fault)
	}
	if !strings.Contains(string(doc), big) {
		t.Errorf("document = %s, want it to contain %s exactly", doc, big)
	}
}

func TestInexpressibleConstructsAreReportedNotDropped(t *testing.T) {
	// Silence would be the worst outcome: the user would set flags, see no
	// error, and get a document missing what they thought they had set.
	tests := []struct {
		name   string
		schema *jsonschema.Schema
		want   string
	}{
		{"oneOf", obj(map[string]*jsonschema.Schema{
			"x": {OneOf: []*jsonschema.Schema{{Type: "string"}, {Type: "integer"}}},
		}), "oneOf"},
		{"anyOf", obj(map[string]*jsonschema.Schema{
			"x": {AnyOf: []*jsonschema.Schema{{Type: "string"}}},
		}), "anyOf"},
		{"not", obj(map[string]*jsonschema.Schema{
			"x": {Not: &jsonschema.Schema{Type: "string"}},
		}), "not"},
		{"if/then", obj(map[string]*jsonschema.Schema{
			"x": {If: &jsonschema.Schema{Type: "string"}},
		}), "conditional"},
		{"array of objects", obj(map[string]*jsonschema.Schema{
			"x": {Type: "array", Items: obj(map[string]*jsonschema.Schema{"a": {Type: "string"}})},
		}), "array of"},
		{"tuple", obj(map[string]*jsonschema.Schema{
			"x": {Type: "array", PrefixItems: []*jsonschema.Schema{{Type: "string"}}},
		}), "tuple"},
		{"untyped", obj(map[string]*jsonschema.Schema{"x": {}}), "no declared type"},
		{"patternProperties", &jsonschema.Schema{
			Type:              "object",
			PatternProperties: map[string]*jsonschema.Schema{"^x": {Type: "string"}},
		}, "patternProperties"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := BuildFlags(tt.schema)
			if fs.Complete() {
				t.Fatalf("%s was reported as fully expressible", tt.name)
			}
			var joined string
			for _, ne := range fs.NotExpressible {
				joined += ne.Reason + " "
			}
			if !strings.Contains(joined, tt.want) {
				t.Errorf("reasons %q do not mention %q", joined, tt.want)
			}
		})
	}
}

func TestDocumentRejectsBadValues(t *testing.T) {
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{
		"n": {Type: "integer"},
		"b": {Type: "boolean"},
	}))
	for name, values := range map[string]map[string][]string{
		"not an integer": {"n": {"twelve"}},
		"not a boolean":  {"b": {"maybe"}},
		"unknown flag":   {"nope": {"x"}},
	} {
		if _, fault := fs.Document(values); fault == nil {
			t.Errorf("%s: accepted", name)
		} else if fault.Code != FaultInvalidInput {
			t.Errorf("%s: code = %s, want %s", name, fault.Code, FaultInvalidInput)
		}
	}
}

func TestRepeatedScalarFlagTakesTheLastValue(t *testing.T) {
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{"name": {Type: "string"}}))
	doc, fault := fs.Document(map[string][]string{"name": {"first", "second"}})
	if fault != nil {
		t.Fatal(fault)
	}
	if !strings.Contains(string(doc), "second") || strings.Contains(string(doc), "first") {
		t.Errorf("document = %s, want the last value to win", doc)
	}
}

func TestFlattenReportsWhatFlagsCannotCarry(t *testing.T) {
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{
		"name": {Type: "string"},
		"nest": obj(map[string]*jsonschema.Schema{"a": {Type: "string"}}),
		"tag":  {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
	}))

	// An empty nested object has no leaf to hang a flag on, and an empty array
	// is indistinguishable from an unset repeatable flag. Both must be called
	// out rather than silently lost.
	_, un := fs.Flatten(map[string]any{"nest": map[string]any{}})
	if len(un) == 0 {
		t.Error("empty nested object was not reported as unexpressible")
	}
	_, un = fs.Flatten(map[string]any{"tag": []any{}})
	if len(un) == 0 {
		t.Error("empty array was not reported as unexpressible")
	}
}

func TestPropertyNamesNeedingPointerEscapingRoundTrip(t *testing.T) {
	// RFC 6901 escaping: "/" and "~" inside a property name.
	fs := BuildFlags(obj(map[string]*jsonschema.Schema{
		"a/b": {Type: "string"},
		"c~d": {Type: "string"},
	}))
	doc, fault := fs.Document(map[string][]string{"a/b": {"x"}, "c~d": {"y"}})
	if fault != nil {
		t.Fatal(fault)
	}
	var got map[string]any
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if got["a/b"] != "x" || got["c~d"] != "y" {
		t.Errorf("document = %v, want the odd property names preserved", got)
	}
}
