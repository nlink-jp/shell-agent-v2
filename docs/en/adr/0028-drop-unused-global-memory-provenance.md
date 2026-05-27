# ADR-0028: Drop unused provenance fields from Global Memory

- Status: Accepted
- Deciders: magi
- Related: ADR-0027 (global memory export/import — simplified by this change), `docs/en/reference/memory-model.md` (schema)

## 1. Context

`GlobalMemoryEntry` (`app/internal/memory/global_memory.go`) carries three
provenance fields that **no code reads**:

| Field | Set sites | Read sites |
|-------|-----------|------------|
| `SessionID` | 4 (`agent.go:1804`, `agent.go:1849`, `tools.go:286`, `agent_extract.go:273`) | **0** |
| `SourceTurnIndex` | 1 (`agent_extract.go:274`) | **0** |
| `PromotedFromID` | **0** | **0** |

`SessionID` and `SourceTurnIndex` are *write-only*: stamped at promotion /
extraction time, never consumed. `PromotedFromID` is entirely dead —
declared in the struct and never even written.

These point into the **session namespace**, which is machine-local and not
durable:

- Session IDs are timestamp strings (`sess-<unixMilli>`, `bindings.go:492`),
  not UUIDs. They are not unique across machines (two machines share the
  wall-clock millisecond space) and the new-session path has no
  collision guard, so even on one machine a back-reference is not a
  reliable key.
- `SourceTurnIndex` is an index into a *specific* session's record list —
  meaningless without that session, and doubly meaningless on another
  machine.

The cost of keeping unread data is not zero: it forced ADR-0027 to add
import-time sanitisation (clear the foreign IDs so a future
provenance-consuming feature cannot mistake a collided foreign ID for a
local session). That is complexity entirely in service of data we do not
use.

### Does this affect Findings?

These dead fields almost certainly originated by analogy with the
**Findings** store, which historically carried `OriginSessionID` /
`OriginSessionTitle`. That history makes a ripple worth ruling out
explicitly. It does not exist:

- The `findings` package imports `internal/memory` for exactly one symbol —
  `memory.SessionDir()` (`findings.go:93`), a path helper. It never
  references `GlobalMemoryEntry` or any field this ADR removes. `memory`
  does not import `findings` at all (one-way, minimal coupling).
- The `Finding` struct (`findings.go:52`) is already clean: `ID`, `Content`,
  `Tags`, `CreatedAt`, `CreatedLabel`, `Source`, `ToolOriginated`. Its
  session-origin fields were removed when findings became per-session
  files (v0.2.0); only an explanatory comment remains. So there is no
  parallel cleanup to do in Findings — it was done already.
- The promote-finding path (`agent.go:~1844`) reads only `f.Content` and
  `f.ToolOriginated` from the Finding; dropping `GlobalMemoryEntry.SessionID`
  removes an assignment, not a read of any Finding field.
- On the frontend, `Finding` (`types.ts:76`) has no `session_id`. The only
  `session_id` fields elsewhere belong to unrelated types (`LLMStatus`,
  `ObjectInfo`). This ADR touches only the `GlobalMemory` interface.

Conclusion: the change is confined to Global Memory. Findings is decoupled
and already in the target shape.

## 2. Decision

**Remove `SessionID`, `SourceTurnIndex`, and `PromotedFromID` from
`GlobalMemoryEntry`.** Drop their assignment at every set site.

Keep:

- `Fact`, `NativeFact`, `Category`, `SourceTime`, `CreatedAt` — core,
  read by dedup / display / prompt budgeting.
- `Source` — **read** by `globalTrustTag` (`global_memory.go:228`) to
  render the `[user-stated]` / `[derived]` tag. The
  `GlobalSourcePromotedFrom*` constants stay; `Source` still records *how*
  an entry arose, which is a portable enum, not a machine-local pointer.
- `ToolOriginated` — a context-free boolean, no namespace hazard; left in
  place (out of scope for this trim).

Rationale: YAGNI. If a future feature genuinely needs origin provenance,
it should be reintroduced **with a design that handles cross-machine
identity** (a stable, globally-unique origin token) rather than the
current machine-local, never-read fields. Carrying a broken-across-machines
reference today is worse than not having one — it is latent risk with no
benefit.

## 3. Consequences

- The cross-machine collision concern for Global Memory **disappears at the
  schema level**: an entry that has no session reference cannot collide
  with or be mistaken for a local session. No future feature can trip over
  it because the data is gone.
- ADR-0027 (export/import) is simplified: there is nothing machine-local to
  sanitise on import; entries are portable verbatim.
- **Backward compatible, no migration code.** `Load` uses a plain
  `json.Unmarshal` (`global_memory.go:107`, no `DisallowUnknownFields`), so
  existing `global_memory.json` files that contain `session_id`,
  `source_turn_index`, or `promoted_from_id` keys load fine — the unknown
  keys are ignored and silently disappear on the next `Save`.
- Loss: any historical origin-session values currently on disk are dropped.
  Acceptable — nothing reads them, and they are unreliable across machines.

## 4. Implementation

- `app/internal/memory/global_memory.go` — remove the three fields from
  `GlobalMemoryEntry` (and the "Promotion back-reference" comment block).
- Remove `SessionID:` / `SourceTurnIndex:` assignments:
  - `app/internal/agent/agent.go:1804` (promote-from-session-memory),
    `:1849` (promote-from-finding) — drop `SessionID`.
  - `app/internal/agent/tools.go:286` (remember-fact tool) — drop
    `SessionID`.
  - `app/internal/agent/agent_extract.go:273-274` (global extraction) —
    drop `SessionID` and `SourceTurnIndex`. (The SessionMemoryEntry literal
    at `:299` keeps its own `SourceTurnIndex` — out of scope; this ADR
    touches Global Memory only.)
- `app/internal/agent/extract_memories_test.go:96-97` — remove the
  assertion on the global entry's `SessionID`.
- `app/bindings.go` — remove `SessionID` from the `GlobalMemoryData` DTO
  (`:1928` area) and the `SessionID: f.SessionID` mapping in
  `GetGlobalMemories`; update the struct doc comment (it currently lists
  `SessionID` as driving the badge, which is stale — the sidebar does not
  render it).
- `app/frontend/src/types.ts` — remove `session_id?` from the
  `GlobalMemory` interface (leave the unrelated `LLMStatus.session_id` and
  `ObjectInfo.session_id`). No Sidebar render change (it is not displayed).
- `docs/en/reference/memory-model.md` + `docs/ja/reference/memory-model.ja.md`
  — update the `GlobalMemoryEntry` schema block to drop the three fields.

### Tests

- Existing memory/agent tests pass after dropping the one `SessionID`
  assertion.
- Add a backward-compat test: `Load` a `global_memory.json` whose entries
  contain legacy `session_id` / `source_turn_index` / `promoted_from_id`
  keys → succeeds; after `Save`, the file no longer contains those keys and
  the in-memory entries are intact (Fact/Category/Source preserved).

## 5. Out of scope

- `SessionMemoryEntry.SourceTurnIndex` (session-scoped; separate store).
- `ToolOriginated` (kept).
- Session-ID generation hardening (`sess-<unixMilli>` uniqueness) — a
  separate concern; removing Global Memory's dependence on session IDs does
  not require it.

## 6. Manual smoke checklist

1. Promote a Session Memory entry and a Finding to Global Memory → both
   appear; `global_memory.json` contains no `session_id` /
   `source_turn_index` / `promoted_from_id` keys; trust tags
   ([user-stated]) still correct.
2. Use the remember-fact tool → entry added, no dropped-field keys, no
   error.
3. Open a pre-existing `global_memory.json` (with the old keys) → loads;
   facts display unchanged; after any mutation the keys are gone from disk.
