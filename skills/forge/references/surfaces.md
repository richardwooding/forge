# Surfaces, labels and views

## Contents

- [The five surfaces](#the-five-surfaces)
- [Labels and views](#labels-and-views)
- [Parity: what must be identical](#parity-what-must-be-identical)
- [Output kinds](#output-kinds)
- [Sharing tools](#sharing-tools)
- [Exit codes](#exit-codes)

## The five surfaces

One tool, reachable five ways, with no per-surface work:

| Surface | Command | Notes |
|---|---|---|
| CLI | `forge <tool> --flags` | in-view tools get a subcommand; `forge run <tool>` always works |
| REPL | `forge repl` | a line shell driving the same command tree |
| MCP | `forge mcp` | stdio; `--http` for streamable HTTP at `/mcp/{view}` |
| REST | `forge serve` | `POST /v1/views/{view}/tools/{name}/invoke` |
| gRPC | `forge serve` | `forge.v1.ToolService` |

`forge serve` runs REST, MCP-HTTP and gRPC together, defaulting to a unix
socket. `forge mcp` is separate and starts nothing else, because stdio MCP must
own stdout exclusively — anything a tool prints is captured, never inherited.

## Labels and views

Labels come from the tool's spec plus any you add (`forge label add x fast`).
A **selector** is a boolean expression: `json && !slow`, `git || vcs`.

A **view** is a named, saved selector:

```sh
forge view create dev 'git || json'
forge view use dev
forge ls --view all
```

The active view decides which tools each surface shows. That is the context
saving: fifty installed tools need not cost an agent fifty tools of schema on
every request. Narrow the view, and the MCP tool list narrows with it.

Out-of-view tools remain callable — `forge run <tool> --any`, or
`forge_search_tools` over MCP.

## Parity: what must be identical

For the same tool and input, every surface must produce the same outcome, and
must advertise a byte-identical schema. Surfaces only translate their native
input to JSON and render the result; everything between is one shared path.

The consequence when writing a tool: behaviour that depends on how it was
called is a bug you will only notice on one surface.

## Output kinds

Declared per operation:

- **JSON** (default) — structured; MCP also gets `StructuredContent`
- `tool.Text()` — the handler returns a `string`
- `tool.Bytes(mediaType)` — the handler returns `[]byte`; the media type is
  required, because REST needs a `Content-Type` and MCP needs it to choose
  between an image, audio and an embedded resource

Large outputs become a resource link over MCP rather than being inlined —
a multi-megabyte base64 blob in an agent's context is worse than a round trip.

Annotations are the tool's own claims, useful to a reader and never a reason to
skip approval: `tool.ReadOnly()`, `tool.Destructive()`, `tool.Idempotent()`,
`tool.OpenWorld()`.

## Sharing tools

```sh
forge export --view dev -o dev.forge     # deterministic tar+zstd
forge import dev.forge --on-conflict skip|replace|rename
forge push ghcr.io/you/forge-tools:dev   # any OCI registry
forge pull ghcr.io/you/forge-tools:dev
```

**Bundles carry no grants.** That is the format's security property: what a
tool may do is decided on the machine it runs on, and a replaced tool does not
inherit the grants of the one it replaced.

## Exit codes

| Condition | CLI | MCP | REST | gRPC |
|---|---|---|---|---|
| success | 0 | result | 200 | `OK` |
| tool ran and reported failure | 1 | `IsError: true` | 422 | `OK` + `is_error` |
| input failed validation | 2 | `InvalidParams` | 400 | `InvalidArgument` |
| tool not found / out of view | 3 | `InvalidParams` | 404 | `NotFound` |
| runtime failure or timeout | 4 | internal error | 500 / 504 | `Internal` |

A tool reporting failure is **not** a protocol error: MCP carries it as a
result with `IsError`, so a model can see the message and correct itself.
Out-of-view reports as not-found rather than permission-denied, so the
existence of invisible tools does not leak.
