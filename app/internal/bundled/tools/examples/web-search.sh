#!/bin/bash
# @tool: web-search
# @description: Search the web using agentic research pipeline via Vertex AI Gemini with Google Search Grounding
# @param: query string "Search query"
# @param: lang string "Language code (e.g. ja, en)"
# @category: read
#
# REQUIRES: gem-search (https://github.com/nlink-jp/gem-search)
#   Build: cd gem-search && make build
#   Install: cp dist/gem-search ~/bin/
# REQUIRES: Vertex AI credentials (gcloud auth application-default login)

INPUT=$(cat)
QUERY=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('query',''))" 2>/dev/null)
LANG=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('lang','ja'))" 2>/dev/null)

if [ -z "$QUERY" ]; then
  echo '{"error": "query is required"}'
  exit 1
fi

export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

echo "$QUERY" | gem-search --format markdown --lang "$LANG" 2>/dev/null
