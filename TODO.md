# TODO

Tracked items that aren't ready for an implementation pass yet —
either because the design needs more thought, or because the work
is too large to fit into the current release stream. Items are
grouped by what kind of effort they need; within a group ordered
by quickest-to-touch first.

## Configurable / UX

Small features the user can already work around, but worth
exposing.

### Analysis-tool visibility before data load (planning vs. tool count)

**Background**: `agent.analysisTools(hasData bool)` exposes only
`load-data`, `reset-analysis`, `create-report`, `list-objects`,
`get-object` to the LLM until `HasData()` becomes true; the
SQL/analyze/describe/preview/summary/promote tools are hidden
until data is loaded. The original rationale (comment in
`internal/agent/tools.go:14-16`) was "keep tool count low for
local LLMs" — defensive against the early gemma2 / early gemma3
tool-calling failure modes.

**Why revisit**: the standard local model is now gemma-4-26b-a4b
(MoE, ~4B active). Modern locals comfortably handle 30–40 tools,
and the round-by-round tool re-evaluation only papers over the
fact that the LLM cannot **plan** a load-then-query workflow upfront —
it has to load blind, see new tools appear, then react. This:

- prevents accurate up-front user-facing plans ("I'll load this
  CSV and run a SQL grouping by month");
- can cause false declines ("I don't have a SQL tool") when the
  user phrases a query without attaching data first;
- invites name hallucination on weaker models ("I'll call
  `query-sql`" → `unknown tool` round-trip);
- leaves `load-data`'s description silent on what becomes
  available after success.

**Direction (sketch, not designed)**:

1. Drop the dynamic filter — expose all analysis tools every
   round.
2. Keep an escape hatch as a config flag (default off):
   `Tools.HideAnalysisToolsUntilDataLoaded bool` for users on
   weaker models / older LM Studio runtimes.
3. Rewrite `load-data`'s description to advertise the
   downstream pipeline: "After load, use `query-sql` /
   `analyze-data` / `describe-data` / `quick-summary` against
   the loaded table."
4. Verification before merge: gemma-4-26b-a4b regression on
   (a) tool selection accuracy at 30+ tools, (b) "summarise the
   data" with no attachment → does the model now propose
   `load-data` instead of declining.

Out of scope for the v0.1.20 security round; will land in a
later release with its own design doc (`agent-tool-visibility.md`).

### HTML output as a first-class object type

**Background**: Reports are produced via `create-report`, stored
as TypeReport, and rendered as Markdown by ReactMarkdown without
`rehype-raw`. When the model wants richer layout
(interactive tables, collapsible sections, embedded charts) it
sometimes emits raw HTML inside the Markdown, which the
renderer escapes — it appears as plain `<tag>` text. v0.1.16
mitigates with a system-prompt warning, but the underlying need
for richer-than-Markdown output is real.

**Direction**: instead of bolting `rehype-raw` + `rehype-sanitize`
onto the Markdown pipeline (XSS surface, mixed-mode rendering,
sanitiser allowlist that's hard to keep tight), introduce a
**separate "HTML document" object type**. Markdown stays pure
Markdown; the model uses a different tool to emit a standalone
HTML file when it wants HTML semantics.

Sketch (not designed):

- New object type `html` alongside `image` / `blob` / `report`
  in `objstore`.
- New tool `create-html-document(title, content)` or a
  sandbox-side `sandbox-register-html` that takes a `/work` HTML
  file and stores it as TypeHTML.
- New viewer: open in a **sandboxed `<iframe sandbox="">`**
  inside a dialog, with a strict CSP (`default-src 'none';
  style-src 'unsafe-inline'; img-src data: object:;`). No script
  execution, no network, no parent-frame access.
- A small "Open as HTML" button on HTML object cards in the Data
  panel, plus markdown reference syntax `[label](object:ID)`
  for cross-references.
- Update `create-report` tool description: keep saying "Markdown
  only", but now point at `create-html-document` as the escape
  hatch.

Pre-design questions:

- Does Wails' webview reliably enforce iframe `sandbox` on macOS?
  (Yes per WebKit docs, but verify.)
- Should HTML documents be editable / re-renderable, or
  one-shot? (One-shot is simpler.)
- How does the size limit of a stored HTML compare with how big
  models will go? Cap at 1 MB?
- Should we parse-and-sanitise on save (single sanitisation
  point) or only on render (defense-in-depth)? Probably both,
  with `rehype-sanitize`-equivalent on save and CSP at render.

Out of scope until we hit a real session that genuinely needs
the richer output and Markdown can't carry it.

## Investigation / verification needed

These have a hypothesis but need evidence before any code change.

### Sandbox integration tests sometimes timeout

**Where**: `internal/sandbox/integration_test.go`.

**What**: Full-suite `go test ./internal/...` occasionally fails
with `panic: test timed out after 1m0s` in the sandbox package.
Re-running with `-timeout 120s` passes. Likely podman cold-start
on macOS. Either prewarm the daemon before the suite or bump
the package-level timeout — but first capture timings across a
few runs to know whether it's truly flaky or genuinely slow.

## Library / dependency

### DuckDB `LoadFile` SQL parameterisation (audit M4, deferred from v0.1.18)

**Where**: `internal/analysis/engine.go` (`LoadFile` and the
nearby `escapeSQLString` helper).

**What**: Paths from the LLM are interpolated directly into
DuckDB SQL string literals. The current defence —
`escapeSQLString` doubling single quotes — works correctly for
today's `read_csv_auto(path)` / `read_json_auto(path)` /
`copy ... to ...` invocations, but it is one DuckDB function
spec change away from breakage. Move to parameterised
queries (DuckDB Go driver bind support) when we next touch the
analysis layer for an unrelated reason. Not urgent; flagged
here so the design choice is captured.

### `nlk/validate` integration

**Where**: tool argument parsing in `internal/agent/tools.go`,
`internal/agent/sandbox_tools.go`, MCP arg validation.

**What**: Migrate ad-hoc JSON-schema-style argument checks to
`nlk/validate` for consistent error messages and one place to
extend constraints. Not urgent — current per-call `json.Unmarshal`
handles the common cases — but worth doing before adding more
tools that share argument shapes.

## Long-running / design needed

### Stronger local-backend multi-image inference

**Context**: v0.1.16's per-image user turns work around
llama.cpp's mmproj slot bug, but the underlying limitation
(LM Studio + llama.cpp's multimodal pipeline lags vLLM / HF
Transformers) remains. Some ideas, none designed yet:

- Direct SigLIP encoder access (skip llama.cpp's mmproj path)
- Re-enable streaming for non-tool-call paths (the
  multi-image + tool-call combo is what currently disables it)
- A configurable "vision model" alongside the chat model so
  Gemma 3 isn't always the choice for image analysis

Out of scope until someone wants to invest in local-only
quality parity with Vertex.

### Per-backend retry policy in Settings

Currently hardcoded to 3 attempts with 5s→60s exponential
backoff (±10% jitter) in `internal/llm/retry.go`'s
`DefaultRetryPolicy`. The other "more knobs" candidates were
shipped — per-backend timeouts, memory v2 toggle, sandbox
image / CPU / memory, and per-backend `output_reserve` /
`max_tool_rounds` are all in the Settings dialog.

Retry was deferred because exposing it risks user
mis-configuration with little benefit; revisit only if a real
session needs custom retry behaviour (e.g., a slower-quota
GCP project that benefits from longer initial backoff).
