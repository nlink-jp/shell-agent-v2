#!/bin/bash
# @tool: search-kb-gem
# @description: Search a pre-indexed knowledge base via gem-rag (Vertex AI Gemini RAG with DuckDB store). Returns answer + source citations as JSON.
# @param: query string "Question to ask the knowledge base"
# @category: read
# @timeout: 120
#
# REQUIRES: gem-rag (https://github.com/nlink-jp/gem-rag)
#   Install: uv tool install gem-rag (or follow project README)
# REQUIRES: Vertex AI credentials (gcloud auth application-default login)
# REQUIRES: pre-indexed corpus — run `gem-rag index --dir <docs>` once
#   before this tool is useful. Without an index the answer will be empty.
#
# Output: passes through gem-rag's --json contract verbatim
# (typically {"answer": "...", "sources": [...]}).

INPUT=$(cat)
QUERY=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('query',''))" 2>/dev/null)

if [ -z "$QUERY" ]; then
  echo '{"error": "query is required"}'
  exit 1
fi

export PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

if ! command -v gem-rag >/dev/null 2>&1; then
  echo '{"error": "gem-rag not installed", "install": "uv tool install gem-rag"}'
  exit 1
fi

gem-rag ask --json "$QUERY" 2>/dev/null
