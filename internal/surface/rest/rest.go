// Package rest serves forge's tools over HTTP.
//
// There is no router generated per tool and no mux mutated at runtime. Go's
// ServeMux cannot unregister a pattern, so any design that maps tools to
// routes one-to-one is wrong the first time a tool is removed. Instead one
// catch-all pattern matches every invocation and the lookup happens in the
// handler, which means a tool installed a second ago is callable now, with
// nothing to register and nothing to tear down.
package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/policy"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"
)

// maxBody caps an invocation's request body. A tool's input is parameters,
// not a payload; anything larger is a mistake or an attempt to exhaust memory.
const maxBody = 8 << 20

// Options configure the surface.
type Options struct {
	Toolkit *toolkit.Toolkit

	// Views resolves a named view from the path. Nil means every request gets
	// Default.
	Views *view.Store

	// Default is the selector for requests that name no view.
	Default labels.Selector

	// BaseURL is what the generated OpenAPI document advertises as its server.
	BaseURL string
}

// Server is the REST surface.
type Server struct{ opts Options }

// New returns a server.
func New(opts Options) *Server {
	if opts.Default == nil {
		opts.Default = labels.All
	}
	return &Server{opts: opts}
}

// Handler returns the routes.
//
// The view is a path segment rather than a header or a query parameter so that
// a client can be configured once with a URL and never think about it again --
// and so that a generated client, which is the point of publishing OpenAPI at
// all, is generated against one view's tools rather than against everything.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/views/{view}/tools", s.listTools)
	mux.HandleFunc("GET /v1/views/{view}/tools/{name}", s.describeTool)
	mux.HandleFunc("POST /v1/views/{view}/tools/{name}/invoke", s.invoke)
	mux.HandleFunc("GET /v1/views/{view}/openapi.json", s.openAPI)

	// The default view, for a caller that has not set one up.
	mux.HandleFunc("GET /v1/tools", s.listTools)
	mux.HandleFunc("GET /v1/tools/{name}", s.describeTool)
	mux.HandleFunc("POST /v1/tools/{name}/invoke", s.invoke)
	mux.HandleFunc("GET /v1/openapi.json", s.openAPI)

	mux.HandleFunc("GET /", s.index)
	return mux
}

// selectorFor resolves the view named in the path.
func (s *Server) selectorFor(r *http.Request) (labels.Selector, string, error) {
	name := r.PathValue("view")
	if name == "" || s.opts.Views == nil {
		return s.opts.Default, "", nil
	}
	sel, resolved, err := s.opts.Views.Resolve(name, "")
	if err != nil {
		// An unknown view serves nothing rather than everything. A typo in a
		// client's base URL must not silently widen what is exposed.
		return labels.None, name, err
	}
	return sel, resolved, nil
}

// bound finds the operation a request names, honouring the view.
func (s *Server) bound(r *http.Request) (*binding.Bound, store.Record, *binding.Fault) {
	sel, viewName, err := s.selectorFor(r)
	if err != nil {
		return nil, store.Record{}, &binding.Fault{
			Code:    binding.FaultNotFound,
			Message: fmt.Sprintf("no view named %q", viewName),
		}
	}

	want := r.PathValue("name")
	records, err := s.opts.Toolkit.List(sel)
	if err != nil {
		return nil, store.Record{}, &binding.Fault{Code: binding.FaultInternal, Message: err.Error()}
	}
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			name := binding.SurfaceName(rec.Spec.Name, op.Name, len(rec.Spec.Ops) == 1)
			if name != want {
				continue
			}
			b, err := s.opts.Toolkit.Bound(rec.Spec.Name, op.Name)
			if err != nil {
				return nil, rec, &binding.Fault{Code: binding.FaultInternal, Tool: rec.Spec.Name, Message: err.Error()}
			}
			return b, rec, nil
		}
	}
	// Out of view answers not-found, like every surface but the CLI: a narrow
	// view must not leak the existence of what it hides.
	return nil, store.Record{}, &binding.Fault{
		Code:    binding.FaultNotFound,
		Message: fmt.Sprintf("no tool named %q", want),
	}
}

type toolSummary struct {
	Name       string   `json:"name"`
	Tool       string   `json:"tool"`
	Op         string   `json:"op"`
	Summary    string   `json:"summary,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	OutputKind string   `json:"outputKind"`
	Requires   []string `json:"requires,omitempty"`
	InvokeURL  string   `json:"invokeUrl"`
}

func (s *Server) listTools(w http.ResponseWriter, r *http.Request) {
	sel, viewName, err := s.selectorFor(r)
	if err != nil {
		writeFault(w, &binding.Fault{Code: binding.FaultNotFound,
			Message: fmt.Sprintf("no view named %q", viewName)}, r)
		return
	}
	records, err := s.opts.Toolkit.List(sel)
	if err != nil {
		writeFault(w, &binding.Fault{Code: binding.FaultInternal, Message: err.Error()}, r)
		return
	}

	out := []toolSummary{}
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			out = append(out, summarise(rec, op, basePath(r)))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

func summarise(rec store.Record, op core.OpSpec, base string) toolSummary {
	name := binding.SurfaceName(rec.Spec.Name, op.Name, len(rec.Spec.Ops) == 1)
	var requires []string
	for _, req := range rec.Spec.Requires {
		requires = append(requires, string(req.Kind))
	}
	summary := op.Summary
	if summary == "" {
		summary = rec.Spec.Summary
	}
	return toolSummary{
		Name:       name,
		Tool:       rec.Spec.Name,
		Op:         op.Name,
		Summary:    summary,
		Labels:     rec.Labels(),
		OutputKind: string(op.OutputKind),
		Requires:   requires,
		InvokeURL:  base + "/tools/" + name + "/invoke",
	}
}

func (s *Server) describeTool(w http.ResponseWriter, r *http.Request) {
	b, rec, fault := s.bound(r)
	if fault != nil {
		writeFault(w, fault, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        b.Name,
		"tool":        rec.Spec.Name,
		"op":          b.Op.Name,
		"summary":     b.Op.Summary,
		"description": rec.Spec.Description,
		"labels":      rec.Labels(),
		"outputKind":  string(b.Op.OutputKind),
		"requires":    rec.Spec.Requires,
		// The canonical bytes, verbatim. Every surface has to advertise the
		// same schema for the same tool, and this is the copy they are all
		// compared against.
		"inputSchema": json.RawMessage(b.CanonJSON),
	})
}

func (s *Server) invoke(w http.ResponseWriter, r *http.Request) {
	b, _, fault := s.bound(r)
	if fault != nil {
		writeFault(w, fault, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeFault(w, &binding.Fault{Code: binding.FaultInvalidInput, Tool: b.Spec.Name,
			Message: "could not read the request body: " + err.Error()}, r)
		return
	}

	res, err := s.opts.Toolkit.Invoke(r.Context(), toolkit.Call{
		Tool:  b.Spec.Name,
		Op:    b.Op.Name,
		Input: body,
	})
	if err != nil {
		writeFault(w, faultFor(err, b), r)
		return
	}

	if res.Rendition.ToolError {
		// The tool ran and reported failure. That is not a server error and
		// not a malformed request: 422 says the request was understood and the
		// thing it asked for could not be done.
		writeProblem(w, http.StatusUnprocessableEntity, problem{
			Type:   problemType("tool-failed"),
			Title:  "the tool reported a failure",
			Status: http.StatusUnprocessableEntity,
			Detail: res.Rendition.Message,
			Tool:   b.Spec.Name,
			Op:     b.Op.Name,
		}, r)
		return
	}
	writeRendition(w, res)
}

// faultFor turns an invocation error into the fault a surface renders.
func faultFor(err error, b *binding.Bound) *binding.Fault {
	if f, ok := binding.AsFault(err); ok {
		return f
	}
	if needs, ok := errors.AsType[*policy.NeedsApproval](err); ok {
		// HTTP has nobody to ask, so this is reported rather than prompted --
		// and, importantly, forge has not recorded a refusal, so approving it
		// from a terminal still works.
		return &binding.Fault{
			Code:    binding.FaultInvalidInput,
			Tool:    needs.Tool,
			Message: needs.Error(),
		}
	}
	return &binding.Fault{Code: binding.FaultInternal, Tool: b.Spec.Name, Op: b.Op.Name, Message: err.Error()}
}

// writeRendition sends a result in the shape the tool declared.
func writeRendition(w http.ResponseWriter, res *toolkit.Result) {
	r := res.Rendition
	if res.Truncated {
		// A header rather than anything in the body: the body is the tool's
		// own bytes and annotating it would corrupt whatever the caller is
		// piping it into. Silence is not an option either -- a client cannot
		// tell a cut-off answer from a whole one.
		w.Header().Set("Forge-Truncated", "true")
	}
	switch r.Kind {
	case core.OutputText:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.Text)
	case core.OutputBytes:
		media := r.MediaType
		if media == "" {
			media = "application/octet-stream"
		}
		w.Header().Set("Content-Type", media)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(r.Bytes)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if len(r.JSON) == 0 {
			_, _ = io.WriteString(w, "null")
			return
		}
		_, _ = w.Write(r.JSON)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// basePath is the prefix a response should use when building URLs, so a
// document served under /v1/views/dev points back into that same view.
func basePath(r *http.Request) string {
	if v := r.PathValue("view"); v != "" {
		return "/v1/views/" + v
	}
	return "/v1"
}

// indentJSON pretty-prints for display, falling back to the original bytes
// rather than showing nothing if it cannot.
func indentJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func problemType(slug string) string {
	return "https://github.com/richardwooding/forge/errors/" + slug
}

func viewOf(r *http.Request) string { return strings.TrimSpace(r.PathValue("view")) }
