package binding

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// roundTripSchema exercises every construct flags are supposed to handle: a
// string, an integer, a number, a boolean, a nested object and a repeatable
// array of strings.
func roundTripSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":  {Type: "string"},
			"count": {Type: "integer"},
			"ratio": {Type: "number"},
			"on":    {Type: "boolean"},
			"nest": {Type: "object", Properties: map[string]*jsonschema.Schema{
				"inner": {Type: "string"},
			}},
			"tag": {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
		},
	}
}

// FuzzFlagRoundTrip is the property that keeps the CLI honest:
//
//	for any document the flag set says it can express,
//	document -> flags -> argv -> document is the identity.
//
// Flag encoding is where cross-surface parity dies, and this finds a break
// without needing five surfaces running. Note the precondition: the property is
// only claimed for documents Flatten reports as expressible. Claiming it for
// all documents would be false -- an empty nested object and an empty array
// both have no flag representation -- and stating the weaker, true property is
// worth more than a strong one that has to be explained away.
func FuzzFlagRoundTrip(f *testing.F) {
	f.Add("hello", int64(3), 1.5, true, "inner", "a", "b")
	f.Add("", int64(0), 0.0, false, "", "", "")
	// The pflag StringSlice trap, and the 2^53 boundary.
	f.Add("a,b,c", int64(9007199254740993), -0.0, true, "x,y", "p,q", "r")
	f.Add("naïve 🌍", int64(-9007199254740993), 1e308, false, "\t\n", `"quoted"`, `back\slash`)
	f.Add("--looks-like-a-flag", int64(1), 0.1, true, "=equals=", " leading", "trailing ")

	fs := BuildFlags(roundTripSchema())
	if !fs.Complete() {
		f.Fatalf("the round-trip schema should be fully expressible: %+v", fs.NotExpressible)
	}

	f.Fuzz(func(t *testing.T, name string, count int64, ratio float64, on bool, inner, tag1, tag2 string) {
		ratioJSON, err := json.Marshal(ratio)
		if err != nil {
			t.Skip() // NaN and Inf are not JSON; no surface can carry them either.
		}

		doc := map[string]any{
			"name":  name,
			"count": json.Number(strconv.FormatInt(count, 10)),
			"ratio": json.Number(ratioJSON),
			"on":    on,
			"nest":  map[string]any{"inner": inner},
			"tag":   []any{tag1, tag2},
		}

		values, unexpressible := fs.Flatten(doc)
		if len(unexpressible) > 0 {
			t.Fatalf("Flatten called a document of scalars unexpressible: %v", unexpressible)
		}

		got, fault := fs.Document(values)
		if fault != nil {
			t.Fatalf("Document: %v (values %#v)", fault, values)
		}

		want, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("round trip changed the document\n via: %#v\n  got: %s\n want: %s", values, got, want)
		}
	})
}

// FuzzNormalizeDoesNotPanic asserts the shared input path survives arbitrary
// bytes. Normalize is reached by every surface, including one fed by a model,
// so a panic here would be a denial of service on all five at once -- and
// ApplyDefaults is documented as able to panic, which is exactly why Normalize
// recovers around it.
func FuzzNormalizeDoesNotPanic(f *testing.F) {
	for _, seed := range []string{
		"", "{}", "null", "[]", `{"name":"x"}`, `{"count":9007199254740993}`,
		`{"count":1.5}`, `{"nest":{"inner":3}}`, `{"unknown":true}`,
		`{"tag":["a","b"]}`, "{", `{"a":`, "\x00", `{"name":"\ud800"}`,
	} {
		f.Add(seed)
	}

	l := mustLoad(f, roundTripSchema())
	b, err := Bind(l, "op")
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		out, fault := Normalize(b, json.RawMessage(raw))
		switch {
		case fault != nil:
			if fault.Code == "" {
				t.Errorf("fault with no code for input %q", raw)
			}
			if fault.Message == "" {
				t.Errorf("fault with no message for input %q", raw)
			}
		default:
			// Whatever comes back must be valid JSON, or a surface will fail
			// trying to render it.
			if !json.Valid(out) {
				t.Errorf("Normalize(%q) returned invalid JSON: %q", raw, out)
			}
		}
	})
}
