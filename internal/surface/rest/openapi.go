package rest

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/store"
)

// openAPI serves a document describing exactly the tools this view exposes.
//
// Generated from the manifests rather than derived from Go types by a
// framework. forge already has the schemas -- they came from the tool itself
// and are the same bytes MCP advertises -- so a framework's reflection would
// be a second source of truth to keep in step, which is the failure this whole
// project is arranged to avoid.
//
// Per view, because that is what makes it useful beyond an agent: a client
// generated from this document gets a narrow, stable API rather than every
// tool anyone has ever installed.
func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) {
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

	title := "forge"
	if v := viewOf(r); v != "" {
		title = "forge (" + v + ")"
	}

	doc := map[string]any{
		// 3.1, because it aligns JSON Schema with the real thing. forge's
		// schemas come from jsonschema-go and go in verbatim; under 3.0 they
		// would have to be down-converted, and a down-conversion is a place
		// for the advertised schema to stop matching the enforced one.
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   title,
			"version": "0.1.0",
			"description": "Tools compiled to WebAssembly and sandboxed by the capabilities they " +
				"declare. This document describes one view; other views are served at " +
				"/v1/views/{view}/openapi.json.",
		},
		"paths": s.paths(records, basePath(r)),
		"components": map[string]any{
			"schemas": map[string]any{"Problem": problemSchema()},
		},
	}
	if s.opts.BaseURL != "" {
		doc["servers"] = []any{map[string]any{"URL": s.opts.BaseURL}}
	}

	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) paths(records []store.Record, base string) map[string]any {
	paths := map[string]any{}

	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			b, err := s.opts.Toolkit.Bound(rec.Spec.Name, op.Name)
			if err != nil {
				// A tool that cannot be bound is left out rather than
				// described wrongly: a generated client built from a schema
				// forge cannot honour is worse than one tool missing.
				continue
			}
			paths[base+"/tools/"+b.Name+"/invoke"] = map[string]any{
				"post": s.operation(rec, b),
			}
		}
	}
	return paths
}

func (s *Server) operation(rec store.Record, b *binding.Bound) map[string]any {
	summary := b.Op.Summary
	if summary == "" {
		summary = rec.Spec.Summary
	}

	description := rec.Spec.Description
	if reqs := rec.Spec.Requires; len(reqs) > 0 {
		// Capabilities belong in the description for the same reason they
		// belong in the MCP one: whoever is choosing between tools should be
		// able to see that this one reaches the network.
		var kinds []string
		for _, req := range reqs {
			kinds = append(kinds, string(req.Kind))
		}
		sort.Strings(kinds)
		if description != "" {
			description += "\n\n"
		}
		description += "Requires: " + join(kinds, ", ") + "."
	}

	return map[string]any{
		"operationId": b.Name,
		"summary":     summary,
		"description": description,
		"tags":        rec.Labels(),
		"requestBody": map[string]any{
			"required": true,
			"content": map[string]any{
				"application/json": map[string]any{
					// The canonical bytes, unmodified. This is the same schema
					// MCP advertises and the same one Normalize enforces.
					"schema": json.RawMessage(b.CanonJSON),
				},
			},
		},
		"responses": responses(b.Op),
	}
}

func responses(op core.OpSpec) map[string]any {
	var ok map[string]any
	switch op.OutputKind {
	case core.OutputText:
		ok = content("text/plain", map[string]any{"type": "string"})
	case core.OutputBytes:
		media := op.OutputMediaType
		if media == "" {
			media = "application/octet-stream"
		}
		ok = content(media, map[string]any{"type": "string", "format": "binary"})
	default:
		schema := any(map[string]any{})
		if op.Output != nil {
			if raw, err := json.Marshal(op.Output); err == nil {
				schema = json.RawMessage(raw)
			}
		}
		ok = content("application/json", schema)
	}
	ok["description"] = "the tool ran and produced a result"

	problemRef := content("application/problem+json",
		map[string]any{"$ref": "#/components/schemas/Problem"})

	return map[string]any{
		"200": ok,
		"400": withDescription(problemRef, "the input did not match the tool's schema"),
		"404": withDescription(clone(problemRef), "no such tool in this view"),
		// 422 rather than 500: the request was understood and what it asked
		// for could not be done. The distinction is the parity table's, so it
		// reads the same here as an IsError result does over MCP.
		"422": withDescription(clone(problemRef), "the tool ran and reported a failure"),
		"500": withDescription(clone(problemRef), "forge could not run the tool"),
		"504": withDescription(clone(problemRef), "the tool exceeded its time limit"),
	}
}

func content(media string, schema any) map[string]any {
	return map[string]any{
		"content": map[string]any{media: map[string]any{"schema": schema}},
	}
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	maps.Copy(out, m)
	return out
}

func withDescription(m map[string]any, d string) map[string]any {
	m["description"] = d
	return m
}

func problemSchema() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "An RFC 9457 problem document.",
		"properties": map[string]any{
			"type":   map[string]any{"type": "string"},
			"title":  map[string]any{"type": "string"},
			"status": map[string]any{"type": "integer"},
			"detail": map[string]any{"type": "string"},
			"tool":   map[string]any{"type": "string"},
			"op":     map[string]any{"type": "string"},
			"view":   map[string]any{"type": "string"},
			"violations": map[string]any{
				"type":        "array",
				"description": "Where the input went wrong, by JSON Pointer.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pointer": map[string]any{"type": "string"},
						"message": map[string]any{"type": "string"},
					},
				},
			},
		},
		"required": []string{"type", "title", "status"},
	}
}

func join(ss []string, sep string) string {
	var out strings.Builder
	for i, s := range ss {
		if i > 0 {
			out.WriteString(sep)
		}
		out.WriteString(s)
	}
	return out.String()
}
