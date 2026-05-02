#!/bin/bash
# @tool: write-note
# @description: Write a note as a text file in the per-session work directory ($SHELL_AGENT_WORK_DIR — same physical location the sandbox sees as /work). Visible in the Data → /work panel immediately. Use register-object afterwards if you want the note to appear in chat as an object:<ID>.
# @param: filename string "Filename only (e.g. hello.txt). Path components are stripped — the note always lands directly in the session work directory."
# @param: content string "Content to write"
# @category: write
# @timeout: 30
#
# Category "write" requires MITL (Man-In-The-Loop) approval before execution.
# Arguments are passed as JSON via stdin. Always sanitize parsed values —
# never pass them to eval or unquoted shell expansion.
#
# Output contract: returns ONLY {status, filename}. Intentionally does
# NOT emit a `next_step` instruction or the absolute host path — see
# docs/en/work-dir-shell-bridge.md §6 for why.

INPUT=$(cat)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json,os; print(os.path.basename(json.load(sys.stdin).get('filename','note.txt')))" 2>/dev/null)
CONTENT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('content',''))" 2>/dev/null)

if [ -z "$FILENAME" ]; then
  echo '{"error": "filename is required"}'
  exit 1
fi

if [ -z "$SHELL_AGENT_WORK_DIR" ]; then
  echo '{"error": "SHELL_AGENT_WORK_DIR not set — this tool requires shell-agent-v2 v0.1.25 or later"}'
  exit 1
fi

OUTPUT_PATH="$SHELL_AGENT_WORK_DIR/$FILENAME"

if ! printf '%s' "$CONTENT" > "$OUTPUT_PATH"; then
  python3 -c "import json; print(json.dumps({'error':'write failed','filename':'$FILENAME'}))"
  exit 1
fi

python3 -c "import json; print(json.dumps({'status':'success','filename':'$FILENAME'}))"
