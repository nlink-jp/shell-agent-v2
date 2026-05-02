#!/bin/bash
# @tool: generate-image
# @description: Generate an image from a text prompt using Vertex AI Gemini. Writes to $SHELL_AGENT_WORK_DIR; the agent should follow up with register-object so the image appears inline in chat.
# @param: prompt string "Image generation prompt describing what to create"
# @param: filename string "Output filename (e.g. sunset.png)"
# @category: execute
# @timeout: 120
#
# REQUIRES: gem-image (https://github.com/nlink-jp/gem-image)
#   Build: cd gem-image && make build
#   Install: cp dist/gem-image ~/bin/
# REQUIRES: Vertex AI credentials (gcloud auth application-default login)
#
# Flow (shell-agent-v2 ≥ v0.1.25):
#   1. This script writes the image to $SHELL_AGENT_WORK_DIR/<filename>
#      (the same physical directory the sandbox bind-mounts at /work).
#   2. The image immediately appears in the Data panel's /work section.
#   3. The agent sees the "next_step" hint in the success payload and
#      follows up with `register-object` to surface it in chat as
#      object:<ID>. See docs/en/work-dir-shell-bridge.md.

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('prompt',''))" 2>/dev/null)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('filename','generated.png'))" 2>/dev/null)

if [ -z "$PROMPT" ]; then
  echo '{"error": "prompt is required"}'
  exit 1
fi

if [ -z "$SHELL_AGENT_WORK_DIR" ]; then
  echo '{"error": "SHELL_AGENT_WORK_DIR not set — this tool requires shell-agent-v2 v0.1.25 or later"}'
  exit 1
fi

export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

OUTPUT_PATH="$SHELL_AGENT_WORK_DIR/$FILENAME"

if ! gem-image -p "$PROMPT" -o "$OUTPUT_PATH" --force 2>/dev/null; then
  echo "{\"error\": \"Image generation failed (gem-image returned non-zero)\", \"prompt\": \"$PROMPT\"}"
  exit 1
fi

# Hint the LLM to surface the image to the user via register-object.
# The image is now visible in the Data panel /work section but
# won't appear inline in chat until registered.
ESCAPED_PROMPT=$(echo "$PROMPT" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read().rstrip()))")
cat <<EOF
{"status":"success","filename":"$FILENAME","next_step":"Call register-object with path=\"$FILENAME\" name=$ESCAPED_PROMPT to surface the image in chat as an object:<ID> reference."}
EOF
