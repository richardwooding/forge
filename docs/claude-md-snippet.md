# A CLAUDE.md snippet for forge

Paste this into `~/.claude/CLAUDE.md` (applies everywhere) or a project's
`CLAUDE.md` (applies to one repo). It is short on purpose: it names the moments
to reach for a tool rather than exhorting in general, and it says when *not*
to, which is the half that usually gets left out.

`forge skill install` puts the full skill into a repository. This snippet is
the smaller, always-loaded companion — the skill teaches how to write a tool,
this reminds you to reach for one.

---

```markdown
## forge

Sandboxed tools over MCP (`mcp__forge__*`). Prefer one over a shell pipeline when
the work is **exact** (hashes, checksums, canonical forms), **reaches out** (a URL,
an API), or must be **remembered** between runs. Do not reach for one to reshape
data already in your context — reading it yourself is cheaper.

Writing the same one-liner twice is the signal to make a tool. `forge_add_tool`
takes one Go file and it is live on every surface at once. `forge_search_tools`
finds what the current view hides.
```

---

## Why it is worded like that

Measured over a full day of real work with these tools installed, and a note
already saying to prefer them, a session made 986 Bash calls and about eight
genuine tool calls. "Prefer forge tools" on its own produced roughly one per
cent, so the wording above does two things differently.

**It names moments, not a preference.** "About to hash something" is a thing
you notice yourself doing; "prefer forge tools" is not.

**It says when not to.** The tools that got used were all doing something the
caller could not — hashing, fetching, remembering. A tool that reshapes data
already in context competes with the caller and loses, so telling a model to
reach for one anyway spends its trust on advice that will not pay off.

`scripts/adoption.py` reports the same numbers for any session transcript, if
you want to know whether this is working rather than assume it.
