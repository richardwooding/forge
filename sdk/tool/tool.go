package tool

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// Capability names what a tool needs from the host. The strings match forge's
// own capability kinds.
type Capability string

const (
	FSRead    Capability = "fs.read"
	FSWrite   Capability = "fs.write"
	NetHTTP   Capability = "net.http"
	Env       Capability = "env"
	Secret    Capability = "secret"
	ClockWall Capability = "clock.wall"
	KV        Capability = "kv"
	Invoke    Capability = "tool.invoke"
)

// Need declares a capability the tool requires.
//
// Reason is shown to the person deciding whether to grant it. Write it for
// them, not for the compiler: "fetch the pages you ask it to summarise" earns a
// yes in a way that "network access" does not.
type Need struct {
	Kind   Capability `json:"kind"`
	Scope  []string   `json:"scope"`
	Reason string     `json:"reason,omitempty"`
}

// Spec describes the tool itself.
type Spec struct {
	Name        string
	Version     string
	Summary     string
	Description string
	Labels      []string
	Needs       []Need

	// Reuse lets forge keep an instance of this tool alive between calls. Only
	// set it if the tool is genuinely stateless between invocations: package
	// variables, open files and paused goroutines all survive into the next
	// call. A fresh instance costs about three milliseconds, so this is worth
	// very little unless a tool is called in a tight loop.
	Reuse bool
}

// Operation is one callable entry point, produced by [Op].
type Operation struct {
	name        string
	summary     string
	input       *jsonschema.Schema
	output      *jsonschema.Schema
	kind        string
	mediaType   string
	annotations annotations
	invoke      func(*Context, json.RawMessage) (any, error)
	err         error
}

type annotations struct {
	ReadOnly    bool `json:"readOnly,omitempty"`
	Destructive bool `json:"destructive,omitempty"`
	Idempotent  bool `json:"idempotent,omitempty"`
	OpenWorld   bool `json:"openWorld,omitempty"`
}

// OpOption adjusts an operation.
type OpOption func(*Operation)

// Summary sets the one-line description shown in listings and help.
func Summary(s string) OpOption { return func(o *Operation) { o.summary = s } }

// Text declares that the operation returns plain text rather than JSON. The
// handler's return value must be a string.
func Text() OpOption { return func(o *Operation) { o.kind = "text" } }

// Bytes declares that the operation returns opaque data of the given media
// type. The handler's return value must be []byte.
//
// The media type is required because it is not decoration: REST needs it for
// Content-Type and MCP needs it to choose between an image, audio and an
// embedded resource block. Neither can guess.
func Bytes(mediaType string) OpOption {
	return func(o *Operation) { o.kind, o.mediaType = "bytes", mediaType }
}

// ReadOnly marks the operation as not modifying anything.
//
// This and its siblings are the tool's own claims. Nothing verifies them: they
// are useful for telling a person what to expect, and never a reason to skip an
// approval that would otherwise be required.
func ReadOnly() OpOption    { return func(o *Operation) { o.annotations.ReadOnly = true } }
func Destructive() OpOption { return func(o *Operation) { o.annotations.Destructive = true } }
func Idempotent() OpOption  { return func(o *Operation) { o.annotations.Idempotent = true } }
func OpenWorld() OpOption   { return func(o *Operation) { o.annotations.OpenWorld = true } }

// Op defines an operation from a typed handler.
//
// The input schema is inferred from In, so the Go type is the single source of
// truth for what the tool accepts -- there is no separate schema to drift out of
// step with the struct the handler actually reads. Use `json` tags for names and
// `jsonschema` tags for descriptions.
func Op[In, Out any](name string, fn func(*Context, In) (Out, error), opts ...OpOption) Operation {
	o := Operation{name: name, kind: "json"}

	in, err := jsonschema.For[In](nil)
	if err != nil {
		o.err = fmt.Errorf("operation %q: cannot derive an input schema from %T: %w", name, *new(In), err)
		return o
	}
	// forge requires an object schema: every surface binds named parameters.
	// A handler taking a bare scalar cannot be expressed, so say so here rather
	// than letting the manifest be rejected with less context.
	if in.Type != "object" {
		o.err = fmt.Errorf("operation %q: input must be a struct, got %s", name, in.Type)
		return o
	}
	o.input = in

	for _, opt := range opts {
		opt(&o)
	}

	if err := checkOutput[Out](name, o.kind); err != nil {
		o.err = err
		return o
	}
	if o.kind == "json" {
		if out, err := jsonschema.For[Out](nil); err == nil {
			o.output = out
		}
	}

	o.invoke = func(ctx *Context, raw json.RawMessage) (any, error) {
		var in In
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("cannot decode input: %w", err)
			}
		}
		return fn(ctx, in)
	}
	return o
}

// checkOutput rejects a handler whose return type cannot carry the declared
// output kind, at registration rather than on first call.
func checkOutput[Out any](name, kind string) error {
	t := reflect.TypeFor[Out]()
	switch kind {
	case "text":
		if t.Kind() != reflect.String {
			return fmt.Errorf("operation %q declares Text() but returns %s", name, t)
		}
	case "bytes":
		if t.Kind() != reflect.Slice || t.Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("operation %q declares Bytes() but returns %s", name, t)
		}
	}
	return nil
}

// registered is the tool this module declares. A module registers exactly one
// tool; several operations belong on one tool rather than on several.
var registered *registration

type registration struct {
	spec Spec
	ops  []Operation
	err  error
}

// Register declares the tool. Assign it to a package-level variable:
//
//	var _ = tool.Register(tool.Spec{Name: "jsonfmt"}, tool.Op("format", format))
//
// It returns a value only so that it can be used that way. Calling it from func
// main has no effect, because a reactor module's main never runs.
func Register(spec Spec, ops ...Operation) struct{} {
	r := &registration{spec: spec, ops: ops}
	switch {
	case registered != nil:
		r.err = fmt.Errorf("tool %q: Register called more than once in one module", spec.Name)
	case len(ops) == 0:
		r.err = fmt.Errorf("tool %q: no operations", spec.Name)
	}
	for _, op := range ops {
		if op.err != nil {
			r.err = op.err
			break
		}
	}
	if registered == nil {
		registered = r
	}
	return struct{}{}
}
