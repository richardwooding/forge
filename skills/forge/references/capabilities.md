# Capabilities

What each capability grants, what it refuses, and the rules that are not
obvious from the name.

## Contents

- [The eight kinds](#the-eight-kinds)
- [Scope syntax](#scope-syntax)
- [Network: what "allow this host" does not mean](#network-what-allow-this-host-does-not-mean)
- [Secrets](#secrets)
- [Key/value](#keyvalue)
- [Tool to tool](#tool-to-tool)
- [Deny codes](#deny-codes)

## The eight kinds

| Kind | Constant | Scope means | Granted access |
|---|---|---|---|
| `fs.read` | `tool.FSRead` | a directory | read under it, through an `os.Root` jail |
| `fs.write` | `tool.FSWrite` | a directory | create and modify under it |
| `net.http` | `tool.NetHTTP` | host patterns | HTTP through forge's client |
| `env` | `tool.Env` | variable names | those variables only |
| `secret` | `tool.Secret` | secret names | those secrets, one read at a time, audited |
| `clock.wall` | `tool.ClockWall` | — | the real wall clock |
| `kv` | `tool.KV` | namespace names | a private key/value namespace |
| `tool.invoke` | `tool.Invoke` | tool names | calling those tools |

Without `clock.wall` a tool sees a frozen clock (not a broken one), which
silently breaks date logic and cache expiry. Grant it if the tool reasons about
time. A monotonic clock and randomness are always available and never gated.

## Scope syntax

Scope is a list of subjects, matched per kind:

- Filesystem kinds match by path segment. A grant for `/a/b` covers `/a/b/c`,
  never `/a/bc`.
- `net.http` matches hosts. `*.example.com` covers `api.example.com` but **not**
  bare `example.com` — list both if you mean both. A grant names a host, not a
  port.
- `env`, `secret`, `kv` and `tool.invoke` match names exactly.
- `*` means everything of that kind. Prefer naming subjects; the reason string
  is what earns a yes, and `*` makes it harder to give one.

## Network: what "allow this host" does not mean

Granting `net.http` for a host is not a decision to make any request to it.
forge additionally, and unconditionally:

- **Screens addresses in the dialler**, so the address that was checked is the
  address dialled. Checking a name and letting the transport resolve it again
  is the window DNS rebinding goes through.
- **Refuses loopback, private ranges and link-local.** Link-local stays refused
  even when `FORGE_ALLOW_PRIVATE_NETWORK=1` opens the rest for local
  development — that range is where cloud credentials live.
- **Does not follow redirects.** A 3xx comes back with its `Location`. To
  follow it, request that URL, which puts it through the allowlist on its own
  merits. Walk chains yourself and bound the hops.
- **Refuses plain http** unless `http` was granted; https only otherwise.
- **Refuses guest-set `Authorization`, `Cookie`, `Host` and `Proxy-*`.**
  Credentials belong in a secret, where they are named and each read is logged.
- **Sends no proxy.** An inherited `HTTP_PROXY` would route around the
  screening.
- **Truncates large bodies**, setting `Truncated` on the response. Check it
  before parsing, or half a document gets parsed as though it were whole.

A wasip1 guest has no socket calls at all, so there is nothing for a tool to
bypass this with.

## Secrets

`tool.GetSecret(name)` returns `(value, found, error)`; `tool.MustGetSecret`
turns a missing one into an error naming it. Secrets are files under forge's
config directory, mode 0600, managed with `forge secret set|ls|rm`.

They are deliberately **not** environment variables: the environment is
readable by everything in a process, cannot be audited per access, and would
appear in the guest's own `os.Environ()` — handing a tool every secret the
moment it was granted one.

**A tool holding both `secret` and `net.http` can send what it reads wherever
it may reach.** No sandbox prevents that; it is what both capabilities are for.
forge shows the pair together at approval time. When writing such a tool, say
plainly in the `Reason` where data goes.

## Key/value

`tool.OpenKV(ns)` returns a `*Store` with `Get`/`Set`/`Delete`/`List` plus
`GetString`/`SetString`/`GetJSON`/`SetJSON`. Values persist between
invocations and across surfaces. A namespace belongs to one tool: two tools
using the name `notes` do not see each other's values.

`Get` returns `(value, found, error)` so an empty value and a missing key stay
distinguishable. Keys may contain any characters — they are encoded, never used
as paths.

## Tool to tool

`tool.Call(name, op, input, &out)` runs another tool in a separate instance.
The callee is attenuated to the intersection of its own grants and the
caller's, so it can never do something the caller could not. It shares the
caller's deadline and budget, and a tool already on the call path is refused,
so cycles fail at the call rather than by exhausting depth.

## Deny codes

`tool.Denied(err)` yields a `*tool.DeniedError` with `Capability`, `Subject`,
`Reason` and `Code`:

| Code | Meaning | What the user should do |
|---|---|---|
| `tool.DenyNoGrant` | no grant of this kind at all | grant it |
| `tool.DenyOutOfScope` | granted, but not for this subject | widen the scope |
| `tool.DenyBudget` | allowed, allowance spent | do less per request |
| `tool.DenyFloor` | forge refuses outright | nothing — do not suggest a grant |

Distinguishing these is the difference between useful advice and sending
someone to change a setting that was never the problem.
