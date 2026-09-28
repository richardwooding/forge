#!/usr/bin/env bash
#
# A Claude Code PreToolUse hook that mentions the forge tool for a job at the
# moment the shell command for it is about to run.
#
# Why this exists. forge ships three channels that tell an agent to prefer a
# tool -- the MCP server instructions, each tool's own UseWhen, and a CLAUDE.md
# snippet -- and measurement says they move discovery and not initiation. In a
# session immediately after all three shipped, with the rules in context, the
# count was 63 shell calls and 0 tool calls, including a release verified with
# `sha256sum` by the same author who had written "never hand-roll a checksum
# comparison" into the instructions.
#
# Prose in a system prompt competes with a habit and loses, because the habit
# does not involve a decision. This is the only channel that arrives at the
# moment of the habit rather than before it.
#
# It never blocks. It prints one note per session per tool and gets out of the
# way -- a hook that interrupted every curl would be turned off within a day,
# and a hook that is off nudges nobody.
#
# Install by pointing a PreToolUse hook on Bash at this file:
#
#     "hooks": {
#       "PreToolUse": [{
#         "matcher": "Bash",
#         "hooks": [{"type": "command", "command": "~/.claude/hooks/forge-nudge.sh"}]
#       }]
#     }

set -uo pipefail

# Every exit is 0. A hook that fails takes the Bash call down with it, and
# nothing here is worth failing a command over.
trap 'exit 0' ERR

command -v jq >/dev/null 2>&1 || exit 0
command -v forge >/dev/null 2>&1 || exit 0

payload=$(cat)
cmd=$(printf '%s' "$payload" | jq -r '.tool_input.command // empty' 2>/dev/null) || exit 0
[ -n "$cmd" ] || exit 0
session=$(printf '%s' "$payload" | jq -r '.session_id // "unknown"' 2>/dev/null) || session=unknown

# A binary counts only where a command name can appear: the start of the line,
# or after a pipe, semicolon, &&, || or an opening parenthesis. Without this a
# URL containing "curl" or a file called checksums-sha256sum.txt would fire it,
# and a nudge that cries wolf is one nobody reads.
at_command_position() {
	printf '%s' "$cmd" | grep -Eqi "(^|[;&|(]|&&|\|\|)[[:space:]]*(sudo[[:space:]]+)?($1)([[:space:]]|$)"
}

suggest=""
note=""
case 1 in
1)
	if at_command_position 'sha256sum|sha512sum|sha1sum|md5sum|shasum'; then
		suggest=hashsum
		note="forge has hashsum for this. \`forge hashsum verify --path FILE --checksums checksums.txt\` does the whole comparison in one call, including the case a shell pipeline turns into an empty pipe: the file is not listed in the manifest at all. Needs fs.read over the directory -- forge grant allow hashsum --scope fs.read=DIR."
	elif at_command_position 'curl|wget'; then
		suggest=fetch
		note="forge has fetch (get/head/json) and urlcheck for this, sandboxed and SSRF-checked. Worth it when the response matters to the task; not worth it for a one-off download you are about to delete."
	fi
	;;
esac

[ -n "$suggest" ] || exit 0

# One note per session per tool, not per session overall -- otherwise the first
# curl of the day silences the checksum nudge for good.
state="${XDG_RUNTIME_DIR:-/tmp}/claude-forge-nudge/${session}"
mkdir -p "$state" 2>/dev/null || exit 0
marker="${state}/${suggest}"
[ -e "$marker" ] && exit 0
: >"$marker" 2>/dev/null || exit 0

jq -nc --arg note "$note" '{
	hookSpecificOutput: {
		hookEventName: "PreToolUse",
		additionalContext: $note
	}
}'
exit 0
