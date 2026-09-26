package rest

import (
	"encoding/json"
	"net/http"

	"github.com/richardwooding/forge/internal/binding"
)

// problem is an RFC 9457 problem document.
//
// 9457 rather than 7807: it obsoletes it, and the media type is the same, so
// there is no cost to naming the current one. The extension members are what
// make it worth using at all -- a client gets the tool, the operation, and a
// JSON Pointer per violation, rather than a sentence it has to parse.
type problem struct {
	Type       string              `json:"type"`
	Title      string              `json:"title"`
	Status     int                 `json:"status"`
	Detail     string              `json:"detail,omitempty"`
	Instance   string              `json:"instance,omitempty"`
	Tool       string              `json:"tool,omitempty"`
	Op         string              `json:"op,omitempty"`
	View       string              `json:"view,omitempty"`
	Violations []binding.Violation `json:"violations,omitempty"`
}

// faultSlug names the error type URI for a fault code. The codes come from
// binding's parity table, so the URIs cannot drift from what the other
// surfaces report.
func faultSlug(c binding.FaultCode) (slug, title string) {
	switch c {
	case binding.FaultInvalidInput:
		return "invalid-input", "the input did not match the tool's schema"
	case binding.FaultNotFound:
		return "not-found", "no such tool"
	case binding.FaultNotInView:
		return "not-found", "no such tool"
	case binding.FaultDeadline:
		return "timeout", "the tool exceeded its time limit"
	case binding.FaultExhausted:
		return "busy", "too many tools running at once"
	default:
		return "internal", "forge could not run the tool"
	}
}

func writeFault(w http.ResponseWriter, f *binding.Fault, r *http.Request) {
	slug, title := faultSlug(f.Code)
	status := f.Code.HTTPStatus()
	if status == http.StatusTooManyRequests {
		// Nothing here knows how long the queue is, so no number is offered
		// rather than an invented one.
		w.Header().Set("Retry-After", "1")
	}
	writeProblem(w, status, problem{
		Type:       problemType(slug),
		Title:      title,
		Status:     status,
		Detail:     f.Message,
		Tool:       f.Tool,
		Op:         f.Op,
		View:       viewOf(r),
		Violations: f.Violations,
	}, r)
}

func writeProblem(w http.ResponseWriter, status int, p problem, r *http.Request) {
	if p.View == "" {
		p.View = viewOf(r)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(p)
}
