# forge

**A multitool that builds itself.**

Hand `forge` a Go program; it compiles it to WebAssembly, harvests a manifest from
the module, and the tool is immediately live on five surfaces at once — CLI, REPL,
MCP, OpenAPI/REST and gRPC — with no restart and no code generation.

```console
$ forge tool new gitstat && $EDITOR gitstat/main.go
$ forge tool add ./gitstat
  ⚙ compile   wasip1/wasm           1.9 MB   0.5s
  ⚙ describe  zero capabilities             12ms
  ✓ gitstat  [git,vcs]  caps: —

$ forge gitstat --since 7d          # CLI
$ forge mcp --view git              # your agent's tool list
```

Two properties make it more than a script runner:

**Tools are sandboxed.** They run as wasm under a capability model and get only
what they declared and you approved — a filesystem path, named hosts, named
secrets, or nothing at all. WASI preview 1 has no socket calls, so a tool
physically cannot reach the network except through forge.

**Tools are labelled, and your view of them is filtered.** Narrow the working set
to `git || json` and every surface shrinks with it — above all the MCP tool list,
so fifty installed tools stop costing your agent fifty tools' worth of context on
every request.

## Status

The CLI and MCP surfaces work. REPL, OpenAPI and gRPC are next, and export /
import after that.

```console
$ forge view set dev 'git || json'
$ forge view use dev            # every surface narrows at once
$ forge mcp                     # stdio, for an agent
$ forge mcp --http 127.0.0.1:7777   # /mcp/<view> serves a named view
```

## Writing a tool

A tool is an ordinary Go program that registers itself. forge compiles it,
harvests its manifest by asking the module to describe itself with no
capabilities at all, and installs it.

```go
package main

import "github.com/richardwooding/forge/sdk/tool"

type Args struct {
	Text string `json:"text" jsonschema:"the text to reverse"`
}

var _ = tool.Register(
	tool.Spec{Name: "reverse", Summary: "Reverses text", Labels: []string{"text"}},
	tool.Op("run", run, tool.Text(), tool.ReadOnly()),
)

func main() {} // never called: a reactor module has no entry point

func run(ctx *tool.Context, a Args) (string, error) { ... }
```

```console
$ forge tool add ./reverse
$ forge reverse --text "forge builds itself"
flesti sdliub egrof
```

The input schema comes from the `Args` struct, so the type the handler reads is
the only description of what the tool accepts — there is no second declaration
to drift out of step, and the flags, the MCP `inputSchema` and the validation
all derive from it.

Note `func main() {}`. Tools are built as WASI *reactors*, where `main` is never
executed, which is why `tool.Register` belongs on a package-level variable.

A tool that needs more than stdin and stdout says so:

```go
Needs: []tool.Need{{
	Kind:   tool.NetHTTP,
	Scope:  []string{"api.example.com"},
	Reason: "fetch the pages you ask it to summarise",
}},
```

forge asks you before granting any of it, shows every request in one prompt —
the combination is what carries the risk — and remembers your answer.

## Security

`forge tool add` compiles Go source on your machine with the ordinary toolchain.
That is a trust decision equivalent to running `go build` on a stranger's
repository: **the wasm sandbox protects invocation, not installation.** Only add
tools whose source you would have been willing to build anyway.

## Licence

MIT
