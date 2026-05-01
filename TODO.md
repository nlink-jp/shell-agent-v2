# TODO

Tracked items that aren't ready for an implementation pass yet —
either because the design needs more thought, or because the work
is too large to fit into the current release stream. Items are
grouped by what kind of effort they need; within a group ordered
by quickest-to-touch first.

## Configurable / UX

Small features the user can already work around, but worth
exposing.

### Report-viewer raw HTML rendering

**Where**: `frontend/src/dialogs/ReportViewer.tsx`,
`frontend/src/components/MessageItem.tsx`.

**What**: The model occasionally emits raw HTML tags inside
report markdown (`<table>`, `<br>`, `<details>`, `<sub>`).
ReactMarkdown without `rehype-raw` shows them as literal text.
v0.1.16 mitigates by warning the LLM in the system prompt and
the create-report tool description, but recurrence is possible.
If we want to render the HTML, add `rehype-raw` *and*
`rehype-sanitize` with a strict allowlist — model output is
partially user-controlled and unsanitised HTML is an XSS
surface.

## Investigation / verification needed

These have a hypothesis but need evidence before any code change.

### Multi-image scaling above 3

**Context**: v0.1.16 adds per-image user turns on the local
backend to work around llama.cpp's mmproj multi-image slot
reuse bug. Verified manually on Vertex and local at N=3.
Gemma 3's training caps multi-image attention around 8.

**What**: Run a manual session with N=5 and N=8 images on the
local backend; confirm descriptions still bind to the correct
ID. If degradation reappears past N=3, the per-turn split
isn't enough on its own — the next layer of mitigation is
chunking the conversation into multiple agent rounds, one
image group per round.

### Sandbox integration tests sometimes timeout

**Where**: `internal/sandbox/integration_test.go`.

**What**: Full-suite `go test ./internal/...` occasionally fails
with `panic: test timed out after 1m0s` in the sandbox package.
Re-running with `-timeout 120s` passes. Likely podman cold-start
on macOS. Either prewarm the daemon before the suite or bump
the package-level timeout — but first capture timings across a
few runs to know whether it's truly flaky or genuinely slow.

## Library / dependency

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
