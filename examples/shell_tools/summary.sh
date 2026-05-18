#!/bin/bash
# @tool: summary
# @description: Summarise text via a single Vertex AI Gemini call. Fast and cheap path for ordinary "give me a summary" requests; chunked + parallel + merge fallback inside gem-summary handles inputs larger than the model's context window. PROVIDE EXACTLY ONE OF the two parameters below — do NOT pass both. WORKFLOW: (1) Attached document (visible as a "Document (object ID: XXXX)" anchor): FIRST call sandbox-copy-object to copy it into /work, THEN call summary with ONLY `filename` set to that /work filename — leave content empty/absent. (2) Short inline text already visible in conversation context: pass it as `content` and leave filename empty/absent. The /work-based exchange pattern matches other shell tools (generate-image → register-object, sandbox-run-python writing chart files); see docs/en/history/work-dir-shell-bridge.md. Prefer this tool over analyze-text for ordinary summary requests; analyze-text is a 3-5-LLM-call sliding-window pipeline designed for log audits and cross-section pattern detection — reserve it for that specific use case where the running summary between windows materially helps.
# @param: filename string "Filename inside /work to summarise. EXCLUSIVE with content — pass only this for attached documents (after sandbox-copy-object). Do NOT also pass content."
# @param: content string "Inline text to summarise (short text already visible in conversation, NOT the user's request). EXCLUSIVE with filename — pass only this for inline text. Do NOT also pass filename."
# @param: style string "Output length preset: short, medium, long. Default: medium."
# @param: lang string "Output language code (e.g. ja, en). Default: auto-detect from input."
# @category: read
# @timeout: 120
#
# REQUIRES: gem-summary (https://github.com/nlink-jp/gem-summary)
#   Install:
#     # macOS arm64 — adjust the URL for your platform
#     curl -L -o /tmp/gs.zip https://github.com/nlink-jp/gem-summary/releases/latest/download/gem-summary-v0.1.0-darwin-arm64.zip
#     cd /tmp && unzip gs.zip && mv gem-summary-darwin-arm64 ~/bin/gem-summary && chmod +x ~/bin/gem-summary
#   Or build from source:
#     git clone https://github.com/nlink-jp/gem-summary
#     cd gem-summary && make build && cp dist/gem-summary ~/bin/
# REQUIRES: Vertex AI credentials (gcloud auth application-default login)
# REQUIRES: ~/.config/gem-summary/config.toml with [gcp].project set,
#   or the GEMSUMMARY_PROJECT / GOOGLE_CLOUD_PROJECT env var set.
#
# Output contract: emits the gem-summary --json payload verbatim:
#   {"summary": ..., "chunks": ..., "tokens_in": ...,
#    "tokens_out": ..., "duration_seconds": ...}
# The "summary" field is the bit to surface to the user; the rest is
# metadata the LLM can mention if the user asked for cost / size
# attribution.

INPUT=$(cat)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('filename',''))" 2>/dev/null)
CONTENT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('content',''))" 2>/dev/null)
STYLE=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('style','medium'))" 2>/dev/null)
LANG=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('lang',''))" 2>/dev/null)

# Match the PATH augmentation used by other gem-* shell tools so a
# Finder/launchd-launched shell-agent-v2 finds gem-summary in either
# ~/bin or ~/go/bin or the standard Homebrew prefixes.
export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

if ! command -v gem-summary >/dev/null 2>&1; then
  echo '{"error": "gem-summary not found on PATH — install from https://github.com/nlink-jp/gem-summary"}'
  exit 1
fi

# Validate exactly-one-of(filename, content). Doing this check now
# (rather than relying on gem-summary to complain) lets us return a
# focused error message the LLM can actually act on.
if [ -n "$FILENAME" ] && [ -n "$CONTENT" ]; then
  echo '{"error": "provide either filename OR content, not both"}'
  exit 1
fi
if [ -z "$FILENAME" ] && [ -z "$CONTENT" ]; then
  echo '{"error": "filename or content is required — for attached documents use sandbox-copy-object to put the file in /work, then call summary with filename=<basename>"}'
  exit 1
fi

ARGS=(--json --style "$STYLE" --quiet)
if [ -n "$LANG" ]; then
  ARGS+=(--lang "$LANG")
fi

if [ -n "$FILENAME" ]; then
  # Validate SHELL_AGENT_WORK_DIR is set; without it we cannot
  # resolve the filename. Surface a clear error so the LLM knows
  # the host shell-agent-v2 is too old to support the /work bridge.
  if [ -z "$SHELL_AGENT_WORK_DIR" ]; then
    echo '{"error": "SHELL_AGENT_WORK_DIR not set — this tool requires shell-agent-v2 v0.1.25 or later"}'
    exit 1
  fi
  # Resolve filename relative to /work. Reject path traversal:
  # the filename must be a plain basename (no slashes, no ..).
  case "$FILENAME" in
    */* | *..*)
      echo '{"error": "filename must be a basename inside /work — no slashes or .."}'
      exit 1
      ;;
  esac
  FULL_PATH="$SHELL_AGENT_WORK_DIR/$FILENAME"
  if [ ! -f "$FULL_PATH" ]; then
    echo "{\"error\": \"file not found in /work: $FILENAME — did you run sandbox-copy-object first?\"}"
    exit 1
  fi
  gem-summary "${ARGS[@]}" "$FULL_PATH"
else
  # Inline content path. Piped via stdin to keep argv clean — large
  # documents would blow ARG_MAX otherwise. --quiet suppresses
  # gem-summary's stderr progress which would otherwise leak into
  # the agent's tool-event log.
  echo "$CONTENT" | gem-summary "${ARGS[@]}"
fi
