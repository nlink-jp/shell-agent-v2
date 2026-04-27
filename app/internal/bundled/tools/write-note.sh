#!/bin/bash
# @tool: write-note
# @description: Write a note to a file in /tmp (requires approval)
# @param: filename string "Filename only (e.g. hello.txt), saved under /tmp/"
# @param: content string "Content to write"
# @category: write
#
# Category "write" requires MITL (Man-In-The-Loop) approval before execution.
# Arguments are passed as JSON via stdin. Always sanitize parsed values —
# never pass them to eval or unquoted shell expansion.

INPUT=$(cat)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json,os; print(os.path.basename(json.load(sys.stdin).get('filename','note.txt')))" 2>/dev/null)
CONTENT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('content',''))" 2>/dev/null)
echo "$CONTENT" > "/tmp/${FILENAME}"
echo "Written to /tmp/${FILENAME}"
