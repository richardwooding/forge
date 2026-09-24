package binding

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// FlagKind is the value type a flag carries. It is deliberately coarser than
// JSON Schema: the CLI only needs to know how to parse the string a user typed.
type FlagKind string

const (
	FlagString  FlagKind = "string"
	FlagBool    FlagKind = "bool"
	FlagInteger FlagKind = "integer"
	FlagNumber  FlagKind = "number"
)

// Flag is one command-line parameter derived from an input schema.
//
// Note what is NOT here: nothing applies a default, and nothing enforces
// required-ness. Both are done once, in Normalize, for every surface. A flag
// that carried its own default would mean two defaulting implementations that
// must agree; a flag marked required would make the CLI report a missing
// parameter in cobra's words while MCP reports it in the validator's, for the
// identical mistake.
type Flag struct {
	// Name is the dotted path a user types: "db.host". Dotted rather than
	// hyphenated because it is reversible -- a hyphen is legal inside a
	// property name, so "db-host" could not be split back unambiguously.
	Name string

	// Pointer is the JSON Pointer to the value: "/db/host".
	Pointer string

	Kind       FlagKind
	Repeatable bool

	// Enum, when set, is the complete list of accepted values, used to drive
	// shell completion.
	Enum []string

	// DefaultDisplay is the schema's default rendered for help text only. It is
	// never applied here.
	DefaultDisplay string

	// Required is for help text only, for the same reason.
	Required bool

	Usage string
}

// NotExpressible records a part of the schema that flags cannot represent.
//
// It is kept rather than dropped because silence would be the worst outcome: a
// user would type flags, see no error, and get a document missing the field
// they thought they had set. The CLI shows these in help and tells the user to
// use --input-json or --set-json.
type NotExpressible struct {
	Pointer string
	Reason  string
}

// FlagSet is everything derived from one operation's input schema.
type FlagSet struct {
	Flags          []Flag
	NotExpressible []NotExpressible
}

// Flag looks up a flag by its dotted name.
func (fs FlagSet) Flag(name string) (Flag, bool) {
	for _, f := range fs.Flags {
		if f.Name == name {
			return f, true
		}
	}
	return Flag{}, false
}

// Complete reports whether every part of the schema is expressible as flags.
func (fs FlagSet) Complete() bool { return len(fs.NotExpressible) == 0 }

// BuildFlags derives the flag set for an input schema, which must be an object
// schema (manifest validation guarantees that).
func BuildFlags(s *jsonschema.Schema) FlagSet {
	var fs FlagSet
	walkObject(&fs, s, "", "")
	sort.Slice(fs.Flags, func(i, j int) bool { return fs.Flags[i].Name < fs.Flags[j].Name })
	sort.Slice(fs.NotExpressible, func(i, j int) bool { return fs.NotExpressible[i].Pointer < fs.NotExpressible[j].Pointer })
	return fs
}

func walkObject(fs *FlagSet, s *jsonschema.Schema, namePrefix, ptrPrefix string) {
	if s == nil {
		return
	}
	if reason := combinatorReason(s); reason != "" {
		fs.NotExpressible = append(fs.NotExpressible, NotExpressible{Pointer: ptr(ptrPrefix), Reason: reason})
		return
	}
	// An object that accepts arbitrary extra properties cannot be enumerated as
	// flags: the set of names is not known until the user supplies them.
	if s.AdditionalProperties != nil && !isFalseSchema(s.AdditionalProperties) {
		fs.NotExpressible = append(fs.NotExpressible, NotExpressible{
			Pointer: ptr(ptrPrefix),
			Reason:  "accepts additional properties, whose names are not known in advance",
		})
	}
	if len(s.PatternProperties) > 0 {
		fs.NotExpressible = append(fs.NotExpressible, NotExpressible{
			Pointer: ptr(ptrPrefix),
			Reason:  "uses patternProperties, whose names are not known in advance",
		})
	}

	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}

	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		prop := s.Properties[name]
		childName := name
		if namePrefix != "" {
			childName = namePrefix + "." + name
		}
		childPtr := ptrPrefix + "/" + escapePointer(name)
		walkValue(fs, prop, childName, childPtr, required[name])
	}
}

func walkValue(fs *FlagSet, s *jsonschema.Schema, name, pointer string, required bool) {
	if s == nil {
		return
	}
	if reason := combinatorReason(s); reason != "" {
		fs.NotExpressible = append(fs.NotExpressible, NotExpressible{Pointer: pointer, Reason: reason})
		return
	}

	switch s.Type {
	case "object":
		walkObject(fs, s, name, pointer)
	case "array":
		item := s.Items
		if item == nil || len(s.PrefixItems) > 0 {
			fs.NotExpressible = append(fs.NotExpressible, NotExpressible{
				Pointer: pointer,
				Reason:  "an array with tuple or unconstrained items cannot be a repeatable flag",
			})
			return
		}
		kind, ok := scalarKind(item.Type)
		if !ok {
			fs.NotExpressible = append(fs.NotExpressible, NotExpressible{
				Pointer: pointer,
				Reason:  fmt.Sprintf("an array of %q cannot be a repeatable flag", itemTypeName(item)),
			})
			return
		}
		fs.Flags = append(fs.Flags, Flag{
			Name: name, Pointer: pointer, Kind: kind, Repeatable: true,
			Enum: enumStrings(item), DefaultDisplay: defaultDisplay(s),
			Required: required, Usage: s.Description,
		})
	case "":
		// No type at all: the value could be anything, so no flag can parse it.
		fs.NotExpressible = append(fs.NotExpressible, NotExpressible{
			Pointer: pointer,
			Reason:  "no declared type",
		})
	default:
		kind, ok := scalarKind(s.Type)
		if !ok {
			fs.NotExpressible = append(fs.NotExpressible, NotExpressible{
				Pointer: pointer,
				Reason:  fmt.Sprintf("type %q has no flag representation", s.Type),
			})
			return
		}
		fs.Flags = append(fs.Flags, Flag{
			Name: name, Pointer: pointer, Kind: kind,
			Enum: enumStrings(s), DefaultDisplay: defaultDisplay(s),
			Required: required, Usage: s.Description,
		})
	}
}

// combinatorReason names the keyword that makes a schema inexpressible as
// flags, or "" when there is none. These are the constructs where one value can
// satisfy several shapes, and a flag has only one parse.
func combinatorReason(s *jsonschema.Schema) string {
	switch {
	case len(s.Types) > 0:
		return "several possible types"
	case len(s.OneOf) > 0:
		return "oneOf: the accepted shape depends on the value"
	case len(s.AnyOf) > 0:
		return "anyOf: the accepted shape depends on the value"
	case len(s.AllOf) > 0:
		return "allOf: the accepted shape is a composition"
	case s.Not != nil:
		return "not: the accepted shape is defined by exclusion"
	case s.If != nil || s.Then != nil || s.Else != nil:
		return "if/then/else: the accepted shape is conditional"
	}
	return ""
}

func scalarKind(t string) (FlagKind, bool) {
	switch t {
	case "string":
		return FlagString, true
	case "boolean":
		return FlagBool, true
	case "integer":
		return FlagInteger, true
	case "number":
		return FlagNumber, true
	}
	return "", false
}

func itemTypeName(s *jsonschema.Schema) string {
	if s.Type != "" {
		return s.Type
	}
	return "untyped values"
}

func isFalseSchema(s *jsonschema.Schema) bool {
	// jsonschema-go models the boolean schema `false` as a schema that permits
	// nothing; the marshalled form is the reliable test.
	b, err := json.Marshal(s)
	return err == nil && string(b) == "false"
}

func enumStrings(s *jsonschema.Schema) []string {
	if len(s.Enum) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		// Quote strings are noise on a command line.
		out = append(out, strings.Trim(string(b), `"`))
	}
	return out
}

func defaultDisplay(s *jsonschema.Schema) string {
	if len(s.Default) == 0 {
		return ""
	}
	return strings.Trim(string(s.Default), `"`)
}

// escapePointer applies RFC 6901 escaping.
func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

func ptr(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// Document builds an input document from the flags the user actually changed.
//
// values is keyed by dotted flag name; a flag the user did not set must be
// absent, NOT present with a zero value. That distinction is the whole reason
// defaults are applied later: forge cannot tell "--count 0" from "did not say"
// if the CLI has already substituted a zero.
func (fs FlagSet) Document(values map[string][]string) (json.RawMessage, *Fault) {
	root := map[string]any{}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		f, ok := fs.Flag(name)
		if !ok {
			return nil, faultf(FaultInvalidInput, "unknown flag --%s", name)
		}
		v, fault := f.parse(values[name])
		if fault != nil {
			return nil, fault
		}
		if err := setAtPointer(root, f.Pointer, v); err != nil {
			return nil, faultf(FaultInvalidInput, "--%s: %v", name, err)
		}
	}
	b, err := json.Marshal(root)
	if err != nil {
		return nil, faultf(FaultInternal, "cannot encode input: %v", err)
	}
	return b, nil
}

// parse converts the raw strings a user typed into a JSON value.
func (f Flag) parse(raw []string) (any, *Fault) {
	if f.Repeatable {
		out := make([]any, 0, len(raw))
		for _, r := range raw {
			v, fault := f.parseOne(r)
			if fault != nil {
				return nil, fault
			}
			out = append(out, v)
		}
		return out, nil
	}
	if len(raw) == 0 {
		return nil, faultf(FaultInvalidInput, "--%s: no value", f.Name)
	}
	// Last wins, matching every command-line tool a user has ever met.
	return f.parseOne(raw[len(raw)-1])
}

func (f Flag) parseOne(raw string) (any, *Fault) {
	switch f.Kind {
	case FlagString:
		return raw, nil
	case FlagBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, faultf(FaultInvalidInput, "--%s: %q is not a boolean", f.Name, raw)
		}
		return b, nil
	case FlagInteger:
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return nil, faultf(FaultInvalidInput, "--%s: %q is not an integer", f.Name, raw)
		}
		// json.Number, not int64: it survives marshalling without becoming a
		// float, which is what keeps values past 2^53 intact end to end.
		return json.Number(raw), nil
	case FlagNumber:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return nil, faultf(FaultInvalidInput, "--%s: %q is not a number", f.Name, raw)
		}
		return json.Number(raw), nil
	}
	return nil, faultf(FaultInternal, "--%s: unknown flag kind %q", f.Name, f.Kind)
}

// setAtPointer writes v at a JSON Pointer, creating intermediate objects.
func setAtPointer(root map[string]any, pointer string, v any) error {
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	cur := root
	for i, part := range parts {
		key := unescapePointer(part)
		if i == len(parts)-1 {
			cur[key] = v
			return nil
		}
		next, ok := cur[key]
		if !ok {
			m := map[string]any{}
			cur[key] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot set %s: %q is already a value", pointer, key)
		}
		cur = m
	}
	return nil
}

// Flatten is Document's inverse: it turns an input document into the flag
// values that would produce it.
//
// It has a second use beyond testing. The REPL renders a filled-in form back as
// the command line the user could have typed, so that what they see is always
// something they could have entered themselves -- and that rendering must be
// the exact inverse of the parse, or the two paths diverge.
//
// unexpressible lists pointers whose value no flag can carry. Callers must not
// treat an empty values map as "nothing to set": an unexpressible document
// needs --input-json instead.
func (fs FlagSet) Flatten(doc map[string]any) (values map[string][]string, unexpressible []string) {
	values = map[string][]string{}
	byPointer := make(map[string]Flag, len(fs.Flags))
	for _, f := range fs.Flags {
		byPointer[f.Pointer] = f
	}

	var walk func(v any, pointer string)
	walk = func(v any, pointer string) {
		if f, ok := byPointer[pointer]; ok {
			if strs, ok := flagStrings(f, v); ok {
				values[f.Name] = strs
			} else {
				unexpressible = append(unexpressible, ptr(pointer))
			}
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			unexpressible = append(unexpressible, ptr(pointer))
			return
		}
		if len(m) == 0 {
			// An empty object has no leaf to hang a flag on, so no sequence of
			// flags reproduces it. Saying so is the honest answer; silently
			// returning nothing would make a round trip look lossless when it
			// is not.
			unexpressible = append(unexpressible, ptr(pointer))
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walk(m[k], pointer+"/"+escapePointer(k))
		}
	}

	walk(doc, "")
	sort.Strings(unexpressible)
	return values, unexpressible
}

// flagStrings renders a JSON value as the strings a user would have typed.
func flagStrings(f Flag, v any) ([]string, bool) {
	if f.Repeatable {
		items, ok := v.([]any)
		if !ok {
			return nil, false
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := scalarString(f.Kind, item)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		// An empty array is not expressible: repeating a flag zero times is
		// indistinguishable from not setting it at all.
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	s, ok := scalarString(f.Kind, v)
	if !ok {
		return nil, false
	}
	return []string{s}, true
}

func scalarString(kind FlagKind, v any) (string, bool) {
	switch kind {
	case FlagString:
		s, ok := v.(string)
		return s, ok
	case FlagBool:
		b, ok := v.(bool)
		if !ok {
			return "", false
		}
		return strconv.FormatBool(b), true
	case FlagInteger, FlagNumber:
		switch n := v.(type) {
		case json.Number:
			return n.String(), true
		case float64:
			return strconv.FormatFloat(n, 'g', -1, 64), true
		}
		return "", false
	}
	return "", false
}
