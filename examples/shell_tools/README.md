# Shell tool examples

Optional shell-tool scripts that wrap companion CLIs from the
`nlink-jp` org. None are embedded in the production binary or
auto-installed — copy the ones you want into your
`<dataDir>/tools/` directory (typically
`~/Library/Application Support/shell-agent-v2/tools/`).

## Available examples

| Script | Wraps | Requires | Default MITL |
|--------|-------|----------|--------------|
| [`web-search.sh`](web-search.sh) | [gem-search](https://github.com/nlink-jp/gem-search) | `gem-search` binary on `$PATH` | off (`@category: read`) |
| [`generate-image.sh`](generate-image.sh) | [gem-image](https://github.com/nlink-jp/gem-image) | `gem-image` binary on `$PATH` | on (`@category: write`) |
| [`search-kb-gem.sh`](search-kb-gem.sh) | [gem-rag](https://github.com/nlink-jp/gem-rag) (Vertex AI Gemini RAG) | `gem-rag` binary + pre-indexed corpus | off |
| [`search-kb-lite.sh`](search-kb-lite.sh) | [lite-rag](https://github.com/nlink-jp/lite-rag) (local LLM RAG) | `lite-rag` binary + pre-indexed corpus | off |

All four scripts declare `@timeout: 120` because the underlying
RAG / search / image round-trips routinely exceed the
30-second default. See
[docs/en/history/tool-execution-timeout.md](../../docs/en/history/tool-execution-timeout.md)
for the rationale.

## Installing one

```sh
cp examples/shell_tools/web-search.sh \
   ~/Library/Application\ Support/shell-agent-v2/tools/
```

shell-agent-v2 picks up new scripts on the next agent turn — no
restart needed. If the tool doesn't appear, check
**Settings → Tools** for parse errors (typically a missing
`@tool:` header).

## Customising / writing your own

These four scripts are reference implementations, not
contracts — feel free to fork the file, edit the command,
adjust the timeout, or change the MITL category. shell-agent-v2
treats anything under `<dataDir>/tools/*.sh` with a valid
`@tool:` header as a tool.

The header format and supported metadata (`@tool:`,
`@description:`, `@category:`, `@timeout:`, `@mitl:`) live in
[`docs/en/reference/architecture.md`](../../docs/en/reference/architecture.md)
under §5 Tool system. The structural test
`internal/bundled.TestRepoRootExamples_HaveToolHeader` keeps
every example here syntactically valid.

## Why these are not auto-installed

Each script needs an external CLI on `$PATH` (`gem-search`,
`gem-image`, `gem-rag`, `lite-rag`). Auto-installing them would
surface broken tools to users who haven't installed the
corresponding companion, polluting the **Settings → Tools** list
with permanent errors. Opt-in install via copy is the cleaner
choice.

Sibling library: [`../system_rules/`](../system_rules/) — same
pattern, but for `<dataDir>/system_rules.md` standing
instructions instead of shell tools.
