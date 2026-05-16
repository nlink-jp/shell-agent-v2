# examples

Optional artefacts users opt into deliberately. None of the
contents here are auto-installed or embedded in the production
binary — copy the bits you want into your shell-agent-v2 data
directory.

## Catalogue

| Directory | What it is |
|-----------|------------|
| [`system_rules/`](system_rules/) | Copy-paste templates for `<dataDir>/system_rules.md` — standing instructions that shape every agent turn. One template per recurring bias/workflow problem. See [docs/en/reference/system-rules.md](../docs/en/reference/system-rules.md) for the System Rules design. |
| [`shell_tools/`](shell_tools/) | Optional shell-tool scripts (`web-search`, `generate-image`, `search-kb-gem`, `search-kb-lite`) wrapping companion CLIs from the `nlink-jp` org. Copy any into `<dataDir>/tools/` to surface as agent tools. See [docs/en/reference/architecture.md §5](../docs/en/reference/architecture.md) for the shell-tool architecture. |

## Why these are not auto-installed

Auto-installed scripts at `app/internal/bundled/tools/` are
deliberately minimal — only the always-useful, no-external-
dependency tools (`file-info`, `weather`, `list-files`, etc.).
Everything here requires either an external CLI (`gem-search`,
`gem-image`, `gem-rag`, `lite-rag`) or a non-trivial user
decision (which audit framing, which RAG corpus). The split
keeps a clean install minimal while making the optional library
easy to find from the repository root.
