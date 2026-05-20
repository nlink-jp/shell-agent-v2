# ADR-0020: Title generation toggle (heuristic fallback)

- Status: Implemented in v0.13.2 (2026-05-20)
- Deciders: magi
- Related: ADR-0015, ADR-0017, ADR-0018, ADR-0019

## 1. Context

After ADR-0019 gated the per-turn auto-extraction LLM call and the
follow-up tools-sort hotfix made the `tools` array byte-stable,
production turn-3+ latency on local LM Studio finally hit the
expected sub-second range (turn 4 round-1 → round-2: 29 s → 2 s,
measured 2026-05-20). One residual issue remained in the same debug
log: **turn 2** still ran cold (~28 s on a 15 K-token prompt).

The cause is the title-generation LLM call. `generateTitleIfNeeded`
runs as a background task in `postResponseTasks` after the first
turn's response is delivered, sends a tiny prompt
(`Generate a very short title…` + the user's first message) to the
same backend, and writes the result to `session.Title`. Same
mechanism as auto-extraction: a single auxiliary LLM call evicts
llama.cpp's single prefix-KV-cache slot, so the *next* main turn
(turn 2) gets a cold re-encode of the whole conversation history.

The impact is bounded — title generation runs at most once per
session — but it is exactly the turn-2 latency users notice on a
fresh session.

## 2. Decision

Mirror the ADR-0019 pattern: per-backend toggle, defaults split by
backend, fall back to a deterministic heuristic when the LLM-driven
path is off.

**(a) `AutoTitleEnabled` flag.** Add to `LocalConfig` and
`VertexAIConfig`. Defaults:
- Local: **off** (use heuristic; cache-preservation wins on llama.cpp)
- Vertex: **on** (server-side cache is per-request-stream and is
  not evicted by auxiliary calls — same rationale as
  AutoExtractEnabled, ADR-0019 §3.1)

**(b) No fallback when off.** When AutoTitle is off,
`generateTitleIfNeeded` simply returns; `session.Title` stays empty
and the UI shows the session as untitled (same transient state it
already shows during the LLM-call window today). The user can
rename via the existing Sessions list right-click → Rename action.

Deliberately rejected a heuristic-title fallback. The motivation
for the toggle is "skip the auxiliary LLM call to preserve the
prefix cache"; conjuring a synthetic title adds product surface
for no functional gain. Untitled is fine — the UI already handles
it.

**(c) UI parity with AutoExtract.** A second checkbox row under each
profile's backend section, same exclusivity copy idiom (this one
just controls cosmetic behaviour — there is no companion tool to
hide). User-driven rename via the existing Sessions list is
unaffected and overrides whichever path produced the original
title.

## 3. Design details

### 3.1 Config schema

```go
type LocalConfig struct {
    // ... existing fields ...
    AutoTitleEnabled *bool `json:"auto_title_enabled,omitempty"`
}

type VertexAIConfig struct {
    // ... existing fields ...
    AutoTitleEnabled *bool `json:"auto_title_enabled,omitempty"`
}

const LocalAutoTitleDefault  = false  // ADR-0020 — cache wins
const VertexAutoTitleDefault = true   // server-side cache is fine

func (c LocalConfig) AutoTitle() bool { ... }
func (c VertexAIConfig) AutoTitle() bool { ... }
```

Same `*bool` representation as `AutoExtractEnabled` so the on-disk
JSON distinguishes "user set false" from "field absent → use
default". DTOs (`LocalProfileFields` / `VertexProfileFields`) get a
matching `auto_title_enabled bool`; the bindings always write
through as `*bool` so user choice persists.

### 3.2 Agent integration

In `Agent.generateTitleIfNeeded` (the goroutine launched from
`postResponseTasks`):

```go
if a.session == nil || a.session.Title != "" {
    return nil
}
if !a.autoTitleEnabled() {
    return nil  // ADR-0020: leave title empty; UI handles untitled
}
// ... existing LLM-driven path ...
```

`autoTitleEnabled()` resolves the flag from the active profile's
active backend, identical pattern to `autoExtractEnabled()`.

### 3.4 Settings UI

Add a second checkbox row directly under the existing
`auto_extract_enabled` row, in both Local and Vertex sections of the
Profile editor:

> **Auto-generate session titles**  (toggle)
> *Local default: off. When off, sessions stay untitled until you
> rename them (skips one LLM call per session).*

Same `.checkbox-row` styling as ADR-0019's row, so no new CSS
needed.

## 4. Alternatives considered

- **Always use the heuristic** (no toggle). Simpler, but Vertex
  users have no incentive to give up the LLM-generated title (their
  cache isn't affected). Rejected in favour of the per-backend
  default that does the right thing automatically while still
  letting users override.
- **Generate title from a smaller / different model.** Adds a model-
  management surface for a one-shot cosmetic feature. Rejected.
- **Defer title generation until session-close / idle.** Preserves
  cache during active use, but mid-session list display would show
  empty titles, and crashes lose the title entirely. The heuristic
  is strictly better here.
- **Run title gen concurrently with turn 1's response stream so
  cache state at end of turn 1 == cache state at start of turn 2.**
  Hard to guarantee — title gen could finish slightly after the
  user's turn-2 send, especially for first-time-cold model loads.
  Rejected as fragile.

## 5. Consequences

**Positive:**

- Turn 2 latency drops from ~28 s to ~hundreds of ms on local
  (estimate: same prefix-cache mechanism that already showed 14×
  speedup for turn-N round transitions per the post-ADR-0019
  measurement).
- No `auto_extract_enabled` × `auto_title_enabled` coupling — they
  are independent knobs over independent LLM calls. The mental
  model is "every auxiliary LLM call between turns evicts the
  cache; flag each one you can afford to skip."

**Negative:**

- Sessions stay untitled until manually renamed. Power users who
  rely on visual scanning of the Sessions list lose that affordance
  on local. Mitigation: the manual-rename action is always
  available; users who care about titles flip the toggle on.
- A new config field every backend section grows. The Profile
  editor now has two new checkbox rows per side; the rendering
  pattern already exists from ADR-0019 so this is mechanical, not
  architectural.

## 6. Implementation plan

1. Config schema + loader defaults (mirrors ADR-0019 Commit 1)
2. Gate `generateTitleIfNeeded` on AutoTitle + DTO + Settings UI
   toggle
3. Tests: gate skips LLM call, DTO round-trip, default values
4. CHANGELOG v0.13.2 amendment + ADR status → Implemented + JA
   mirror + docs-mirror-check

Verification: same debug-log workflow as ADR-0019. Replay 3-turn
session on local; confirm no `Generate a very short title…` JSONL
entry appears, turn 2 latency drops to sub-second. Vertex profile
regression: 3-turn session should still emit the title-gen LLM
call and produce a polished title.
