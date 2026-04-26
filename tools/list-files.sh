#!/bin/bash
# @tool: list-files
# @description: List files in a directory
# @param: path string "Directory path to list"
# @category: read
#
# Example: list-files {"path": "/tmp"}
# Arguments are passed as JSON via stdin (not CLI args).

INPUT=$(cat)
PATH_ARG=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('path','.'))" 2>/dev/null)
ls -la "${PATH_ARG:-/tmp}/"
