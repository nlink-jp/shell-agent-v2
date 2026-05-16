#!/bin/bash
# @tool: search-kb-lite
# @description: Search a pre-indexed knowledge base via lite-rag (local LLM via OpenAI-compatible API, e.g. LM Studio, with DuckDB store). Returns answer + source citations as JSON.
# @param: query string "Question to ask the knowledge base"
# @category: read
# @timeout: 120
#
# REQUIRES: lite-rag (https://github.com/nlink-jp/lite-rag)
#   Build: cd lite-rag && make build
#   Install: cp dist/lite-rag ~/bin/
# REQUIRES: ~/.config/lite-rag/config.toml — copy from
#   lite-rag/config.example.toml and set api.base_url, api.api_key,
#   models.* (LM Studio default works out of the box).
# REQUIRES: pre-indexed corpus — run `lite-rag index --dir <docs>` once
#   before this tool is useful. Without an index the answer will be empty.
# REQUIRES: a running local LLM endpoint (e.g. LM Studio) reachable at
#   the configured api.base_url. Without it the call hangs/fails.
#
# Output: passes through lite-rag's --json contract verbatim
# (typically {"answer": "...", "sources": [...]}).

INPUT=$(cat)
QUERY=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('query',''))" 2>/dev/null)

if [ -z "$QUERY" ]; then
  echo '{"error": "query is required"}'
  exit 1
fi

export PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

if ! command -v lite-rag >/dev/null 2>&1; then
  echo '{"error": "lite-rag not installed", "install": "build from https://github.com/nlink-jp/lite-rag and copy dist/lite-rag to ~/bin/"}'
  exit 1
fi

lite-rag ask --json "$QUERY" 2>/dev/null
