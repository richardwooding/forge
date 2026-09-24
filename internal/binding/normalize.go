package binding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/manifest"
)

// Bound is everything the surfaces need for one operation, derived once.
type Bound struct {
	Spec core.Spec
	Op   core.OpSpec

	Resolved *jsonschema.Resolved
	Flags    FlagSet

	// CanonJSON is the canonical input schema. Every surface must advertise
	// exactly these bytes; the conformance suite compares them.
	CanonJSON []byte
}

// Bind derives the per-operation data from a loaded manifest.
func Bind(l *manifest.Loaded, opName string) (*Bound, error) {
	op, ok := l.Spec.Op(opName)
	if !ok {
		return nil, faultf(FaultNotFound, "tool %q has no operation %q", l.Spec.Name, opName)
	}
	return &Bound{
		Spec:      l.Spec,
		Op:        op,
		Resolved:  l.Resolved[opName],
		Flags:     BuildFlags(op.Input),
		CanonJSON: l.CanonJSON[opName],
	}, nil
}

// BindAll derives every operation of a tool, keyed by operation name.
func BindAll(l *manifest.Loaded) (map[string]*Bound, error) {
	out := make(map[string]*Bound, len(l.Spec.Ops))
	for _, op := range l.Spec.Ops {
		b, err := Bind(l, op.Name)
		if err != nil {
			return nil, err
		}
		out[op.Name] = b
	}
	return out, nil
}

// Normalize turns raw input from any surface into the canonical document a tool
// receives. It is the only place this happens, which is what makes five
// surfaces agree about what an input means and about how a bad one is reported.
//
// The order matters: decode preserving number syntax, apply defaults, then
// validate. Validating first would reject an input that defaults would have
// completed; applying defaults after validation would let an invalid default
// through.
func Normalize(b *Bound, raw json.RawMessage) (json.RawMessage, *Fault) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}

	// UseNumber keeps 9007199254740993 as its own digits instead of rounding it
	// to a float64. Every surface produces JSON precisely so this one decode
	// can be the only one, and losing precision here would undo that.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var inst any
	if err := dec.Decode(&inst); err != nil {
		return nil, b.fault(FaultInvalidInput, "input is not valid JSON: %v", err)
	}
	if dec.More() {
		return nil, b.fault(FaultInvalidInput, "input has trailing content after the JSON value")
	}
	if _, ok := inst.(map[string]any); !ok {
		return nil, b.fault(FaultInvalidInput, "input must be a JSON object")
	}

	if fault := b.applyDefaults(&inst); fault != nil {
		return nil, fault
	}

	// Validate against a copy whose numbers are float64. jsonschema-go's
	// validator reflects on the Go type, and json.Number is a string type, so
	// validating the decoded instance directly would report every number as a
	// string -- and, worse, would accept a number where a string was required.
	// The original instance, with its digits intact, is what the tool receives.
	if err := b.Resolved.Validate(forValidation(inst)); err != nil {
		f := b.fault(FaultInvalidInput, "%s", cleanValidationMessage(err))
		f.Violations = violations(b.Op.Input, inst)
		f.Cause = err
		return nil, f
	}

	out, err := json.Marshal(inst)
	if err != nil {
		return nil, b.fault(FaultInternal, "cannot re-encode input: %v", err)
	}
	return out, nil
}

// applyDefaults fills in schema defaults, guarding against the documented
// possibility that ApplyDefaults panics. A panic inside a host function becomes
// a wasm trap that explains nothing, so it is converted to a fault here.
func (b *Bound) applyDefaults(inst *any) (fault *Fault) {
	defer func() {
		if r := recover(); r != nil {
			fault = b.fault(FaultInternal, "applying schema defaults panicked: %v", r)
		}
	}()
	if err := b.Resolved.ApplyDefaults(inst); err != nil {
		return b.fault(FaultInvalidInput, "cannot apply schema defaults: %v", err)
	}
	return nil
}

func (b *Bound) fault(code FaultCode, format string, args ...any) *Fault {
	f := faultf(code, format, args...)
	if b != nil {
		f.Tool, f.Op = b.Spec.Name, b.Op.Name
	}
	return f
}

// cleanValidationMessage trims the schema dump jsonschema-go appends to its
// validation errors. The schema is already available to the caller through
// Bound, and repeating it turns a one-line error into a screenful.
func cleanValidationMessage(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "\nschema:"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}

// violations is a best-effort pre-pass producing JSON Pointers for input
// problems.
//
// It exists because jsonschema-go's Validate returns a single flat prose error
// with no instance location and stops at the first failure. That is fine for a
// terminal and useless for a REST client, which needs to know which field to
// fix. This walk covers the mistakes that actually happen -- a missing required
// property, a wrong type, a value outside an enum, an unexpected property --
// and reports every one of them rather than only the first. Anything subtler is
// still caught by Validate; it just arrives without a pointer.
func violations(s *jsonschema.Schema, inst any) []Violation {
	var out []Violation
	var walk func(s *jsonschema.Schema, inst any, pointer string)

	walk = func(s *jsonschema.Schema, inst any, pointer string) {
		if s == nil || inst == nil {
			return
		}
		if t := s.Type; t != "" {
			if got, ok := jsonTypeOf(inst); ok && !typeMatches(t, got, inst) {
				out = append(out, Violation{
					Pointer: ptr(pointer),
					Message: fmt.Sprintf("expected %s, got %s", t, got),
				})
				return
			}
		}
		if len(s.Enum) > 0 && !inEnum(s.Enum, inst) {
			out = append(out, Violation{
				Pointer: ptr(pointer),
				Message: "value is not one of " + strings.Join(enumStrings(s), ", "),
			})
			return
		}

		switch v := inst.(type) {
		case map[string]any:
			for _, req := range s.Required {
				if _, ok := v[req]; !ok {
					out = append(out, Violation{
						Pointer: pointer + "/" + escapePointer(req),
						Message: "required property is missing",
					})
				}
			}
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				prop, ok := s.Properties[k]
				if !ok {
					if s.AdditionalProperties != nil && isFalseSchema(s.AdditionalProperties) {
						out = append(out, Violation{
							Pointer: pointer + "/" + escapePointer(k),
							Message: "unexpected property",
						})
					}
					continue
				}
				walk(prop, v[k], pointer+"/"+escapePointer(k))
			}
		case []any:
			if s.Items != nil {
				for i, item := range v {
					walk(s.Items, item, fmt.Sprintf("%s/%d", pointer, i))
				}
			}
		}
	}

	walk(s, inst, "")
	return out
}

func jsonTypeOf(v any) (string, bool) {
	switch v.(type) {
	case nil:
		return "null", true
	case bool:
		return "boolean", true
	case string:
		return "string", true
	case json.Number, float64:
		return "number", true
	case map[string]any:
		return "object", true
	case []any:
		return "array", true
	}
	return "", false
}

func typeMatches(want, got string, inst any) bool {
	if want == got {
		return true
	}
	// "integer" is a number whose value has no fractional part.
	if want == "integer" && got == "number" {
		n, ok := inst.(json.Number)
		if !ok {
			f, isFloat := inst.(float64)
			return isFloat && f == float64(int64(f))
		}
		_, err := n.Int64()
		return err == nil
	}
	return false
}

func inEnum(enum []any, inst any) bool {
	want, err := json.Marshal(inst)
	if err != nil {
		return true // cannot tell; leave it to Validate
	}
	for _, e := range enum {
		got, err := json.Marshal(e)
		if err == nil && bytes.Equal(want, got) {
			return true
		}
	}
	return false
}

// forValidation returns a copy of a decoded instance with every json.Number
// replaced by a float64.
//
// This exists for one reason, established by experiment rather than assumed:
// jsonschema-go decides an instance's JSON type from its Go type, and
// json.Number is defined as a string. Validating the decoded instance directly
// therefore reports {"n": 3} as "3 has type string, want integer" -- and,
// far worse, quietly ACCEPTS {"name": 3} where a string is required. The
// precision json.Number protects still matters for the document the tool
// receives, so the two representations are kept apart: float64 for the type
// check, the original digits for the payload.
func forValidation(v any) any {
	switch t := v.(type) {
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		// A number too large for float64 cannot be range-checked meaningfully;
		// leave the type check to the string form rather than invent a value.
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = forValidation(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = forValidation(item)
		}
		return out
	}
	return v
}
