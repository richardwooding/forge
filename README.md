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

`forge mcp --allow-install` goes a step further: it adds `forge_add_tool`, so an
agent can compile and install a tool without leaving MCP. Nothing reaches the
store until a human approves it through an MCP elicitation naming the tool and
what it wants — and if the connected client cannot be asked, forge refuses
rather than treating silence as a yes. It is off by default; see Security below
for the trust decision it does not remove.

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

## The surfaces agree, and that is checked

Four surfaces exposing the same tools is only useful if they behave the same
way. `internal/conformance` drives all of them — a cobra tree, an MCP client
over the SDK's own plumbing, an httptest server, a bufconn dialler — through
one adversarial corpus and asserts:

- each exposes the same tools;
- each advertises a **byte-identical** input schema;
- each produces the same outcome for the same input, failures included;
- a tool's own error message survives everywhere;
- a view hides the same tools everywhere;
- an integer past 2^53 survives exactly, end to end.

The failure clause matters as much as the success one, and the schema clause
catches the nastiest drift of all: every surface working, and the four
disagreeing about what the tool accepts.

Writing it found two real bugs immediately. The MCP surface decoded results
into an `any` before sending them, which silently rounded every integer past
2^53; and the REPL's dispatcher swallowed the difference between a tool that
failed and one that succeeded.

## Security

`forge tool add` compiles Go source on your machine with the ordinary toolchain.
That is a trust decision equivalent to running `go build` on a stranger's
repository: **the wasm sandbox protects invocation, not installation.** Only add
tools whose source you would have been willing to build anyway.

`forge mcp --allow-install` makes the same trust decision on an agent's say-so
instead of a typed command. The human-in-the-loop elicitation it requires
guards whether a tool is kept and made callable — it cannot undo the fact that
building it already ran the Go toolchain, unsandboxed, over the source.

## Licence

MIT

## The other surfaces

```console
$ forge serve                       # a unix socket by default
  REST  /v1/tools          OpenAPI /v1/openapi.json
  MCP   /mcp               gRPC    forge.v1.ToolService
```

Everything is served per view, so `/v1/views/dev/openapi.json` describes only
the tools in `dev` and a client generated from it stays inside that view.

```console
$ curl -s --unix-socket ~/.run/forge/forge.sock \
    -X POST -d '{"data":"{\"b\":2,\"a\":1}"}' \
    http://localhost/v1/tools/jsonfmt_canon/invoke
{"a":1,"b":2}

$ grpcurl -plaintext 127.0.0.1:7777 forge.v1.ToolService/ListTools
```

One service for every tool rather than one generated per tool: gRPC refuses
`RegisterService` after `Serve` has begun, so a tool installed while the server
is running could never be given a service of its own.

### Regenerating the protobuf code

```console
$ go install github.com/bufbuild/buf/cmd/buf@latest
$ go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
$ go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
$ buf lint && buf generate
```

The generated code is committed so `go get` works without any of that. CI
regenerates and fails on a diff, so it cannot drift from the `.proto`.
