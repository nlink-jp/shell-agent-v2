#!/bin/bash
# @tool: file_info
# @description: Inspect a single filesystem entry. Returns plain-text key/value lines describing it. For a regular file: `path`, `mime` (e.g. text/csv, application/json, image/png), `kind` (human description from `file(1)`), `size_bytes`, `modified` (YYYY-MM-DD HH:MM:SS), and for text-ish MIMEs (text/*, application/json/xml/javascript) also `lines`. For a directory: `kind: directory`, `path`, `entries` (number of immediate children including hidden ones). Returns `Error: missing 'path' parameter` or `Error: file not found: <path>` on failure. Use BEFORE preview_file or get_text to confirm a file is small / text-bearing / actually exists. Does not read the file's contents.
# @param: path string "Absolute or relative path to a file or directory (e.g. /tmp/data.csv, ~/Downloads/image.png, $SHELL_AGENT_WORK_DIR/note.md)"
# @category: read
# @timeout: 30
#
# Example: file_info {"path": "/tmp/data.csv"}
# Arguments are passed as JSON via stdin.

INPUT=$(cat)
FILE_PATH=$(printf '%s' "$INPUT" | FILE_INPUT="$INPUT" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ["FILE_INPUT"])
    p = d.get("path", "").strip()
except Exception:
    p = ""
print(p)
')

if [ -z "$FILE_PATH" ]; then
    echo "Error: missing 'path' parameter"
    exit 1
fi

if [ ! -e "$FILE_PATH" ]; then
    echo "Error: file not found: $FILE_PATH"
    exit 1
fi

if [ -d "$FILE_PATH" ]; then
    echo "kind: directory"
    echo "path: $FILE_PATH"
    echo "entries: $(ls -1A "$FILE_PATH" 2>/dev/null | wc -l | tr -d ' ')"
    exit 0
fi

MIME=$(file --brief --mime-type "$FILE_PATH" 2>/dev/null)
DESC=$(file --brief "$FILE_PATH" 2>/dev/null)
SIZE_BYTES=$(stat -f%z "$FILE_PATH" 2>/dev/null || stat -c%s "$FILE_PATH" 2>/dev/null)
MTIME=$(stat -f "%Sm" -t "%Y-%m-%d %H:%M:%S" "$FILE_PATH" 2>/dev/null || stat -c "%y" "$FILE_PATH" 2>/dev/null)

echo "path: $FILE_PATH"
echo "mime: $MIME"
echo "kind: $DESC"
echo "size_bytes: $SIZE_BYTES"
echo "modified: $MTIME"

case "$MIME" in
    text/*|application/json|application/xml|application/javascript)
        LINES=$(wc -l < "$FILE_PATH" 2>/dev/null | tr -d ' ')
        echo "lines: $LINES"
        ;;
esac
