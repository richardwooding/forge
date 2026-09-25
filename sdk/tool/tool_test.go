package tool

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// reset clears the package-level registration between tests. Production code
// registers exactly once, from a package-level var, so this exists only for
// tests.
func reset(t *testing.T) {
	t.Helper()
	registered = nil
	t.Cleanup(func() { registered = nil })
}

type args struct {
	Name string `json:"name" jsonschema:"who to greet"`
	N    int    `json:"n,omitempty"`
}

type result struct {
	Out string `json:"out"`
}

func TestDescribeRefusesWhenNothingIsRegistered(t *testing.T) {
	reset(t)
	// The overwhelmingly likely cause is Register having been called from main,
	// which never runs in a reactor module. The message has to say so, or the
	// author looks in the wrong place.
	var got struct {
		Error string `json:"forgeError"`
	}
	if err := json.Unmarshal(describe(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "not from main") {
		t.Errorf("error = %q, want advice about main", got.Error)
	}
}

func TestDescribeIncludesTheDerivedInputSchema(t *testing.T) {
	reset(t)
	Register(Spec{Name: "demo", Summary: "a demo"},
		Op("run", func(c *Context, a args) (result, error) { return result{a.Name}, nil }))

	var m struct {
		Name string `json:"name"`
		ABI  int    `json:"abi"`
		Ops  []struct {
			Name  string `json:"name"`
			Input struct {
				Type       string `json:"type"`
				Properties map[string]struct {
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"input"`
			OutputKind string `json:"outputKind"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(describe(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "demo" || m.ABI != ABIVersion {
		t.Errorf("manifest = %+v", m)
	}
	if len(m.Ops) != 1 || m.Ops[0].Input.Type != "object" {
		t.Fatalf("ops = %+v", m.Ops)
	}
	// The Go type is the single source of truth: the jsonschema tag becomes the
	// description, and a field without omitempty becomes required.
	if got := m.Ops[0].Input.Properties["name"].Description; got != "who to greet" {
		t.Errorf("name description = %q", got)
	}
	if len(m.Ops[0].Input.Required) != 1 || m.Ops[0].Input.Required[0] != "name" {
		t.Errorf("required = %v, want [name]", m.Ops[0].Input.Required)
	}
}

func TestOpRejectsMistakesAtRegistration(t *testing.T) {
	tests := []struct {
		name string
		op   func() Operation
		want string
	}{
		{
			"non-struct input",
			func() Operation {
				return Op("bad", func(c *Context, s string) (string, error) { return s, nil })
			},
			"must be a struct",
		},
		{
			"Text with a non-string return",
			func() Operation {
				return Op("bad", func(c *Context, a args) (result, error) { return result{}, nil }, Text())
			},
			"declares Text()",
		},
		{
			"Bytes with a non-[]byte return",
			func() Operation {
				return Op("bad", func(c *Context, a args) (string, error) { return "", nil }, Bytes("image/png"))
			},
			"declares Bytes()",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset(t)
			Register(Spec{Name: "demo"}, tt.op())
			var got struct {
				Error string `json:"forgeError"`
			}
			if err := json.Unmarshal(describe(), &got); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Error, tt.want) {
				t.Errorf("error = %q, want it to mention %q", got.Error, tt.want)
			}
		})
	}
}

func TestInvokeEncodesEachOutputKind(t *testing.T) {
	reset(t)
	Register(Spec{Name: "demo"},
		Op("j", func(c *Context, a args) (result, error) { return result{"json:" + a.Name}, nil }),
		Op("t", func(c *Context, a args) (string, error) { return "text:" + a.Name, nil }, Text()),
		Op("b", func(c *Context, a args) ([]byte, error) { return []byte{0x00, 0xff, 0x10}, nil }, Bytes("application/octet-stream")),
	)

	call := func(op string) resultEnvelope {
		t.Helper()
		raw, err := json.Marshal(callEnvelope{Op: op, Input: json.RawMessage(`{"name":"x"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var env resultEnvelope
		if err := json.Unmarshal(invoke(raw), &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatalf("op %s failed: %s", op, env.Error)
		}
		return env
	}

	if got := call("j"); string(got.Output) != `{"out":"json:x"}` {
		t.Errorf("json output = %s", got.Output)
	}
	if got := call("t"); got.Text != "text:x" {
		t.Errorf("text output = %q", got.Text)
	}
	got := call("b")
	b, err := base64.StdEncoding.DecodeString(got.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// Binary must survive intact, including a zero byte and a byte that is not
	// valid UTF-8 -- which is exactly why it travels base64 rather than as text.
	if len(b) != 3 || b[0] != 0x00 || b[1] != 0xff {
		t.Errorf("bytes = %v", b)
	}
}

func TestHandlerErrorAndPanicBothBecomeAFailedResult(t *testing.T) {
	reset(t)
	Register(Spec{Name: "demo"},
		Op("err", func(c *Context, a args) (string, error) { return "", errFor(a.Name) }, Text()),
		Op("panic", func(c *Context, a args) (string, error) { panic("boom " + a.Name) }, Text()),
	)

	for _, op := range []string{"err", "panic"} {
		raw, _ := json.Marshal(callEnvelope{Op: op, Input: json.RawMessage(`{"name":"z"}`)})
		var env resultEnvelope
		if err := json.Unmarshal(invoke(raw), &env); err != nil {
			t.Fatal(err)
		}
		if env.OK {
			t.Errorf("op %s reported success", op)
		}
		if !strings.Contains(env.Error, "z") {
			t.Errorf("op %s error = %q, want the handler's own message", op, env.Error)
		}
	}
}

type namedError string

func (e namedError) Error() string { return "failed for " + string(e) }

func errFor(name string) error { return namedError(name) }

func TestRegisterTwiceIsReported(t *testing.T) {
	reset(t)
	Register(Spec{Name: "one"}, Op("a", func(c *Context, a args) (string, error) { return "", nil }, Text()))
	Register(Spec{Name: "two"}, Op("b", func(c *Context, a args) (string, error) { return "", nil }, Text()))

	// The first registration wins and the manifest still describes it, rather
	// than the module silently describing whichever ran last.
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(describe(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "one" {
		t.Errorf("name = %q, want the first registration to win", m.Name)
	}
}

func TestInvokeReportsAnUnknownOperation(t *testing.T) {
	reset(t)
	Register(Spec{Name: "demo"}, Op("a", func(c *Context, a args) (string, error) { return "", nil }, Text()))
	raw, _ := json.Marshal(callEnvelope{Op: "nope"})
	var env resultEnvelope
	if err := json.Unmarshal(invoke(raw), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || !strings.Contains(env.Error, "nope") {
		t.Errorf("env = %+v", env)
	}
}
