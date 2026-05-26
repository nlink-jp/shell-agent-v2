# ADR-0025: Restore the assistant's tool-call explanation text on session reload

- Status: Accepted
- Deciders: magi
- Related: ADR-0024 (session restore), `docs/en/history/agent-data-flow.md §3.2`, `docs/en/history/tool-event-restore.md`

## 1. Context

Reported discrepancy: when a turn makes tool calls, the assistant's
"what I'm about to do" explanation text is shown as a chat bubble in the
**live** view, but is **missing after session reload**. Live and restored
views diverge.

The root cause is three pieces of code/doc being out of sync:

- **Live (agent loop)** — `agentLoop` (`app/internal/agent/agent.go`
  ~2079-2093) persists the tool-call turn via
  `AddAssistantMessageWithToolCalls(resp.Content, …)` and then emits that
  same `resp.Content` as a real chat bubble:
  `a.emitActivity(ActivityEvent{Type: "assistant_text", Detail: resp.Content})`.
  Its comment says this surfaces the explanation "as an assistant bubble
  in the live conversation, **matching how the same content already
  appears when the session is reloaded from disk**."
- **Frontend** — the `assistant_text` activity handler
  (`App.tsx` ~288) pushes `{role:'assistant', content}` and its comment
  says "Reload from disk already shows the same content via
  session.Records — this just brings the live UX in line with what's
  persisted."
- **Restore (LoadSession)** — `bindings.go` actually did the opposite:
  `case "assistant": … if len(r.ToolCalls) > 0 { continue }` — dropping
  the text. `agent-data-flow.md §3.2` documented this:
  *"A turn that carries `ToolCalls` is not restored as a chat bubble —
  its narrative was a transient progressTool banner."*

So both the live emit and both code comments assert restore-parity, while
the restore path and the §3.2 doc still encode the older "skip it"
behavior. The "transient progressTool banner" rationale is **stale**: the
narrative was promoted from an ephemeral banner to a real, persisted,
live-shown bubble, but LoadSession (and the doc) were never updated to
match.

The original skip also worried about "dragging back thought-style
preamble (e.g. `シンクタイム: 3秒`)". That no longer applies: `agentLoop`
cleans `resp.Content` (`chat.CleanResponse` → `stripGemmaToolCallTags` →
`StripCurrentGuardTags` → `TrimSpace`) **before** both persisting it and
emitting it live, so the persisted Content equals the live-shown text.

## 2. Decision

In `LoadSession`'s `case "assistant"`, restore the record's `Content`
whenever it is non-empty, **regardless of whether the turn also carried
`ToolCalls`**. Continue to skip:

- the legacy pre-r3 placeholder `Content == "[Calling: …]"` (never
  user-visible), and
- empty `Content` (a pure tool-call turn with no explanation — nothing
  was shown live, the tool-event bubbles carry the turn).

i.e. remove the `if len(r.ToolCalls) > 0 { continue }` branch. The tool
calls themselves remain surfaced separately by the `tool` records →
tool-event bubbles (ADR / tool-event-restore.md), preserving ordering
because records are iterated in chronological order.

Restored Content is **not** re-cleaned, matching the existing final-reply
restore path (which also emits persisted Content verbatim).

## 3. Consequences

- Live and restored views match for tool-call turns (the reported bug is
  fixed).
- Ordering is preserved: the explanation bubble appears at its record
  position, before the tool-event bubbles of the same turn.
- `agent-data-flow.md §3.2` is updated: the `assistant` row no longer
  says tool-call turns are skipped; instead, a tool-call turn restores
  its (non-empty, cleaned) explanation text as a bubble, and only the
  ephemeral progressTool "thinking" banner is not replayed.

### Edge case: legacy pre-clean sessions

Sessions written before the clean-before-persist pipeline existed may
hold tool-call records whose `Content` carries uncleaned preamble that
was never shown live. Restoring it verbatim could show minor noise.
Accepted as-is because: (a) it is symmetric with the existing final-reply
restore path, which already emits legacy Content verbatim without
re-cleaning; (b) re-cleaning on read is a separate, broader change. Not
worth special-casing here.

## 4. Rejected alternatives

- **Reverse the direction — stop emitting `assistant_text` live so the
  live view matches the old "skip on restore" behavior.** Rejected: the
  live explanation bubble was added deliberately (the text is genuinely
  useful — "what I'm about to do and why" — and was previously "dropped
  on the floor"). Removing it is a UX regression; the bug is that restore
  doesn't match live, and live is the desired behavior.
- **Re-clean Content on restore (CleanResponse on read).** Rejected as
  scope creep: it would also change the long-standing final-reply restore
  path. Tracked as a possible separate hardening if legacy noise is ever
  reported.
- **Status quo.** Rejected: it is the reported discrepancy.

## 5. Implementation

- `app/bindings.go` — `LoadSession` `case "assistant"`: drop the
  `len(r.ToolCalls) > 0` skip; keep the `[Calling:` and empty-Content
  skips. (Done, pending this ADR's approval; not committed.)
- `app/bindings_test.go` — `TestLoadSession_RestoresToolEventBubbles`:
  a tool-call assistant record with non-empty Content now restores as an
  `assistant` bubble (before its tool-event bubbles); a pure tool-call
  record with empty Content is still skipped. (Done; test passes.)
- `docs/en/history/agent-data-flow.md §3.2` (+ ja mirror) — update the
  `assistant` role row to describe the corrected restore rule.

Verification: `go test -tags no_duckdb_arrow ./...` green; manual — run a
tool-call turn with an explanation, reload the session, confirm the
explanation bubble reappears in the same order as live.

## 6. Out of scope

- Re-cleaning persisted Content on restore (see §3 edge case).
- Replaying the ephemeral progressTool "thinking" banner on restore
  (intentionally not persisted; unchanged).
- Surfacing tool arguments/results in restored bubbles (tool-event-restore.md
  non-goals, unchanged).
