# TODO

Tracked items that aren't ready for an implementation pass yet —
either because the design needs more thought, or because the work
is too large to fit into the current release stream. Items are
grouped by what kind of effort they need; within a group ordered
by quickest-to-touch first.

## Cleanup (small, low-risk)

These are one- or two-file edits with obvious correctness — they
just need a session where someone confirms the precondition still
holds and lands the change.

### Drop the `'done'` soft-fallback in the frontend tool-event union

**Where**: `frontend/src/types.ts` (the `ChatMessage`
status field) and the receiver in `frontend/src/App.tsx`'s
`agent:activity` listener.

**What**: Phase A of the tool-bubble success/failure work
introduced `'success' | 'error'` as the canonical statuses; the
listener still falls back to a synthetic `'success'` when an
older payload arrives without a `status` field. After v0.1.16
no in-flight payload should be missing the field — confirm
against a fresh session log, then drop the fallback so the type
stays honest.

### Dead `.object-item` CSS

**Where**: `frontend/src/App.css`.

**What**: The Objects panel was removed in the info-display
redesign Phase 3 and the styles for `.object-item*` are no longer
referenced. Verify nothing else (including the report viewer)
inherits them, then delete.

### `ObjectReferences` binding usage review

**Where**: `bindings.go` and `frontend/src/bindings.ts`.

**What**: The `ObjectReferences` Wails binding may be unused on
the frontend after the Objects panel removal. Search for
callers; if none, remove the binding.

### LLM tool description: "central object repository"

**Where**: `internal/agent/tools.go` `buildToolDefs`, the
`list-objects` / `get-object` descriptions.

**What**: Wording reads "central object repository", which is
internal jargon. Rephrase to something a model and a human can
both follow ("session-wide object store" or similar).

## Configurable / UX

Small features the user can already work around, but worth
exposing.

### Configurable max-rounds cap

**Where**: `internal/agent/agent.go:38` (`maxToolRounds`),
`internal/config/`, Settings UI.

**What**: Hardcoded to 10. Loop detection (shipped v0.1.16)
catches same-error stretches early but a long, legitimate
analysis can still hit the cap. Surface as a Settings entry,
default 10. Mitigation 2 from the original "Agent loop: get
unstuck" TODO. Out of scope until a real session legitimately
needs more rounds.

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

### Configurable Settings: more knobs

Aggregate of small Settings asks that are individually too
small to ship alone. Surface together when one of them becomes
load-bearing.

- Per-backend retry policy (currently `DefaultRetryPolicy(...)`)
- Per-backend per-request timeout
- Memory v2 budget defaults
- Sandbox image / CPU / memory defaults
