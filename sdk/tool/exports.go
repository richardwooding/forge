package tool

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// The wire envelopes. These are the guest half of a contract whose other half
// is forge's wasmrt package; the round trip is asserted by a test there that
// builds a real guest, so the two cannot drift silently.

type callEnvelope struct {
	Op    string          `json:"op"`
	Input json.RawMessage `json:"input,omitempty"`
}

type resultEnvelope struct {
	OK bool `json:"ok"`

	Output json.RawMessage `json:"output,omitempty"`
	Text   string          `json:"text,omitempty"`
	Bytes  string          `json:"bytes,omitempty"` // base64

	Error string `json:"error,omitempty"`
}

// manifest is the JSON forge parses. Its field names must match forge's
// core.Spec exactly: the parser rejects unknown fields so that a tool built for
// a newer forge fails with a clear message rather than by silently losing part
// of its own description.
type manifestJSON struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Ops         []opJSON `json:"ops"`
	Requires    []Need   `json:"requires,omitempty"`
	Reuse       bool     `json:"reuse,omitempty"`
	ABI         int      `json:"abi"`
}

type opJSON struct {
	Name            string      `json:"name"`
	Summary         string      `json:"summary,omitempty"`
	Input           any         `json:"input"`
	Output          any         `json:"output,omitempty"`
	OutputKind      string      `json:"outputKind"`
	OutputMediaType string      `json:"outputMediaType,omitempty"`
	Annotations     annotations `json:"annotations,omitzero"`
}

func describe() []byte {
	if registered == nil {
		return errorManifest("no tool registered: call tool.Register from a package-level var or init, not from main")
	}
	if registered.err != nil {
		return errorManifest(registered.err.Error())
	}

	m := manifestJSON{
		Name:        registered.spec.Name,
		Version:     registered.spec.Version,
		Summary:     registered.spec.Summary,
		Description: registered.spec.Description,
		Labels:      registered.spec.Labels,
		Requires:    registered.spec.Needs,
		Reuse:       registered.spec.Reuse,
		ABI:         ABIVersion,
	}
	for _, op := range registered.ops {
		j := opJSON{
			Name:            op.name,
			Summary:         op.summary,
			Input:           op.input,
			OutputKind:      op.kind,
			OutputMediaType: op.mediaType,
			Annotations:     op.annotations,
		}
		// Assign only when there is a schema. A nil *jsonschema.Schema stored
		// in an `any` is not an empty interface, so omitempty would not drop it
		// and the manifest would carry "output": null -- which then travels
		// into the canonical schema bytes every surface is compared on.
		if op.output != nil {
			j.Output = op.output
		}
		m.Ops = append(m.Ops, j)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return errorManifest("cannot encode manifest: " + err.Error())
	}
	return b
}

// errorManifest reports a registration problem through the same channel as a
// good manifest, so forge can name the tool's mistake instead of reporting an
// empty or malformed description.
func errorManifest(msg string) []byte {
	b, _ := json.Marshal(struct {
		Error string `json:"forgeError"`
	}{msg})
	return b
}

func invoke(raw []byte) []byte {
	if registered == nil || registered.err != nil {
		return failure("tool is not registered correctly")
	}
	var call callEnvelope
	if err := json.Unmarshal(raw, &call); err != nil {
		return failure("cannot decode call: " + err.Error())
	}

	var op *Operation
	for i := range registered.ops {
		if registered.ops[i].name == call.Op {
			op = &registered.ops[i]
			break
		}
	}
	if op == nil {
		return failure("no such operation: " + call.Op)
	}

	out, err := runHandler(op, &Context{op: call.Op}, call.Input)
	if err != nil {
		// A handler returning an error is a tool that ran and failed, which is
		// not the same as the tool being broken; forge reports it as a result,
		// not as an invocation failure.
		return failure(err.Error())
	}

	res := resultEnvelope{OK: true}
	switch op.kind {
	case "text":
		s, _ := out.(string)
		res.Text = s
	case "bytes":
		b, _ := out.([]byte)
		res.Bytes = base64.StdEncoding.EncodeToString(b)
	default:
		b, err := json.Marshal(out)
		if err != nil {
			return failure("cannot encode result: " + err.Error())
		}
		res.Output = b
	}
	b, err := json.Marshal(res)
	if err != nil {
		return failure("cannot encode result envelope: " + err.Error())
	}
	return b
}

// runHandler calls the tool's own code, converting a panic into an ordinary
// failure. A panic would otherwise unwind through the wasm export as a trap,
// which tells forge only that the module died -- so the tool's user would get
// "runtime failure" instead of the message the panic carried.
func runHandler(op *Operation, ctx *Context, input json.RawMessage) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return op.invoke(ctx, input)
}

func failure(msg string) []byte {
	b, _ := json.Marshal(resultEnvelope{OK: false, Error: msg})
	return b
}

// sprintf is fmt.Sprintf, kept behind a name so the logging helpers do not each
// import fmt at their call sites.
func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
