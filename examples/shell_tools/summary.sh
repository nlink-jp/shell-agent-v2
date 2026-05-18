#!/bin/bash
# @tool: summary
# @description: Summarise a text document in a single Vertex AI Gemini call. Fast and cheap path for the everyday "give me a summary" case. Pass the raw document text as the `content` parameter; if the user attached a markdown/text document, you can fetch its content first with get-text, then pass that content here. Optional `style` is "short" / "medium" / "long" (default medium). Optional `lang` forces the output language (e.g. "ja", "en"); leave empty for auto-detect from the input. Prefer this over `analyze-text` whenever the user just wants a summary — `analyze-text` is a sliding-window multi-LLM-call pipeline designed for log audits and cross-section anomaly detection, which is overkill for ordinary summary requests and costs 3-5x more LLM calls. Use `analyze-text` only when the user specifically asks for pattern-finding across a long document where the running summary between windows materially matters.
# @param: content string "The raw text to summarise (the document body, not a description of it)."
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
# REQUIRES: ~/.config/gem-summary/config.toml with at least [gcp].project
#   set, or the GEMSUMMARY_PROJECT / GOOGLE_CLOUD_PROJECT env var set.
#
# Output contract: emits the gem-summary --json payload verbatim,
# i.e. {"summary":..., "chunks":..., "tokens_in":..., "tokens_out":...,
# "duration_seconds":...}. The "summary" field is the bit to surface to
# the user; the rest is metadata the LLM can mention if the user asked
# for cost / size attribution.

INPUT=$(cat)
CONTENT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('content',''))" 2>/dev/null)
STYLE=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('style','medium'))" 2>/dev/null)
LANG=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('lang',''))" 2>/dev/null)

if [ -z "$CONTENT" ]; then
  echo '{"error": "content is required — pass the document text as the `content` parameter"}'
  exit 1
fi

# Match the PATH augmentation used by other gem-* shell tools so a
# Finder/launchd-launched shell-agent-v2 finds gem-summary in either
# ~/bin or ~/go/bin or the standard Homebrew prefixes.
export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

if ! command -v gem-summary >/dev/null 2>&1; then
  echo '{"error": "gem-summary not found on PATH — install from https://github.com/nlink-jp/gem-summary"}'
  exit 1
fi

ARGS=(--json --style "$STYLE" --quiet)
if [ -n "$LANG" ]; then
  ARGS+=(--lang "$LANG")
fi

# Pipe content over stdin to keep the argv clean — large documents
# would blow ARG_MAX otherwise. --quiet suppresses gem-summary's
# stderr progress (which would otherwise leak into the agent's
# tool-event log).
echo "$CONTENT" | gem-summary "${ARGS[@]}"
