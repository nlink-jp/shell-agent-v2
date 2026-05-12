# Contributing to shell-agent-v2

shell-agent-v2 is a macOS-native local-first chat and agent tool with
interactive data analysis. It ships as a Wails v2 (Go + React)
desktop application. This guide is for **new developers** who want to
build, run, modify, or extend the tool.

For Japanese: [CONTRIBUTING.ja.md](CONTRIBUTING.ja.md) (full parity).

## Where to start

- **Use the tool** → [README.md](README.md) covers install, features,
  and basic usage.
- **Understand the internals** →
  [docs/en/architecture.md](docs/en/architecture.md) for the system
  overview, then drill into the subsystem documents linked from there.
- **Fix a bug or add a feature** → read this file end to end, then
  jump to [§6 How to add X](#6-how-to-add-x).

## 1. Prerequisites

Primary development target: macOS on Apple Silicon. Cross-compilation
is possible but not regularly tested on Intel macOS, Linux, or Windows.

- **Go** — toolchain version from `app/go.mod`
- **Node.js** — 18+ for the React frontend build
- **Wails CLI** — `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **podman** (preferred) or **docker** — per-session sandbox runtime
- **python3** — required only for some integration tests (cleanly
  skipped when absent)

## 2. Build & Run

```sh
cd app
make build          # production build → dist/shell-agent-v2.app
make dev            # hot-reload dev mode (Wails)
```

**Never run `go build` directly.** The Makefile invokes Wails to
package the React frontend, embed assets, and code-sign the `.app`
bundle. A raw `go build` drops a stripped binary in the project root
and silently breaks code signing on launch.

Release artefacts must be copied with `ditto` (not `cp -r`) — macOS
uses extended attributes for ad-hoc code signing and `cp` strips them.

## 3. Test

```sh
cd app
make test                                                # all packages
go test -tags no_duckdb_arrow -count=1 ./internal/agent  # one package
```

The `no_duckdb_arrow` build tag is mandatory whenever DuckDB is in the
dependency graph (nearly always); without it the embedded Arrow
library collides with Wails. The Makefile sets it for you.

Tests skip cleanly when external prerequisites are missing:

- Sandbox tests need a running `podman` or `docker` daemon.
- MCP guardian tests need `python3` on `PATH`.

Tests are package-local; stubs and fakes are used at process /
network boundaries.

## 4. Project structure

```
shell-agent-v2/
├── app/
│   ├── bindings.go          # Wails bindings (thin delegation)
│   ├── main.go              # entry point
│   ├── Makefile             # build / test / release recipes
│   ├── internal/            # all domain logic
│   │   ├── agent/           # state machine, execution loop, tool
│   │   │                    # dispatch, MCP guardians, ToolDescriptor
│   │   │                    # registry
│   │   ├── chat/            # chat engine, message building, temporal
│   │   │                    # context, resolve-date
│   │   ├── llm/             # backend abstraction (local + Vertex AI)
│   │   ├── analysis/        # DuckDB engine (per-session, lazy init)
│   │   ├── memory/          # 4-facility memory model
│   │   ├── findings/        # global findings store with origin
│   │   │                    # provenance
│   │   ├── toolcall/        # shell-script tool registry
│   │   ├── mcp/             # mcp-guardian stdio JSON-RPC client
│   │   ├── objstore/        # central object repository (images,
│   │   │                    # blobs, markdown, reports)
│   │   ├── sandbox/         # per-session container sandbox
│   │   ├── contextbuild/    # LLM-context builder (warm/hot/summary)
│   │   ├── sessionio/       # session export / import (.shellagent)
│   │   ├── bundled/         # first-run scaffold of bundled shell
│   │   │                    # tools
│   │   ├── pathfix/         # macOS app-launch PATH normalisation
│   │   ├── atomicio/        # atomic file write helpers
│   │   ├── frontendlint/    # frontend build-time lint helper
│   │   ├── config/          # JSON config (path expansion)
│   │   └── logger/          # structured logging
│   ├── frontend/src/        # React UI
│   └── dist/                # build output (.app, zips)
├── docs/                    # English + Japanese reference + design
│                            # notes (en/ja mandatory mirror)
├── CHANGELOG.md             # behaviour changes, dated
├── README.md / README.ja.md # user-facing features + install
├── CONTRIBUTING.md / .ja.md # this file
└── AGENTS.md                # summary for LLM agents
```

For the conceptual model — state machine, memory architecture, tool
dispatch flow — start at
[docs/en/architecture.md](docs/en/architecture.md). That document is
the canonical "how it fits together" reference and links into the
per-subsystem deep dives.

## 5. Documentation rules

The repository keeps documentation in three audience tiers:

| Audience | Files |
|----------|-------|
| Users | `README.md`, `README.ja.md`, `CHANGELOG.md` |
| New developers | `CONTRIBUTING.md`, `CONTRIBUTING.ja.md` (this file) |
| Maintainers | `docs/en/`, `docs/ja/` |

**English and Japanese are mandatory mirrors.** Every `docs/en/X.md`
has a paired `docs/ja/X.ja.md` with the same structure. README and
CONTRIBUTING follow the same parity rule.

`CHANGELOG.md` gets an entry for every behaviour change, in the same
PR as the change itself.

For substantial design decisions — new subsystems, breaking changes,
non-obvious tradeoffs — write a design note under `docs/en/` (with the
`docs/ja/` mirror). Routine behaviour changes update the relevant
existing reference document instead.

## 6. How to add X

### 6.1 Add a tool

shell-agent-v2 has five tool sources. The v0.6.0 registry refactor
consolidated three of them (`analysis`, `builtin`, `sandbox`) into a
single `ToolDescriptor`-driven dispatcher — adding a tool from those
sources is **a one-file edit** plus the handler implementation.

1. **Pick the source.**
   - `analysis` — runs against the per-session DuckDB engine.
     Examples: `query-sql`, `analyze-data`, `grep-text`.
   - `builtin` — operates on agent-level state without DB / sandbox.
     Examples: `resolve-date`, `list-objects`, `get-object`.
   - `sandbox` — runs inside the per-session container.
     Examples: `sandbox-run-shell`, `sandbox-write-file`.
   - `mcp` — exposed by an external `mcp-guardian` process. Tools are
     discovered dynamically; no shell-agent-v2 code change required.
   - `shell-script` — user-supplied scripts in the data dir, parsed
     by `internal/toolcall/`.

2. **For `analysis` / `builtin` / `sandbox`:** add a `ToolDescriptor`
   to the appropriate builder in
   `app/internal/agent/tool_descriptors_*.go`. Each descriptor carries
   Name, Description, Parameters (JSON Schema), Category, Source,
   MITLDefault, HideUntilDataLoaded, and Handle. Implement the handler
   function alongside; dispatcher wiring is automatic.

3. **For `shell-script`:** drop the script in the user data dir with
   the required header comment block. The first-run scaffolder
   (`internal/bundled/`) shows the format and parsed fields.

4. **For `mcp`:** configure the guardian via Settings or
   `config.MCPProfileConfig`. No shell-agent-v2 code change required.

5. **Test.** Tools with no external dependencies get a unit test in
   the agent package. Tools that hit DuckDB / sandbox / MCP need an
   integration test — see existing tests for the same source as
   templates.

6. **Document.** If the tool is user-observable (chat surface,
   Settings → Tools, LLM behaviour), update `README.md` +
   `README.ja.md` and add a `CHANGELOG.md` entry in the same commit
   or PR. Substantial design choices (new tool category, new MITL
   gate, new schema field) warrant a design note in `docs/`.

The v0.6.0 rationale and the structural invariants the registry
enforces are documented in
[docs/en/tool-registry-refactor.md](docs/en/tool-registry-refactor.md).

## 7. Commit & PR conventions

- **Typed commits** — `feat:`, `fix:`, `docs:`, `refactor:`, `test:`,
  `chore:`, `security:`. Conventional-commits style; an optional
  scope in parentheses is encouraged (e.g. `feat(agent): ...`).
- **One logical change per commit.** A behaviour change with its test
  and CHANGELOG entry can ride together; an unrelated refactor stays
  in a separate commit.
- **Why over what.** The commit body explains motivation and
  non-obvious tradeoffs. The diff already shows the what.
- **No secrets or PII.** GCP project IDs, service-account emails,
  personal data must never appear in the repo. Use environment
  variables or `~/.shell-agent-v2/` local config.
- **PR rules.** Link the issue, link any relevant design note,
  include manual smoke-test notes for UI changes.

Organization-wide rules apply on top:
<https://github.com/nlink-jp/.github/blob/main/CONVENTIONS.md>.

## 8. Releases

Releases are coordinated by maintainers. Contributors do not run
release commands — landing a PR on `main` is sufficient. The
per-version checklist is recorded in `CHANGELOG.md` historical
entries.
