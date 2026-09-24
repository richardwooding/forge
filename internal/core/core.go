// Package core holds the types every other part of forge agrees on: what a tool
// is, how it is looked up, and how it is called.
//
// It deliberately knows nothing about wasm, and nothing about CLI, MCP, REST or
// gRPC. The runtime implements these interfaces; the surfaces consume them. That
// is what lets five surfaces stay in step — they are all looking at the same
// Spec and calling the same Invoke.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/richardwooding/forge/internal/capability"
)

// ABIVersion is the guest/host contract a tool was built against. forge refuses
// a tool whose ABI it does not implement, rather than letting an old guest fail
// in some more interesting way later.
type ABIVersion int

// ABICurrent is the ABI this build of forge speaks.
const ABICurrent ABIVersion = 1

// OutputKind is the envelope an operation returns. It says how to carry the
// result, not what shape it has — that is [OpSpec.Output].
type OutputKind string

const (
	// OutputJSON is a JSON value, validated against OpSpec.Output when set.
	OutputJSON OutputKind = "json"
	// OutputText is UTF-8 text with no further structure.
	OutputText OutputKind = "text"
	// OutputBytes is opaque data; OpSpec.OutputMediaType must say what it is.
	OutputBytes OutputKind = "bytes"
	// OutputStream is a sequence of chunks delivered as they are produced.
	OutputStream OutputKind = "stream"
)

// Valid reports whether k is a kind forge knows.
func (k OutputKind) Valid() bool {
	switch k {
	case OutputJSON, OutputText, OutputBytes, OutputStream:
		return true
	}
	return false
}

// Annotations are a tool's own claims about an operation, carried through to
// MCP's tool annotations.
//
// Nothing verifies them. They are a tool author's assertion, useful for showing
// a person what to expect or for tightening a policy — never for skipping an
// approval that would otherwise be required.
type Annotations struct {
	ReadOnly    bool `json:"readOnly,omitempty"`
	Destructive bool `json:"destructive,omitempty"`
	Idempotent  bool `json:"idempotent,omitempty"`
	OpenWorld   bool `json:"openWorld,omitempty"`
}

// OpSpec is one callable operation of a tool. A tool with a single operation is
// the common case; several map to CLI subcommands, separate MCP tools, separate
// REST routes and separate RPCs.
type OpSpec struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`

	// Input is the operation's parameter object. It must be a schema of type
	// "object": every surface binds named parameters, and MCP's AddTool panics
	// outright on anything else, so this is checked when a manifest loads.
	Input *jsonschema.Schema `json:"input"`

	// Output describes the result when known. Optional, because plenty of tools
	// emit text; when present it gives MCP a structured output schema, REST a
	// response schema and gRPC a typed result.
	Output *jsonschema.Schema `json:"output,omitempty"`

	OutputKind OutputKind `json:"outputKind"`

	// OutputMediaType is required when OutputKind is OutputBytes: REST needs a
	// Content-Type and MCP needs to choose between image, audio and an embedded
	// resource.
	OutputMediaType string `json:"outputMediaType,omitempty"`

	Annotations Annotations `json:"annotations,omitzero"`
}

// Spec is everything forge knows about a tool, harvested from the module itself
// at install time.
type Spec struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Ops         []OpSpec `json:"ops"`

	// Requires is what the tool asks for. What it gets is this intersected with
	// the user's policy; see capability.Intersect.
	Requires []capability.Request `json:"requires,omitempty"`

	// Reuse lets forge keep an instance alive between invocations. Off by
	// default because a reactor instance carries package-level state, open
	// files and paused goroutines from one call to the next. A fresh instance
	// costs about 3ms, so this is an optimisation, not a necessity.
	Reuse bool `json:"reuse,omitempty"`

	ABI ABIVersion `json:"abi"`
}

// Op returns the named operation.
func (s Spec) Op(name string) (OpSpec, bool) {
	for _, o := range s.Ops {
		if o.Name == name {
			return o, true
		}
	}
	return OpSpec{}, false
}

// DefaultOp is the operation used when a caller names none. A single-operation
// tool needs no subcommand; with several, the caller must choose.
func (s Spec) DefaultOp() (OpSpec, bool) {
	if len(s.Ops) == 1 {
		return s.Ops[0], true
	}
	return OpSpec{}, false
}

// HasLabel reports whether the tool carries the label.
func (s Spec) HasLabel(l string) bool {
	for _, got := range s.Labels {
		if got == l {
			return true
		}
	}
	return false
}

// Ref names a tool, optionally pinning a version: "jsonfmt" or "jsonfmt@1.2.0".
type Ref struct {
	Name    string
	Version string
}

// ParseRef splits a reference. An empty Version means "whatever is installed".
func ParseRef(s string) (Ref, error) {
	name, version, _ := strings.Cut(s, "@")
	if name == "" {
		return Ref{}, fmt.Errorf("empty tool reference %q", s)
	}
	return Ref{Name: name, Version: version}, nil
}

func (r Ref) String() string {
	if r.Version == "" {
		return r.Name
	}
	return r.Name + "@" + r.Version
}

// Matches reports whether spec satisfies this reference.
func (r Ref) Matches(s Spec) bool {
	return s.Name == r.Name && (r.Version == "" || s.Version == r.Version)
}

// EventKind says what happened to the tool set.
type EventKind string

const (
	EventAdded   EventKind = "added"
	EventRemoved EventKind = "removed"
	EventUpdated EventKind = "updated"

	// EventResync tells a watcher its stream fell behind and was truncated. The
	// watcher must call List again rather than trusting what it has. Dropping a
	// slow consumer and telling it to resync is the only honest option: the
	// alternative is an unbounded buffer that turns one stalled surface into a
	// memory leak for the whole process.
	EventResync EventKind = "resync"
)

// Event is one change to the registry.
type Event struct {
	Kind EventKind
	Ref  Ref
}

// Selector decides which tools a surface may see. It lives here rather than in
// the labels package so core does not depend on the selector's syntax.
type Selector interface {
	Matches(labels []string) bool
	String() string
}

// AllTools is a Selector that admits everything.
var AllTools Selector = allTools{}

type allTools struct{}

func (allTools) Matches([]string) bool { return true }
func (allTools) String() string        { return "*" }

// Registry is the set of installed tools, filtered by a selector.
type Registry interface {
	// List returns the tools matching sel, ordered by name. The ordering is
	// part of the contract: unordered listing makes cross-surface comparison
	// and protobuf descriptor generation flake for no reason.
	List(sel Selector) []Spec

	// Get looks up one tool by reference.
	Get(ref Ref) (Spec, bool)

	// Watch returns a channel of changes. Each call gets its own channel, so
	// five surfaces can watch at once without starving each other, and the
	// channel closes when ctx is done.
	Watch(ctx context.Context) <-chan Event
}

// Streams are the byte-level channels of one invocation. A nil Stdout or Stderr
// discards; a nil Stdin reads as empty.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Progress, when set, receives the tool's own progress reports. Total is
	// zero when the tool does not know how much work there is.
	Progress func(done, total int64, message string)
}

// Usage is what one invocation consumed. It is always populated, because the
// numbers are cheap to collect and are the only way to notice a tool getting
// slower or greedier over time.
type Usage struct {
	Wall         int64  `json:"wallNanos"`
	PeakPages    uint32 `json:"peakPages"`
	HostCalls    int    `json:"hostCalls"`
	BytesOut     int64  `json:"bytesOut"`
	Instantiated bool   `json:"instantiated"` // false when a pooled instance was reused
}

// Result is the outcome of an invocation that actually ran.
//
// The distinction that matters on every surface: a non-nil error from Invoke
// means forge could not run the tool, while a Result with ToolError set means
// the tool ran and reported failure. MCP requires the second to arrive as a
// normal result with IsError, not as a protocol error, so the model can see it
// and correct itself; REST answers 422 rather than 500; gRPC answers OK with
// is_error rather than hiding the payload behind a status. Collapsing the two
// would be wrong on three surfaces at once.
type Result struct {
	Output    json.RawMessage
	ExitCode  int
	ToolError bool

	Stdout, Stderr []byte

	// Truncated reports that output hit a cap and was cut. Surfaces must say so
	// rather than silently presenting a partial answer as a whole one.
	Truncated bool

	Usage Usage
}

// Call is one request to run an operation.
type Call struct {
	Ref Ref
	Op  string

	// Input is canonical JSON, already validated against the operation's input
	// schema. It is deliberately not a map: every surface can produce JSON
	// losslessly, whereas decoding to a map first turns every number into a
	// float64 and quietly breaks integer schemas past 2^53.
	Input json.RawMessage

	Streams Streams
}

// Invoker runs tools.
type Invoker interface {
	Invoke(ctx context.Context, c Call) (Result, error)
}
