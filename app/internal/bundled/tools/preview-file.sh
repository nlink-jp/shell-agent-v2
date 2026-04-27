#!/bin/bash
# @tool: preview-file
# @description: Read the first portion of a text file. Returns at most N lines (default 100) and caps total bytes (default 8KB) to keep LLM context bounded.
# @param: path string "Absolute or relative path to a text file"
# @param: lines integer "Maximum number of lines to return (default 100, max 1000)"
# @param: max_bytes integer "Hard cap on returned byte length (default 8192)"
# @category: read
#
# Example: preview-file {"path": "/tmp/log.txt", "lines": 50}
# Arguments are passed as JSON via stdin.

INPUT=$(cat)
read FILE_PATH LINES MAX_BYTES <<EOF
$(FILE_INPUT="$INPUT" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ["FILE_INPUT"])
except Exception:
    d = {}
p = str(d.get("path", "")).strip()
n = d.get("lines")
b = d.get("max_bytes")
try: n = int(n) if n is not None else 100
except Exception: n = 100
try: b = int(b) if b is not None else 8192
except Exception: b = 8192
n = max(1, min(n, 1000))
b = max(64, min(b, 65536))
print(p, n, b)
')
EOF

if [ -z "$FILE_PATH" ]; then
    echo "Error: missing 'path' parameter"
    exit 1
fi

if [ ! -f "$FILE_PATH" ]; then
    echo "Error: not a regular file: $FILE_PATH"
    exit 1
fi

MIME=$(file --brief --mime-type "$FILE_PATH" 2>/dev/null)
case "$MIME" in
    text/*|application/json|application/xml|application/javascript|application/x-yaml|application/x-empty)
        ;;
    *)
        echo "Error: not a text file (mime: $MIME). Use file-info to inspect."
        exit 1
        ;;
esac

TOTAL_LINES=$(wc -l < "$FILE_PATH" 2>/dev/null | tr -d ' ')
TOTAL_BYTES=$(stat -f%z "$FILE_PATH" 2>/dev/null || stat -c%s "$FILE_PATH" 2>/dev/null)

OUTPUT=$(head -n "$LINES" "$FILE_PATH" | head -c "$MAX_BYTES")
RETURNED_BYTES=${#OUTPUT}

echo "--- preview: $FILE_PATH"
echo "--- total_lines: $TOTAL_LINES, total_bytes: $TOTAL_BYTES, returned_bytes: $RETURNED_BYTES"
if [ "$RETURNED_BYTES" -lt "$TOTAL_BYTES" ]; then
    echo "--- (truncated)"
fi
echo "---"
printf '%s' "$OUTPUT"
