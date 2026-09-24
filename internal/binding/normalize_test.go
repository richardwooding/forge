package binding

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/manifest"
)

// mustLoad wraps a bare input schema in a minimal valid manifest.
func mustLoad(t testing.TB, input *jsonschema.Schema) *manifest.Loaded {
	t.Helper()
	l, err := manifest.Validate(core.Spec{
		Name: "fixture",
		ABI:  core.ABICurrent,
		Ops: []core.OpSpec{{
			Name:       "op",
			Input:      input,
			OutputKind: core.OutputText,
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Validate: %v", err)
	}
	return l
}

func mustBind(t testing.TB, input *jsonschema.Schema) *Bound {
	t.Helper()
	b, err := Bind(mustLoad(t, input), "op")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return b
}

func TestNormalizeAppliesDefaultsThenValidates(t *testing.T) {
	b := mustBind(t, obj(map[string]*jsonschema.Schema{
		"name":  {Type: "string"},
		"count": {Type: "integer", Default: json.RawMessage(`3`)},
	}, "name"))

	out, fault := Normalize(b, json.RawMessage(`{"name":"x"}`))
	if fault != nil {
		t.Fatalf("Normalize: %v", fault)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["count"] != float64(3) {
		t.Errorf("default not applied: %v", got)
	}

	// The order matters: validating before applying defaults would reject an
	// input that the defaults would have completed.
	b2 := mustBind(t, obj(map[string]*jsonschema.Schema{
		"name":  {Type: "string"},
		"count": {Type: "integer", Default: json.RawMessage(`3`)},
	}))
	if _, fault := Normalize(b2, json.RawMessage(`{}`)); fault != nil {
		t.Errorf("an optional field with a default should not need to be supplied: %v", fault)
	}
}

func TestRequiredPropertyWithADefaultIsRefusedByTheManifest(t *testing.T) {
	// jsonschema-go ignores defaults on required properties, so such a default
	// is never applied. Rather than let a tool ship a declaration that silently
	// does nothing, the manifest refuses it.
	_, err := manifest.Validate(core.Spec{
		Name: "fixture",
		ABI:  core.ABICurrent,
		Ops: []core.OpSpec{{
			Name:       "op",
			OutputKind: core.OutputText,
			Input: obj(map[string]*jsonschema.Schema{
				"count": {Type: "integer", Default: json.RawMessage(`3`)},
			}, "count"),
		}},
	})
	if err == nil {
		t.Fatal("accepted a property that is both required and defaulted")
	}
	if !strings.Contains(err.Error(), "never be applied") {
		t.Errorf("error %q does not explain that the default is dead", err)
	}
}

func TestNormalizePreservesLargeIntegers(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1
	b := mustBind(t, obj(map[string]*jsonschema.Schema{"n": {Type: "integer"}}))

	out, fault := Normalize(b, json.RawMessage(`{"n":`+big+`}`))
	if fault != nil {
		t.Fatalf("Normalize: %v", fault)
	}
	if !strings.Contains(string(out), big) {
		t.Errorf("Normalize rounded a large integer: %s", out)
	}
}

func TestNormalizeRejectsNonObjectAndBadJSON(t *testing.T) {
	b := mustBind(t, obj(map[string]*jsonschema.Schema{"name": {Type: "string"}}))
	for name, raw := range map[string]string{
		"array":     `[]`,
		"string":    `"x"`,
		"number":    `3`,
		"null":      `null`,
		"truncated": `{"name":`,
		"trailing":  `{} {}`,
	} {
		if _, fault := Normalize(b, json.RawMessage(raw)); fault == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
}

func TestNormalizeTreatsEmptyInputAsAnEmptyObject(t *testing.T) {
	// Every surface has some way of sending "no parameters" -- no flags, an
	// empty body, an omitted arguments field -- and they must all mean the
	// same thing.
	b := mustBind(t, obj(map[string]*jsonschema.Schema{"name": {Type: "string"}}))
	for _, raw := range []string{"", "   ", "{}"} {
		if _, fault := Normalize(b, json.RawMessage(raw)); fault != nil {
			t.Errorf("Normalize(%q) = %v, want it treated as {}", raw, fault)
		}
	}
}

func TestViolationsCarryPointersForTheCommonMistakes(t *testing.T) {
	// jsonschema-go returns one flat message with no location, which is fine
	// for a terminal and useless for a REST client. These are the mistakes that
	// actually happen, and all of them are reported, not just the first.
	b := mustBind(t, &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string"},
			"mode": {Type: "string", Enum: []any{"fast", "slow"}},
			"nest": {Type: "object", Properties: map[string]*jsonschema.Schema{
				"n": {Type: "integer"},
			}},
		},
		Required: []string{"name"},
	})

	tests := []struct {
		name, input, wantPointer, wantMessage string
	}{
		{"missing required", `{}`, "/name", "required"},
		{"wrong type", `{"name":3}`, "/name", "expected string"},
		{"bad enum", `{"name":"x","mode":"medium"}`, "/mode", "not one of"},
		{"nested wrong type", `{"name":"x","nest":{"n":"no"}}`, "/nest/n", "expected integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fault := Normalize(b, json.RawMessage(tt.input))
			if fault == nil {
				t.Fatal("accepted invalid input")
			}
			if fault.Code != FaultInvalidInput {
				t.Errorf("code = %s, want %s", fault.Code, FaultInvalidInput)
			}
			var found bool
			for _, v := range fault.Violations {
				if v.Pointer == tt.wantPointer && strings.Contains(v.Message, tt.wantMessage) {
					found = true
				}
			}
			if !found {
				t.Errorf("violations %+v do not include %s / %q", fault.Violations, tt.wantPointer, tt.wantMessage)
			}
		})
	}
}

func TestViolationsReportEveryProblemNotJustTheFirst(t *testing.T) {
	b := mustBind(t, &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"a": {Type: "string"},
			"b": {Type: "string"},
		},
		Required: []string{"a", "b"},
	})
	_, fault := Normalize(b, json.RawMessage(`{}`))
	if fault == nil {
		t.Fatal("accepted invalid input")
	}
	if len(fault.Violations) != 2 {
		t.Errorf("got %d violations, want 2: %+v", len(fault.Violations), fault.Violations)
	}
}

func TestFaultCarriesToolAndOperation(t *testing.T) {
	// Every surface prints the same message; it has to say which tool.
	b := mustBind(t, obj(map[string]*jsonschema.Schema{"name": {Type: "string"}}, "name"))
	_, fault := Normalize(b, json.RawMessage(`{}`))
	if fault == nil {
		t.Fatal("accepted invalid input")
	}
	if fault.Tool != "fixture" || fault.Op != "op" {
		t.Errorf("fault = (%q, %q), want (fixture, op)", fault.Tool, fault.Op)
	}
}

func TestIntegerAcceptsAWholeNumberButNotAFraction(t *testing.T) {
	b := mustBind(t, obj(map[string]*jsonschema.Schema{"n": {Type: "integer"}}))
	if _, fault := Normalize(b, json.RawMessage(`{"n":3}`)); fault != nil {
		t.Errorf("rejected a whole number for an integer: %v", fault)
	}
	if _, fault := Normalize(b, json.RawMessage(`{"n":1.5}`)); fault == nil {
		t.Error("accepted a fraction for an integer")
	}
}

func TestFaultCodeMappingsAreTotal(t *testing.T) {
	// The parity table is a contract; every code must map on every surface.
	for _, c := range []FaultCode{
		FaultInvalidInput, FaultNotFound, FaultNotInView,
		FaultInternal, FaultDeadline, FaultExhausted,
	} {
		if c.ExitCode() == 0 {
			t.Errorf("%s has exit code 0, which means success", c)
		}
		if s := c.HTTPStatus(); s < 400 || s > 599 {
			t.Errorf("%s maps to HTTP %d, which is not an error status", c, s)
		}
	}
}
