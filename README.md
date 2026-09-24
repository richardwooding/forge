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

Early. The wasm runtime, capability model and filesystem jail are being built
first; the CLI and MCP surfaces are the v0.1 target.

## Security

`forge tool add` compiles Go source on your machine with the ordinary toolchain.
That is a trust decision equivalent to running `go build` on a stranger's
repository: **the wasm sandbox protects invocation, not installation.** Only add
tools whose source you would have been willing to build anyway.

## Licence

MIT
