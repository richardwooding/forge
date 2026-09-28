---
name: forge
description: Use a sandboxed forge tool instead of a shell pipeline, and write new ones in Go compiled to WebAssembly. Use when you are about to hash or checksum something, fetch a URL, or keep state between runs; when you are writing a one-liner you have written before; when a task needs to remember what it saw last time; or when the prompt mentions forge, a forge tool, `forge tool add`, forge_add_tool, or wasm/wasip1. Covers discovery with forge_search_tools, the single-file tool shape registered from a package-level var, declaring capabilities (net.http, kv, secret, tool.invoke) with reasons a person will read, handling a *tool.DeniedError by its code, and when NOT to build a tool.
---

# forge

forge compiles a Go program to WebAssembly and exposes it as a command on five
surfaces at once — CLI, REPL, MCP, REST and gRPC. A tool gets only the
capabilities it declared and the user approved, and views filter which tools
each surface shows.

Two habits matter, in this order.

## 1. Reach for an installed tool before writing a pipeline

Installed forge tools appear as `mcp__forge__<tool>_<op>` (or
`mcp__forge__<tool>` when the tool has one operation). Prefer one over `jq`,
`python3 -c` or a shell pipeline for work it already covers — that is the
whole point of having installed it.

The tool list in context shows only the **active view**. Tools outside it still
exist and are still callable:

- `forge_search_tools` — find a tool by query or labels. Returns names,
  summaries and labels only, never schemas, so discovery is cheap.
- `forge_describe_tool` — get one tool's full schema once you know you want it.

Two round trips beats carrying fifty schemas you will not use.

## 2. A recurring one-liner is a candidate tool — sometimes

If you are about to write the same `sha256sum | cut -d' ' -f1` for the third
time, that is a tool. Writing one is cheap and it becomes available everywhere
at once.

### When not to build one

This is the part that is easy to get wrong, and it was measured rather than
guessed. Over a full day of work with these tools installed, the ones actually
reached for were all doing something the caller could not: hashing, fetching,
remembering. A tool that only reshapes data already in the caller's context was
used **once**, because reading that data directly is cheaper than a round trip.

So build a tool when the work:

- **cannot** be done in context — the network, the filesystem, state between
  runs, a secret;
- **should not** be done by eye — a hash, a checksum, a canonical form, anything
  where being approximately right is being wrong;
- would **cost more** in context than the call — processing something large and
  returning a small answer.

Do not build one to reformat, extract or restructure data the caller can
already see. It will compete with the caller and lose.

**Prefer a tool that needs nothing** when the shape above still fits: a tool
declaring no capabilities never prompts and can reach nothing.

### The shape

```go
package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
    Data string `json:"data" jsonschema:"the text to work on"`
}
type Out struct {
    Result string `json:"result"`
}

// Registered from a package-level var, NOT from main.
var _ = tool.Register(
    tool.Spec{
        Name:    "mytool",
        Version: "0.1.0",
        Summary: "One line, shown in listings",
        Labels:  []string{"text"},
    },
    tool.Op("run", run, tool.Summary("What this operation does"), tool.ReadOnly()),
)

func main() {}

func run(ctx *tool.Context, a Args) (Out, error) {
    return Out{Result: a.Data}, nil
}
```

**`main()` never runs.** forge builds with `-buildmode=c-shared`, which produces
a WASI *reactor*: package `init` runs and the exports are callable, but `main`
is never called. Registering from inside `func main` yields a tool that
describes as empty. The `var _ = tool.Register(...)` form is what makes that
mistake awkward to write.

Input schemas come from the Go type. Use `json` tags for names and `jsonschema`
tags for descriptions — the struct is the single source of truth, so there is
no separate schema to drift.

### Installing it

Via MCP, when the server runs with `--allow-install`:

`forge_add_tool` takes **one Go file** as `source`. Re-installing the same tool
name replaces it in place, so iterating is one more call.

Compile-check before you call it, so the approval prompt is only ever for
source that builds:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= GOWORK=off \
  go build -buildmode=c-shared -o /dev/null ./
```

That needs a `go.mod` requiring `github.com/richardwooding/forge/sdk`.

From a terminal instead: `forge tool add ./dir` — which also accepts a
multi-file package, unlike the MCP route.

**Approval covers installation, not compilation.** forge builds the source
before it asks, so declining still ran the Go toolchain over it, unsandboxed.
Treat `forge_add_tool` as the same trust decision as `go build` on a stranger's
repository.

## Capabilities

A tool that needs something from the machine declares it, with a reason the
person deciding will actually read:

```go
Needs: []tool.Need{{
    Kind:   tool.NetHTTP,
    Scope:  []string{"api.github.com"},
    Reason: "look up the pull requests you ask about",
}},
```

Capabilities are declared per **tool**, not per operation, so one operation
needing the network makes the whole tool prompt. Split a tool if that matters.

Guest calls: `tool.HTTP` / `tool.Get` / `tool.GetJSON`, `tool.OpenKV(ns)`,
`tool.GetSecret(name)`, `tool.Call(tool, op, in, &out)`.

Those names are not `KV`, `Secret` and `Invoke` — those identifiers are the
capability *constants* used in `Needs`.

Handle refusals by code rather than treating every failure as fatal:

```go
if d, ok := tool.Denied(err); ok {
    switch d.Code {
    case tool.DenyNoGrant, tool.DenyOutOfScope: // a grant would fix it
    case tool.DenyBudget:                       // do less
    case tool.DenyFloor:                        // nothing will fix it; do not suggest a grant
    }
}
```

`DenyFloor` is the one that catches people out: forge refuses loopback,
private ranges and link-local outright, so advising a user to widen a grant
after a `DenyFloor` sends them to change a setting that cannot help.

For what each capability grants and the full network rules, see
[references/capabilities.md](references/capabilities.md).

For labels, views, import/export and the other surfaces, see
[references/surfaces.md](references/surfaces.md).

## Checks worth running

- `forge ls` — what is installed, with capability badges
- `forge grant ls` — what each tool was actually allowed
- `forge info <tool>` — schema, digest, grants
- `forge doctor` — toolchain, wasm engine tier, cache and store health
