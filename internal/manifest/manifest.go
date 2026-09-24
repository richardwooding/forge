// Package manifest loads, validates and canonicalises a tool's self-description.
//
// Everything here runs when a tool is installed, not when it is called. That is
// deliberate: a manifest problem should be a build-time error with a message
// naming the tool, not a surprise inside a host function three surfaces later.
//
// Three checks exist because of specific, verified sharp edges rather than
// general caution; each says so at its own site.
package manifest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/core"
)

// Limits on a manifest, applied before anything is trusted. A manifest arrives
// from guest code running in a sandbox, so it is attacker-controlled input.
const (
	// MaxSize caps the JSON a describe call may return.
	MaxSize = 256 << 10
	// MaxOps caps operations per tool.
	MaxOps = 64
	// MaxLabels caps labels per tool.
	MaxLabels = 32
	// MaxNameLen caps a tool or operation name.
	MaxNameLen = 64
	// MaxSummaryLen caps a one-line summary.
	MaxSummaryLen = 200
	// MaxDescriptionLen caps the long description.
	MaxDescriptionLen = 16 << 10
)

// nameRE is the charset for tool, operation and label names.
//
// Names must survive being a CLI subcommand, an MCP tool name, a URL path
// segment, a protobuf identifier after mangling, and a filesystem path in the
// store. The intersection of those is narrow, and it is much cheaper to be
// strict here than to discover on the gRPC surface that a name cannot be
// expressed.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Error is a manifest problem, naming the tool and the field at fault.
type Error struct {
	Tool  string
	Field string
	Msg   string
}

func (e *Error) Error() string {
	switch {
	case e.Tool == "" && e.Field == "":
		return "manifest: " + e.Msg
	case e.Field == "":
		return fmt.Sprintf("manifest for %q: %s", e.Tool, e.Msg)
	default:
		return fmt.Sprintf("manifest for %q: %s: %s", e.Tool, e.Field, e.Msg)
	}
}

func errf(tool, field, format string, args ...any) error {
	return &Error{Tool: tool, Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Loaded is a validated manifest plus everything derived from it once, so that
// no surface has to re-resolve a schema or re-canonicalise it.
type Loaded struct {
	Spec core.Spec

	// Resolved is keyed by operation name, ready for Validate and ApplyDefaults.
	Resolved map[string]*jsonschema.Resolved

	// CanonJSON is the canonical serialisation of each operation's input
	// schema. It is the reference every surface's advertised schema is compared
	// against: byte-identical, or the surfaces disagree about what the tool
	// accepts even when they all appear to work.
	CanonJSON map[string][]byte
}

// Parse validates raw manifest JSON and prepares everything derived from it.
func Parse(raw []byte) (*Loaded, error) {
	if len(raw) == 0 {
		return nil, errf("", "", "empty")
	}
	if len(raw) > MaxSize {
		return nil, errf("", "", "too large: %d bytes, limit %d", len(raw), MaxSize)
	}

	var spec core.Spec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		// An unknown field is usually a tool built against a newer forge, so
		// say that rather than only quoting the JSON error.
		return nil, errf("", "", "cannot decode (a tool built for a newer forge?): %v", err)
	}
	return Validate(spec)
}

// Validate checks a spec and prepares its derived data.
func Validate(spec core.Spec) (*Loaded, error) {
	if spec.ABI == 0 {
		return nil, errf(spec.Name, "abi", "missing; the tool did not report an ABI version")
	}
	if spec.ABI != core.ABICurrent {
		return nil, errf(spec.Name, "abi", "version %d, but this forge speaks %d", spec.ABI, core.ABICurrent)
	}
	if !nameRE.MatchString(spec.Name) {
		return nil, errf(spec.Name, "name", "must match %s", nameRE)
	}
	if len(spec.Name) > MaxNameLen {
		return nil, errf(spec.Name, "name", "longer than %d characters", MaxNameLen)
	}
	if len(spec.Summary) > MaxSummaryLen {
		return nil, errf(spec.Name, "summary", "longer than %d characters", MaxSummaryLen)
	}
	if len(spec.Description) > MaxDescriptionLen {
		return nil, errf(spec.Name, "description", "longer than %d characters", MaxDescriptionLen)
	}
	if len(spec.Labels) > MaxLabels {
		return nil, errf(spec.Name, "labels", "%d labels, limit %d", len(spec.Labels), MaxLabels)
	}
	seenLabel := map[string]bool{}
	for _, l := range spec.Labels {
		if !nameRE.MatchString(l) {
			return nil, errf(spec.Name, "labels", "%q must match %s", l, nameRE)
		}
		if seenLabel[l] {
			return nil, errf(spec.Name, "labels", "%q appears twice", l)
		}
		seenLabel[l] = true
	}

	for _, r := range spec.Requires {
		if !r.Kind.Valid() {
			// Not ignored: a typo'd capability must not read as "wants nothing".
			return nil, errf(spec.Name, "requires", "unknown capability %q", r.Kind)
		}
		if len(r.Scope) == 0 {
			return nil, errf(spec.Name, "requires", "capability %s has no scope; use [\"*\"] to ask for all", r.Kind)
		}
	}

	if len(spec.Ops) == 0 {
		// The likeliest cause by far, so lead with it: in a reactor build main()
		// never runs, so registering from there leaves the manifest empty.
		return nil, errf(spec.Name, "ops", "no operations; call tool.Register from init(), not main()")
	}
	if len(spec.Ops) > MaxOps {
		return nil, errf(spec.Name, "ops", "%d operations, limit %d", len(spec.Ops), MaxOps)
	}

	l := &Loaded{
		Spec:      spec,
		Resolved:  make(map[string]*jsonschema.Resolved, len(spec.Ops)),
		CanonJSON: make(map[string][]byte, len(spec.Ops)),
	}

	seenOp := map[string]bool{}
	for _, op := range spec.Ops {
		if !nameRE.MatchString(op.Name) {
			return nil, errf(spec.Name, "ops", "operation name %q must match %s", op.Name, nameRE)
		}
		if seenOp[op.Name] {
			return nil, errf(spec.Name, "ops", "operation %q declared twice", op.Name)
		}
		seenOp[op.Name] = true

		if len(op.Summary) > MaxSummaryLen {
			return nil, errf(spec.Name, "ops."+op.Name, "summary longer than %d characters", MaxSummaryLen)
		}
		if !op.OutputKind.Valid() {
			return nil, errf(spec.Name, "ops."+op.Name, "unknown outputKind %q", op.OutputKind)
		}
		if op.OutputKind == core.OutputBytes && op.OutputMediaType == "" {
			// REST needs a Content-Type and MCP must choose between an image,
			// audio and an embedded resource block. Neither can guess.
			return nil, errf(spec.Name, "ops."+op.Name, "outputKind %q requires outputMediaType", core.OutputBytes)
		}

		res, canon, err := prepareInput(spec.Name, op)
		if err != nil {
			return nil, err
		}
		l.Resolved[op.Name] = res
		l.CanonJSON[op.Name] = canon
	}
	return l, nil
}

// prepareInput validates one operation's input schema and derives its resolved
// form and canonical bytes.
func prepareInput(tool string, op core.OpSpec) (*jsonschema.Resolved, []byte, error) {
	field := "ops." + op.Name + ".input"
	if op.Input == nil {
		return nil, nil, errf(tool, field, "missing")
	}
	// Must be an object. Every surface binds named parameters, and the MCP
	// SDK's AddTool panics outright on a nil or non-object schema -- which
	// would take down the whole server for one bad tool, so it is caught here.
	if op.Input.Type != "object" {
		return nil, nil, errf(tool, field, "must be a schema of type \"object\", got %q", op.Input.Type)
	}
	if err := rejectRefs(tool, field, op.Input); err != nil {
		return nil, nil, err
	}

	// ValidateDefaults, because a default that does not satisfy its own schema
	// produces a value forge would then hand to a tool as if it were valid.
	res, err := op.Input.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		return nil, nil, errf(tool, field, "%v", err)
	}

	canon, err := canonical(op.Input)
	if err != nil {
		return nil, nil, errf(tool, field, "cannot canonicalise: %v", err)
	}
	return res, canon, nil
}

// rejectRefs refuses $ref and $defs anywhere in a manifest schema.
//
// This is narrower than JSON Schema allows, and it is on purpose. jsonschema-go's
// ApplyDefaults does not follow $ref, so a schema using one would get its
// defaults applied on some paths and not others -- a divergence that shows up as
// a tool behaving differently depending on which surface called it, which is the
// exact class of bug forge is built to avoid. A tool author who needs shared
// sub-schemas can inline them; the SDK generates inlined schemas already.
func rejectRefs(tool, field string, s *jsonschema.Schema) error {
	var walk func(path string, s *jsonschema.Schema) error
	walk = func(path string, s *jsonschema.Schema) error {
		if s == nil {
			return nil
		}
		if s.Ref != "" {
			return errf(tool, field, "%s uses $ref, which forge does not accept: defaults are not applied through references, so the same input would behave differently on different surfaces; inline the schema instead", path)
		}
		if len(s.Defs) > 0 {
			return errf(tool, field, "%s uses $defs; inline the definitions instead", path)
		}
		for name, p := range s.Properties {
			if err := walk(path+"."+name, p); err != nil {
				return err
			}
		}
		for _, sub := range []struct {
			name string
			s    *jsonschema.Schema
		}{
			{"items", s.Items},
			{"additionalProperties", s.AdditionalProperties},
			{"not", s.Not},
			{"if", s.If},
			{"then", s.Then},
			{"else", s.Else},
		} {
			if err := walk(path+"."+sub.name, sub.s); err != nil {
				return err
			}
		}
		for _, group := range []struct {
			name string
			ss   []*jsonschema.Schema
		}{
			{"allOf", s.AllOf},
			{"anyOf", s.AnyOf},
			{"oneOf", s.OneOf},
			{"prefixItems", s.PrefixItems},
		} {
			for i, sub := range group.ss {
				if err := walk(fmt.Sprintf("%s.%s[%d]", path, group.name, i), sub); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("", s)
}

// canonical serialises a schema with object keys sorted, so two schemas that
// mean the same thing produce the same bytes. Surfaces are compared on these
// bytes, so the ordering has to be deterministic rather than merely stable
// within one process.
func canonical(s *jsonschema.Schema) ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	// encoding/json sorts map keys when marshalling, so a round trip through
	// map[string]any is enough to canonicalise ordering.
	return json.Marshal(v)
}

// Requests returns the capability requests, which is what the consent prompt
// shows the user.
func (l *Loaded) Requests() []capability.Request { return l.Spec.Requires }
