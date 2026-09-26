// Package conformance checks that forge's surfaces agree.
//
// The invariant, stated precisely:
//
//	For every tool T, input D, and surfaces S1 and S2:
//	  canon(outcome(S1, T, D)) == canon(outcome(S2, T, D))
//	and the input schema each surface advertises for T is byte-identical.
//
// Two clauses there are easy to skip and are the reason this exists. The
// failure cases are covered as well as the successes, because a surface that
// reports a bad input differently from its neighbours is as broken as one that
// computes a different answer. And the schema clause catches the nastiest
// drift of all: every surface working, and the four of them disagreeing about
// what the tool accepts.
//
// Every driver goes through its real transport -- a cobra tree, an MCP client
// over the SDK's own plumbing, an httptest server, a bufconn dialler -- rather
// than calling handlers directly. A harness that skipped the transport would
// prove only that the handlers agree, which was never in doubt.
package conformance

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// Class is what happened, coarsely enough to compare across surfaces that
// express failure in different vocabularies.
//
// Deliberately coarse. Surfaces report a bad input as exit code 2, as a
// JSON-RPC InvalidParams, as HTTP 400 and as gRPC InvalidArgument; demanding
// they produce the same words would be testing the protocols, not forge. What
// must agree is which of these buckets an outcome lands in.
type Class string

const (
	// ClassOK is a tool that ran and produced a result.
	ClassOK Class = "ok"

	// ClassToolError is a tool that ran and reported failure. Distinct from
	// ClassFailed on purpose: it is the row of the parity table most likely to
	// be got wrong, because it looks like an error and is not one.
	ClassToolError Class = "tool_error"

	// ClassInvalidInput is input the schema rejected.
	ClassInvalidInput Class = "invalid_input"

	// ClassNotFound is no such tool, including a tool hidden by a view.
	ClassNotFound Class = "not_found"

	// ClassFailed is forge unable to run the tool.
	ClassFailed Class = "failed"
)

// Outcome is one invocation, reduced to what every surface can express.
type Outcome struct {
	Class Class

	// Output is the result, canonicalised: JSON with its keys sorted, text as
	// itself, binary as its bytes. Surfaces frame results differently -- a
	// content block, a response body, a protobuf field -- and the framing is
	// not what has to agree.
	Output string

	// Message is the tool's own words when Class is ClassToolError. Compared
	// because a surface that swallows it leaves the caller with nothing to act
	// on.
	Message string
}

// Driver is one surface, driven through its real transport.
type Driver interface {
	// Name identifies the surface in a failure message.
	Name() string

	// List returns the tool names this surface exposes, sorted.
	List(ctx context.Context) ([]string, error)

	// Describe returns the canonical input schema this surface advertises.
	Describe(ctx context.Context, name string) ([]byte, error)

	// Invoke runs a tool with a JSON input document.
	Invoke(ctx context.Context, name string, input json.RawMessage) (Outcome, error)
}

// canonJSON sorts object keys so two equivalent documents compare equal.
//
// Decoded with json.Number so that a large integer is compared by its digits.
// Without it this helper would itself round 2^53+1 and quietly agree that the
// surfaces agreed.
func canonJSON(raw []byte) string {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}
