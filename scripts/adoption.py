#!/usr/bin/env python3
"""Report how often a Claude Code session reached for a forge tool.

forge's premise is that a useful helper should be reachable. Whether that is
working is an empirical question, and the answer turned out to be surprising:
measured over a full day of building forge itself, with the skill installed and
a note saying to prefer these tools, the session made 969 Bash calls and about
eight genuine tool calls. Everything about adoption -- the skill's trigger, the
MCP descriptions, the server instructions -- was rewritten off the back of that
number, so it needs to stay measurable.

    scripts/adoption.py ~/.claude/projects/<project>/<session>.jsonl

Read-only. It parses the transcript and prints counts; it changes nothing.
"""

import collections
import json
import re
import sys

# Shell that a forge tool could plausibly have served. Deliberately narrow:
# most Bash in a coding session is git, builds and file edits, and counting
# those as missed opportunities would flatter the numbers in the wrong
# direction.
ADDRESSABLE = {
    "hash or checksum": r"\b(sha\d*sum|md5sum|shasum|openssl\s+dgst)\b",
    "fetch a URL": r"\b(curl|wget)\b",
    "JSON handling": r"(jq\s|python3?\s+-c.*json)",
    "base64": r"\bbase64\b",
}


def tool_calls(path):
    """Yield (name, input) for every tool call in a transcript."""
    with open(path, encoding="utf-8") as f:
        for line in f:
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            content = (entry.get("message") or {}).get("content")
            if not isinstance(content, list):
                continue
            for block in content:
                if isinstance(block, dict) and block.get("type") == "tool_use":
                    yield block.get("name", "?"), block.get("input") or {}


def main(argv):
    if len(argv) != 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2

    names = collections.Counter()
    forge = collections.Counter()
    addressable = collections.Counter()
    bash = 0

    for name, inp in tool_calls(argv[1]):
        names[name] += 1
        if name.startswith("mcp__forge__"):
            forge[name] += 1
        if name == "Bash":
            bash += 1
            command = inp.get("command", "")
            for label, pattern in ADDRESSABLE.items():
                if re.search(pattern, command):
                    addressable[label] += 1

    total = sum(names.values())
    print(f"tool calls: {total}   Bash: {bash}   forge: {sum(forge.values())}")

    if forge:
        print("\nforge tools used:")
        for name, count in forge.most_common():
            print(f"  {count:4}  {name.removeprefix('mcp__forge__')}")

    if addressable:
        print("\nshell a tool might have served:")
        for label, count in addressable.most_common():
            print(f"  {count:4}  {label}")

    # The skill is model-invoked, so whether it ever fired is a direct read on
    # its trigger text -- and on the day this was written, it never did.
    print(f"\nforge skill invoked: {names.get('Skill', 0)} time(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
