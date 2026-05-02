#!/bin/bash
# @tool: generate-image
# @description: Generate an image from a text prompt using Vertex AI Gemini. Writes the image to $SHELL_AGENT_WORK_DIR (the per-session work directory; same physical path the sandbox bind-mounts at /work). Once this returns, follow up with register-object (path=<filename>) to surface the image inline in chat as an object:<ID> reference. The Data → /work panel shows the file even before registration.
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
# Output contract: returns ONLY {status, filename}. Intentionally does
# NOT emit a `next_step` instruction or the absolute host path:
#   - Imperative `next_step` strings narrow the LLM's planning horizon
#     to the immediate sub-step and can derail multi-step user plans.
#     The follow-up convention belongs in @description (re-injected
#     every round) — see docs/en/work-dir-shell-bridge.md §Risks.
#   - Absolute host paths in tool output become exfiltration material
#     (LLM context → log → ...) and a juicy input for any tool that
#     accepts absolute paths (e.g. load-data, which is MITL-gated for
#     exactly this reason). Output filenames only.

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
  # Error path may emit just the filename and a generic message;
  # the prompt is the user's input so re-emitting it is harmless,
  # but kept short.
  python3 -c "import json; print(json.dumps({'error': 'image generation failed (gem-image returned non-zero)', 'filename': '$FILENAME'}))"
  exit 1
fi

# Success path: status + filename only. No next_step, no absolute path.
python3 -c "import json; print(json.dumps({'status': 'success', 'filename': '$FILENAME'}))"
