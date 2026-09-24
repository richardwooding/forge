package binding

import (
	"errors"
	"fmt"
)

// FaultCode classifies a failure that is forge's to report, as opposed to a
// failure the tool itself reported.
//
// # The parity contract
//
// Every surface maps from this table and none invents its own mapping. The
// conformance suite asserts every row, so a surface that drifts fails the build.
//
//	Condition                     Code               CLI  MCP                      REST  gRPC
//	----------------------------  -----------------  ---  -----------------------  ----  ------------------
//	success                       -                    0  CallToolResult            200  OK
//	tool ran and reported failure  - (ToolError)        1  CallToolResult{IsError}   422  OK + is_error
//	input fails schema validation FaultInvalidInput    2  InvalidParams             400  InvalidArgument
//	usage error (unknown flag)    FaultInvalidInput    2  n/a                       400  n/a
//	tool not found                FaultNotFound        3  InvalidParams             404  NotFound
//	tool exists, out of view      FaultNotInView       3  InvalidParams             404  NotFound
//	runtime failure (wasm trap)   FaultInternal        4  internal error            500  Internal
//	timeout                       FaultDeadline        4  internal error            504  DeadlineExceeded
//	over concurrency limit        FaultExhausted       4  internal error            429  ResourceExhausted
//	SIGINT                        -                  130  -                           -  Canceled
//
// Two rows carry the weight. A tool that ran and reported failure is NOT a
// Fault: MCP requires it as a normal result with IsError so the model can see
// and correct it, and the same reasoning gives REST a 422 and gRPC an OK with
// is_error rather than a status that hides the payload where most clients never
// look. And FaultNotInView answers NotFound rather than PermissionDenied, so a
// narrow view does not leak the existence of the tools it hides -- the CLI is
// the deliberate exception, because there the caller is a local human who
// benefits from being told the tool exists and how to widen the view.
type FaultCode string

const (
	FaultInvalidInput FaultCode = "invalid_input"
	FaultNotFound     FaultCode = "not_found"
	FaultNotInView    FaultCode = "not_in_view"
	FaultInternal     FaultCode = "internal"
	FaultDeadline     FaultCode = "deadline_exceeded"
	FaultExhausted    FaultCode = "exhausted"
)

// ExitCode is the process exit status the CLI uses for this fault. It is a
// method on the code so the CLI cannot invent its own mapping.
func (c FaultCode) ExitCode() int {
	switch c {
	case FaultInvalidInput:
		return 2
	case FaultNotFound, FaultNotInView:
		return 3
	case FaultInternal, FaultDeadline, FaultExhausted:
		return 4
	}
	return 1
}

// HTTPStatus is the REST status for this fault.
func (c FaultCode) HTTPStatus() int {
	switch c {
	case FaultInvalidInput:
		return 400
	case FaultNotFound, FaultNotInView:
		return 404
	case FaultInternal:
		return 500
	case FaultDeadline:
		return 504
	case FaultExhausted:
		return 429
	}
	return 500
}

// Violation locates one input problem. JSON Pointer rather than a dotted path,
// because it is the only notation REST, MCP and the CLI can all agree on for a
// value inside a document.
type Violation struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

// Fault is the single failure type every surface renders from. Message is the
// same text everywhere; only the envelope differs.
type Fault struct {
	Code       FaultCode   `json:"code"`
	Message    string      `json:"message"`
	Tool       string      `json:"tool,omitempty"`
	Op         string      `json:"op,omitempty"`
	Violations []Violation `json:"violations,omitempty"`

	Cause error `json:"-"`
}

func (f *Fault) Error() string {
	if f.Tool == "" {
		return f.Message
	}
	return f.Tool + ": " + f.Message
}

func (f *Fault) Unwrap() error { return f.Cause }

// AsFault extracts a Fault from an error chain, reporting whether one was there.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	ok := errors.As(err, &f)
	return f, ok
}

func faultf(code FaultCode, format string, args ...any) *Fault {
	return &Fault{Code: code, Message: fmt.Sprintf(format, args...)}
}
