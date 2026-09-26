// Command jsonfmt is a forge tool for reshaping JSON.
//
// It declares no capabilities at all: it is a pure function from text to text,
// so forge never has to ask permission for it and it can reach nothing on the
// machine it runs on.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type FormatArgs struct {
	Data   string `json:"data" jsonschema:"the JSON text to format"`
	Indent int    `json:"indent,omitempty" jsonschema:"spaces per level, default 2"`
}

type MinifyArgs struct {
	Data string `json:"data" jsonschema:"the JSON text to compact"`
}

type CanonArgs struct {
	Data string `json:"data" jsonschema:"the JSON text to canonicalise"`
}

var _ = tool.Register(
	tool.Spec{
		Name:    "jsonfmt",
		Version: "0.1.0",
		Summary: "Format, compact and canonicalise JSON",
		Description: "Reshapes JSON text. Needs nothing from the machine it runs on: " +
			"no filesystem, no network, no secrets.",
		Labels: []string{"json", "text"},
	},
	tool.Op("format", format, tool.Text(), tool.ReadOnly(), tool.Idempotent(),
		tool.Summary("Pretty-print JSON with an indent")),
	tool.Op("minify", minify, tool.Text(), tool.ReadOnly(), tool.Idempotent(),
		tool.Summary("Strip all insignificant whitespace")),
	tool.Op("canon", canon, tool.Text(), tool.ReadOnly(), tool.Idempotent(),
		tool.Summary("Sort every object's keys, so two equivalent documents compare equal")),
)

func main() {}

func format(ctx *tool.Context, a FormatArgs) (string, error) {
	if a.Indent <= 0 {
		a.Indent = 2
	}
	if a.Indent > 16 {
		return "", fmt.Errorf("indent %d is more than 16 spaces", a.Indent)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(a.Data), "", strings.Repeat(" ", a.Indent)); err != nil {
		return "", fmt.Errorf("not valid JSON: %w", err)
	}
	return buf.String(), nil
}

func minify(ctx *tool.Context, a MinifyArgs) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(a.Data)); err != nil {
		return "", fmt.Errorf("not valid JSON: %w", err)
	}
	return buf.String(), nil
}

// canon re-encodes through map[string]any, which sorts object keys, so two
// documents that mean the same thing produce the same bytes.
//
// Numbers are decoded with UseNumber so that large integers keep their digits:
// canonicalising a document must not quietly change what is in it.
func canon(ctx *tool.Context, a CanonArgs) (string, error) {
	dec := json.NewDecoder(strings.NewReader(a.Data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("not valid JSON: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
