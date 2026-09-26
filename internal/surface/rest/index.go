package rest

import (
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/richardwooding/forge/internal/binding"
)

// index is a plain HTML listing of what this view exposes.
//
// Server-rendered, with no JavaScript and nothing fetched from a CDN. A
// documentation bundle would be a megabyte of vendored third-party script
// shipped inside a binary whose whole argument is that it sandboxes the code
// it runs -- and for a localhost multitool, a list of tools with their schemas
// and a link to the raw document is what anyone actually wants. The OpenAPI
// document is there for the cases where a real viewer or a code generator is
// warranted.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeFault(w, &binding.Fault{Code: binding.FaultNotFound,
			Message: "no such path: " + r.URL.Path}, r)
		return
	}

	sel, _, err := s.selectorFor(r)
	if err != nil {
		sel = s.opts.Default
	}
	records, err := s.opts.Toolkit.List(sel)
	if err != nil {
		writeFault(w, &binding.Fault{Code: binding.FaultInternal, Message: err.Error()}, r)
		return
	}

	var b strings.Builder
	b.WriteString(pageHead)

	if len(records) == 0 {
		b.WriteString(`<p class="empty">No tools are installed yet. Try <code>forge tool add ./mytool</code>.</p>`)
	}

	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			bd, err := s.opts.Toolkit.Bound(rec.Spec.Name, op.Name)
			if err != nil {
				continue
			}
			summary := op.Summary
			if summary == "" {
				summary = rec.Spec.Summary
			}
			fmt.Fprintf(&b, `<section><h2>%s</h2><p>%s</p>`,
				html.EscapeString(bd.Name), html.EscapeString(summary))

			if l := rec.Labels(); len(l) > 0 {
				b.WriteString(`<p class="labels">`)
				for _, label := range l {
					fmt.Fprintf(&b, `<span class="chip">%s</span>`, html.EscapeString(label))
				}
				b.WriteString(`</p>`)
			}
			if reqs := rec.Spec.Requires; len(reqs) > 0 {
				b.WriteString(`<p class="wants">Requires: `)
				for i, req := range reqs {
					if i > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "%s (%s)", html.EscapeString(string(req.Kind)),
						html.EscapeString(strings.Join(req.Scope, ", ")))
				}
				b.WriteString(`</p>`)
			}

			fmt.Fprintf(&b, `<pre><code>POST %s/tools/%s/invoke</code></pre>`,
				html.EscapeString(basePath(r)), html.EscapeString(bd.Name))
			fmt.Fprintf(&b, `<details><summary>input schema</summary><pre><code>%s</code></pre></details>`,
				html.EscapeString(indentJSON(bd.CanonJSON)))
			b.WriteString(`</section>`)
		}
	}

	fmt.Fprintf(&b, `<p class="foot"><a href="%s/openapi.json">openapi.json</a></p></body></html>`,
		html.EscapeString(basePath(r)))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// pageHead carries the gloam palette inline, in both light and dark, so the
// page matches forge's terminal output without fetching anything.
const pageHead = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>forge</title>
<style>
:root { color-scheme: light dark;
  --bg:#fff; --panel:#f6f8fa; --border:#d8dee4; --fg:#1f2328; --muted:#59636e; --accent:#7c3aed; }
@media (prefers-color-scheme: dark) { :root {
  --bg:#0d1117; --panel:#161b22; --border:#2a3038; --fg:#e6edf3; --muted:#9aa7b4; --accent:#a371f7; } }
body { background:var(--bg); color:var(--fg); margin:0 auto; padding:2rem 1rem; max-width:52rem;
  font:15px/1.6 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; }
h1 { color:var(--accent); font-size:1.4rem; margin:0 0 .25rem; }
h2 { color:var(--accent); font-size:1rem; margin:0 0 .25rem; }
section { border:1px solid var(--border); border-radius:10px; padding:1rem; margin:1rem 0;
  background:var(--panel); }
p { margin:.25rem 0; } .muted,.foot,.labels,.wants { color:var(--muted); font-size:.9rem; }
.chip { border:1px solid var(--border); border-radius:999px; padding:0 .5rem; margin-right:.3rem; }
pre { overflow-x:auto; background:var(--bg); border:1px solid var(--border); border-radius:8px;
  padding:.6rem; margin:.5rem 0; }
code { font:inherit; } summary { cursor:pointer; color:var(--muted); }
a { color:var(--accent); }
</style></head><body>
<h1>forge</h1>
<p class="muted">Tools compiled to WebAssembly, sandboxed by the capabilities they declare.</p>
`
