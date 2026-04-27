#!/bin/bash
# @tool: generate-image
# @description: Generate an image from a text prompt using Vertex AI Gemini. Output is saved to the job workspace.
# @param: prompt string "Image generation prompt describing what to create"
# @param: filename string "Output filename (e.g. sunset.png)"
# @category: execute
#
# REQUIRES: gem-image (https://github.com/nlink-jp/gem-image)
#   Build: cd gem-image && make build
#   Install: cp dist/gem-image ~/bin/
# REQUIRES: Vertex AI credentials (gcloud auth application-default login)

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('prompt',''))" 2>/dev/null)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('filename','generated.png'))" 2>/dev/null)

if [ -z "$PROMPT" ]; then
  echo '{"error": "prompt is required"}'
  exit 1
fi

export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

OUTPUT_DIR="${SHELL_AGENT_WORK_DIR:-/tmp/shell-agent-images}"
mkdir -p "$OUTPUT_DIR"
OUTPUT_PATH="$OUTPUT_DIR/$FILENAME"

gem-image -p "$PROMPT" -o "$OUTPUT_PATH" --force 2>/dev/null

if [ -f "$OUTPUT_PATH" ]; then
  echo "{\"status\": \"success\", \"filename\": \"$FILENAME\", \"prompt\": \"$PROMPT\"}"
else
  echo "{\"error\": \"Image generation failed\", \"prompt\": \"$PROMPT\"}"
fi
