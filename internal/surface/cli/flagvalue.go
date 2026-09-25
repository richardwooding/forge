package cli

import (
	"strings"

	"github.com/spf13/pflag"

	"github.com/richardwooding/forge/internal/binding"
)

// schemaValue is a flag that holds whatever the user typed and reports the
// schema's type in help.
//
// It exists to reconcile two things that pull in opposite directions. Help text
// should say `--times int`, because `--times string` for an integer field is
// simply misinformation. But parsing and validation must happen in Normalize,
// once, so that the CLI and MCP report the same mistake in the same words --
// which rules out pflag's typed flags, whose own errors would arrive first and
// differently.
//
// So the value is carried as text and only its declared Type is borrowed from
// the schema. pflag prints the type; binding does the work.
type schemaValue struct {
	raw  string
	kind binding.FlagKind
}

func (v *schemaValue) String() string { return v.raw }

func (v *schemaValue) Set(s string) error {
	v.raw = s
	return nil
}

// Type is what pflag shows in help text.
func (v *schemaValue) Type() string {
	switch v.kind {
	case binding.FlagInteger:
		return "int"
	case binding.FlagNumber:
		return "number"
	default:
		return "string"
	}
}

var _ pflag.Value = (*schemaValue)(nil)

// schemaArray is the repeatable form.
//
// It implements pflag.SliceValue so that repeated occurrences accumulate, and
// deliberately does not split on commas: pflag's own StringSlice does, which
// silently corrupts any value containing one.
type schemaArray struct {
	values []string
	kind   binding.FlagKind
}

func (v *schemaArray) String() string {
	// Empty renders as empty, not as "[]". pflag captures this at registration
	// to decide whether to print "(default ...)" in help, and an unset
	// repeatable flag has no default worth announcing.
	if len(v.values) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("[")
	for i, s := range v.values {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString(s)
	}
	return out.String() + "]"
}

func (v *schemaArray) Set(s string) error {
	v.values = append(v.values, s)
	return nil
}

func (v *schemaArray) Type() string {
	switch v.kind {
	case binding.FlagInteger:
		return "ints"
	case binding.FlagNumber:
		return "numbers"
	default:
		return "strings"
	}
}

func (v *schemaArray) Append(s string) error { v.values = append(v.values, s); return nil }
func (v *schemaArray) Replace(s []string) error {
	v.values = append([]string(nil), s...)
	return nil
}
func (v *schemaArray) GetSlice() []string { return v.values }

var _ pflag.SliceValue = (*schemaArray)(nil)

// TypeName is the type forge shows for a flag, so that help and `forge info`
// cannot disagree about what a flag takes.
func TypeName(kind binding.FlagKind, repeatable bool) string {
	if repeatable {
		return (&schemaArray{kind: kind}).Type()
	}
	if kind == binding.FlagBool {
		return "bool"
	}
	return (&schemaValue{kind: kind}).Type()
}
