package binding

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/richardwooding/forge/internal/core"
)

// Rendition is a tool's result in a form no surface has interpreted yet.
//
// It is the second half of the waist: a surface's only jobs are to produce
// json.RawMessage going in and to render one of these coming out. Keeping the
// result in a neutral shape until the last moment is what stops the CLI, MCP
// and REST from each deciding separately what a tool "really" returned.
type Rendition struct {
	Kind core.OutputKind

	JSON      json.RawMessage
	Text      string
	Bytes     []byte
	MediaType string

	// ToolError reports that the tool ran and failed. This is not a Fault:
	// the tool worked as designed and is telling the caller something. Message
	// carries what it said.
	ToolError bool
	Message   string
}

// String renders the result for a terminal.
//
// JSON is indented because a human is reading it; a surface that needs the
// compact form uses JSON directly. Binary is described rather than printed,
// since writing arbitrary bytes to a terminal can leave it in a state the user
// has to reset.
func (r Rendition) String() string {
	switch {
	case r.ToolError:
		return r.Message
	case r.Kind == core.OutputText:
		return r.Text
	case r.Kind == core.OutputBytes:
		return fmt.Sprintf("<%d bytes of %s>", len(r.Bytes), r.mediaType())
	case len(r.JSON) == 0:
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, r.JSON, "", "  "); err != nil {
		return string(r.JSON)
	}
	return buf.String()
}

// Raw returns the bytes a caller should write when piping the result onward,
// and whether they are safe to print to a terminal.
//
// The two differ for exactly one reason: binary output piped to a file is the
// point of the Bytes kind, and the same bytes written to a terminal are not.
func (r Rendition) Raw() (data []byte, printable bool) {
	switch r.Kind {
	case core.OutputText:
		return []byte(r.Text), true
	case core.OutputBytes:
		return r.Bytes, false
	default:
		return r.JSON, true
	}
}

func (r Rendition) mediaType() string {
	if r.MediaType == "" {
		return "application/octet-stream"
	}
	return r.MediaType
}
