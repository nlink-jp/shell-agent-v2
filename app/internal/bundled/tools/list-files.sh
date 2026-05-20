#!/bin/bash
# @tool: list-files
# @description: List the contents of one directory using `ls -la`. Returns the raw long-format ls output as plain text (mode bits, owner, group, size in bytes, mtime, name) — one entry per line, hidden entries (names starting with `.`) included. NOT recursive: only the immediate children of `path`. For deeper inspection use file-info on a specific file, or preview-file to read text contents. Defaults to /tmp if `path` is omitted. The session work directory (where write-note / register-object write files) is at $SHELL_AGENT_WORK_DIR; pass that as `path` to list it.
# @param: path string "Absolute or relative directory path (e.g. /tmp, ~/Downloads, $SHELL_AGENT_WORK_DIR). Defaults to /tmp if omitted. The trailing `/` is added automatically."
# @category: read
# @timeout: 30
#
# Example: list-files {"path": "/tmp"}
# Arguments are passed as JSON via stdin (not CLI args).

INPUT=$(cat)
PATH_ARG=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('path','.'))" 2>/dev/null)
ls -la "${PATH_ARG:-/tmp}/"
