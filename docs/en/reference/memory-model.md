# Memory Model — shell-agent-v2

> Date: 2026-05-03
> Status: **v0.2.0 design (pre-release)** — major version bump.
> The architecture below replaces the v0.1.x model wholesale.
> No data migration: legacy `pinned.json` and `findings.json`
> from v0.1.x are ignored on first v0.2.0 launch (intentional
> reset; see §11 "Breaking changes").
> Audience: contributors and operators.

shell-agent-v2 has **four distinct memory facilities** in the
v0.2.0 design. This document is the single entry point: what
each one is, how they're created, where they live, and how they
get assembled into the LLM's system prompt.

For deep design rationale see:

- [memory-architecture-v2.md](memory-architecture-v2.md) — Records
  / contextbuild design (unchanged from v0.1.x)
- [memory-injection-hardening.md](memory-injection-hardening.md) —
  threat model & defenses (v0.1.26 Security Round 3, applied to
  Global Memory in v0.2.0)

---

## 1. Four Facilities at a Glance

| Facility | Scope | Stored at | What it holds | Set by |
|---|---|---|---|---|
| **Records** | per-session | `sessions/<id>/chat.json` (+ `summaries.json`) | Verbatim conversation history (immutable, append-only) | Agent loop on each turn |
| **Session Memory** | per-session | `sessions/<id>/session_memory.json` | Auto-extracted session-context facts (`fact` / `context` categories) | `extractMemories` after each assistant turn |
| **Findings** | per-session | `sessions/<id>/findings.json` | Data-analysis discoveries: anomalies, patterns, statistical observations | `promote-finding` tool (LLM, `hasData`-gated) and `analyze-data` auto-promote |
| **Global Memory** | cross-session (only global facility) | `global_memory.json` | User identity / decisions (`preference` / `decision` categories) | `extractMemories` after each assistant turn + manual Pin from Session Memory or Findings |

Naming notes:

- **"Pinned"** is gone. The act of "pinning" still exists as
  the user action for promoting a session-scoped item to Global
  Memory, but the **store** is called Global Memory.
- **`Pin to Global Memory`** is a UI button on Session Memory
  rows and Findings rows.

How each flows into the LLM context:

- Records → user/assistant/tool messages (after possible
  summarisation by `contextbuild`)
- Session Memory + Findings + Global Memory → injected into the
  **system prompt** (Session Memory and Findings only when their
  session is active)

---

## 2. Records (Conversation History)

The literal log of user / assistant / tool messages plus
metadata. Single source of truth for the session.

**Persistence**: append-only JSON at
`~/Library/Application Support/shell-agent-v2/sessions/<id>/chat.json`.
Atomic writes via `internal/atomicio.WriteFileAtomic`.

**Compaction model**: records are immutable. Older raw records
are folded into a derived summary in
`sessions/<id>/summaries.json` (content-keyed cache) when the
LLM context budget is exceeded. No record is ever rewritten.
The v0.1.x `Memory.UseV2` opt-in flag is **removed in v0.2.0**:
the contextbuild path is now the only path. Legacy
destructive-compaction code (`compactIfOverBudget`,
`compactMemoryIfNeeded`) is deleted, not just no-op'd.

**Time markers**: `[YYYY-MM-DD HH:MM TZ]` prefix at meaningful
boundaries (>30 min gap, tool/report roles). Summary blocks get
a range header.

**Tier field**: removed in v0.2.0. (Was already vestigial under
v0.1.26 Memory v2.)

**Privacy flag (v0.3.0)**: `Session.Private bool` is persisted
in `chat.json` with the `omitempty` JSON tag — legacy sessions
without the field load as `Private = false`. When `true`, the
session opts out of cross-session promotion: the
`extractMemories` pipeline drops `preference` / `decision`
facts (would otherwise route to Global Memory), the
`PromoteSessionMemoryToGlobal` and `PromoteFindingToGlobal`
handlers reject server-side, and the frontend hides the ★ Pin
buttons plus shows a 🔒 indicator on the sidebar row and as a
chat-pane banner. Privacy is fixed at session creation
(no mid-session toggle) so the boundary stays unambiguous.
Full design: [privacy-controls.md](privacy-controls.md) §2.

**Bundle portability (v0.4.0)**: a session can be packaged as
a single `.shellagent` ZIP via the sidebar Export icon or the
`/export` slash command. The bundle carries this `chat.json`
plus session memory, findings, summaries, sandbox `work/`,
the analysis DuckDB, and every objstore object the session
owns. Re-imported sessions get fresh sess-ids and fresh object
IDs (with all in-record references rewritten through the
`internal/sessionio` rewriter). Privacy flag is preserved
verbatim. Full design:
[session-import-export.md](session-import-export.md).

**Deep dive**: [memory-architecture-v2.md](memory-architecture-v2.md).

---

## 3. Session Memory (Auto-Extracted Session Context)

Auto-extracted facts about the *current* conversation that
don't rise to the level of cross-session user identity. Things
like "user is currently analysing Q1 sales data", "user
attached the Gouf model image", "user wants the report in
three sections".

**Schema** (`internal/memory/session_memory.go`):

```go
type SessionMemoryEntry struct {
    Fact            string    // English form
    NativeFact      string    // User's language
    Category        string    // fact | context  (subset of the 4 categories)
    SourceTime      time.Time
    CreatedAt       time.Time

    // Provenance
    SourceTurnIndex int
    Source          string    // user_turn | assistant_turn
    ToolOriginated  bool
}
```

`SessionID` is implicit (file is per-session). No `Source =
manual` value because manual session-context entry isn't a use
case (anything the user types directly is already in Records).

**Auto-extraction**: same `extractMemories` pass as Global
Memory. The LLM produces facts tagged with category; categories
`fact` / `context` route here.

**System prompt rendering** (current session only):

```
- [user-stated] [context] User is analysing 2025 Q1 sales data (ユーザーは2025年Q1売上データを分析中) (learned 2026-05-03)
- [derived] [fact] Three datasets are loaded: sales, customers, returns (3つのデータセットがロード済み) (learned 2026-05-03)
```

**Capacity**: per-session FIFO cap (default 50; lower than
Global Memory because per-session noise should be bounded
tightly).

**Lifecycle**: deleted with the session. The whole
`sessions/<id>/` directory is removed.

**Promotion**: each row in the sidebar Session Memory section
has a "Pin to Global Memory" button. Clicking opens a dialog
to confirm category (defaults to `decision`; user can change to
`preference`) and creates a new Global Memory entry. The
original Session Memory entry stays.

---

## 4. Findings (Data-Analysis Discoveries)

Insights derived **specifically from data analysis** — anomalies
in a loaded dataset, statistical patterns, structured
observations from `analyze-data`. Not auto-extracted from
arbitrary conversation: `promote-finding` is the only LLM path
and it is gated to sessions with loaded data.

**Schema** (`internal/findings/findings.go`):

```go
type Finding struct {
    ID              string    // f-YYYYMMDD-NNN[-hex]
    Content         string
    Tags            []string  // free-form; severity tags get coloured badges
    CreatedAt       string    // RFC3339
    CreatedLabel    string    // "2026-05-03 (Friday)"

    // Provenance
    Source          string    // llm_promoted | analyze_data
    ToolOriginated  bool
}
```

`SessionID` and `OriginSessionTitle` are removed — file is
per-session. Source `manual` is removed (no `/finding` slash
command in v0.2.0).

**Creation paths** (v0.2.0):

- **`promote-finding` tool** — LLM-callable, gated to sessions
  where `hasData == true` (i.e., at least one table loaded via
  `load-data`). MITL default ON: every promotion shows a
  confirmation dialog.
- **`analyze-data` auto-promote** — when sliding-window analysis
  surfaces structured `Finding` records, they get added in bulk.

**Removed in v0.2.0**:
- `/finding <text>` slash command (manual entry path) — Pin
  workflow + Session Memory cover this need.

**Capacity**: per-session FIFO cap (default 100 per session).

**Lifecycle**: deleted with the session.

**System prompt rendering** (current session only):

```
- [derived] [2026-05-03] 2025-03-09 Osaka Widget-C: 1850 units sold (50× weekly avg) — likely data error or bulk order
- [derived] [2026-05-03] Tokyo Widget-A shows consistent 130-unit weekly volume across all weeks
```

**Promotion**: each Finding row has a "Pin to Global Memory"
button (same UI affordance as Session Memory). Useful when an
analytical finding is actually a long-term decision worth
remembering across sessions ("we decided that Widget-C
demand is unpredictable").

**Dedicated UI panel**: Findings get their own pane in the
chat-pane area (alongside the Data disclosure), not the
sidebar. Rationale: Findings are tightly coupled to the loaded
data — looking at findings makes most sense next to the
dataset that produced them. The pane shows a flat list with
filter / search.

---

## 5. Global Memory (Cross-Session User Identity)

Long-lived facts the agent remembers about the user across
every session. The **only** facility that persists across
sessions in v0.2.0.

**Schema** (`internal/memory/global_memory.go`):

```go
type GlobalMemoryEntry struct {
    Fact            string    // English form
    NativeFact      string    // User's language
    Category        string    // preference | decision  (subset of the 4 categories)
    SourceTime      time.Time
    CreatedAt       time.Time

    // Provenance
    SessionID       string    // Originating session ID (or empty for manual entry)
    SourceTurnIndex int
    Source          string    // user_turn | assistant_turn | manual | promoted_from_session_memory | promoted_from_finding
    ToolOriginated  bool

    // Promotion back-reference (if Source is promoted_from_*)
    PromotedFromID  string    // Session Memory entry index, or Finding ID
}
```

**Categories** (`preference` / `decision` only):

| Category | Purpose | Example |
|---|---|---|
| `preference` | User preferences and habits | "User prefers Go over Python" |
| `decision` | Architectural / design decisions | "Chose DuckDB over SQLite for analysis" |

`fact` / `context` are session-scoped concepts (Session Memory).
They cannot be auto-routed here.

**Creation paths**:

- **Auto-extraction** (`extractMemories`): facts tagged
  `preference` or `decision` route here automatically.
- **Manual Pin from Session Memory**: user clicks "Pin to
  Global Memory" on a Session Memory row, optionally
  reclassifying to `preference`/`decision`.
- **Manual Pin from Finding**: same flow from the Findings
  panel.
- **Direct manual entry**: settings UI / API (`Set` method).

**System prompt rendering** (always injected):

```
- [user-stated] [preference] User prefers Go over Python (learned 2026-05-03)
- [user-stated] [decision] Chose DuckDB over SQLite (promoted from Finding, 2026-05-02)
```

**Capacity**: FIFO cap (default 100). Lower than v0.1.x's 100
applied to "Pinned" because Global Memory is now tighter
(only `preference`/`decision`).

**Sidebar display**: Memory tab → top section "Global Memory".
Rows show fact, category badge, trust badge, learned date.
Each row has a "Demote to Session Memory" button (rare; for
when you realise a global entry is actually session-bound).

---

## 6. Sidebar & UI Layout

**Memory sidebar** (existing tab, restructured):

```
┌── Sidebar / Memory tab ──────────┐
│                                  │
│ [Global Memory]                  │
│  • [user-stated] User prefers Go │
│  • [user-stated] Chose DuckDB    │
│  …                               │
│                                  │
│ [Session Memory]                 │
│  • [user-stated] User analysing  │
│    Q1 sales data                 │
│  • [derived] Three datasets…     │
│  • [Pin to Global Memory] btn    │
│  …                               │
│                                  │
└──────────────────────────────────┘
```

Two sections in the existing Memory sidebar tab — Global
Memory (top) and Session Memory (below). Bulk-select + delete
on each section. Pin button on Session Memory rows.

**Findings panel** (new, chat-pane area):

```
┌── Chat pane ─────────────────────┐
│ chat conversation                │
│                                  │
│ ┌── Data ──────────────────────┐ │
│ │ Tables / objects / /work     │ │
│ └──────────────────────────────┘ │
│ ┌── Findings ──────────────────┐ │ ← NEW dedicated panel
│ │ • [analyze-data] Q1 anomaly  │ │
│ │   in Osaka Widget-C…         │ │
│ │ • [promote-finding] Tokyo…   │ │
│ │ Filter: [all|critical|…]     │ │
│ │ [Pin to Global Memory] btn   │ │
│ └──────────────────────────────┘ │
└──────────────────────────────────┘
```

Sits next to the Data disclosure. List view with severity tag
filter, search, Pin button per row.

---

## 7. System Prompt Assembly

`chat.Engine.BuildSystemPrompt(globalMemoryContext,
sessionMemoryContext, findingsContext)` in
`internal/chat/chat.go`:

```
{base system prompt}

{temporal context}
{Location: ... if set}
{sandbox guidance, if Sandbox.Enabled}

Important facts you remember about the user:
{global_memory.FormatForPrompt()}

Notes about the current session:
{session_memory.FormatForPrompt()}

Analysis findings in this session:
{findings.FormatForPrompt()}
```

The result is the `system` message in the LLM call. Records do
**not** flow through here — they go in as separate
user/assistant/tool messages from `BuildMessages`
(`agent.buildMessagesV2`).

**Empty sections are omitted entirely** (no header without
content).

**Token budget**: each `FormatForPrompt` is internally bounded
to **16 KiB** with newest-first inclusion plus an elision
marker. Combined max system prompt addition: ~48 KiB worst
case.

---

## 8. Auto-Extraction (`extractMemories`)

Renamed from `extractPinnedMemories` to reflect the broader
routing. Runs as a post-response background task after every
assistant turn.

**Pipeline**:

1. Collect last 4 hot-tier records
2. Wrap with `nlk/guard.Tag` for prompt-injection isolation
3. Ask the same LLM to extract facts in format
   `category|turn-N|english fact|native expression`
4. For each returned line:
   - Drop if category is outside `{preference, decision, fact,
     context}` allowlist
   - Drop if `IsSelfReferential(fact)` matches
   - Determine source via `parseTurnToken` + content overlap
     refinement
   - **Route by category**:
     - `preference` / `decision` → `globalMemory.Add(...)`
     - `fact` / `context` → `sessionMemory.Add(...)`
5. Atomic save, fire `global_memory:updated` and/or
   `session_memory:updated` events to refresh the UI

The extraction prompt explicitly tells the LLM:

> Categories `preference` and `decision` describe long-term
> user identity and persist across all sessions. Categories
> `fact` and `context` describe the current conversation's
> state and disappear when the session ends. Choose the
> category that matches the durability you intend.

This makes the LLM's category choice carry scope intent.

---

## 9. Provenance & Trust Tags

(Same model as v0.1.26, applied to Global Memory and Session
Memory.)

| Source value | Trust tag | Meaning |
|---|---|---|
| `user_turn` | `[user-stated]` | Extracted from a user-role record |
| `manual` | `[user-stated]` | Pinned by user via UI |
| `promoted_from_session_memory` | `[user-stated]` | User Pin'd a Session Memory entry |
| `promoted_from_finding` | `[user-stated]` | User Pin'd a Finding |
| `assistant_turn` | `[derived]` | Extracted from an assistant-role record |
| `llm_promoted` | `[derived]` | Promoted by `promote-finding` tool |
| `analyze_data` | `[derived]` | Auto-promoted by `analyze-data` tool |
| empty (legacy) | `[derived]` | Lower trust default |

Findings: same Source enum (subset). Defenses (self-referential
filter, category allowlist, `nlk/guard` wrap) apply at
extraction time.

---

## 10. Capacity & Retention

**Per-store FIFO caps:**

| Store | Default cap | Config key |
|---|---|---|
| Global Memory | 100 | `Memory.MaxGlobalMemory` |
| Session Memory | 50 (per session) | `Memory.MaxSessionMemoryPerSession` |
| Findings | 100 (per session) | `Memory.MaxFindingsPerSession` |

**Render-time budget**: each `FormatForPrompt` clips at 16 KiB,
newest-first.

**Time-based decay**: still not implemented. (Future
consideration if Global Memory grows messy in practice; for
now FIFO is enough.)

---

## 11. Breaking Changes (v0.1.x → v0.2.0)

This is a **breaking architectural change**:

- `Memory.UseV2` config flag — **removed**. The contextbuild
  path (immutable Records + derived summary cache) is now the
  only path. Legacy v1 destructive-compaction code is deleted
  rather than gated by a flag — there is no more "v1 mode".
- `pinned.json` (global file) — **ignored on launch**. Old
  preferences/decisions are lost. Re-pin them via conversation
  or settings UI.
- `findings.json` (global file) — **ignored on launch**. Old
  cross-session findings are lost.
- Session files (`sessions/<id>/chat.json` +
  `summaries.json`) — **preserved**. You can still browse old
  conversations; they just don't have Session Memory or
  Findings attached (those facilities didn't exist in v0.1.x).
- `/finding` slash command — **removed**. Use the chat-pane
  Findings panel + Pin button.
- `PinnedFact` type — renamed to `GlobalMemoryEntry` (Go API).
- `PinnedStore` → `GlobalMemoryStore`.
- `SetPinnedHandler` → `SetGlobalMemoryHandler` and
  `SetSessionMemoryHandler` (both).
- Wails event `pinned:updated` → `global_memory:updated` and
  `session_memory:updated`.
- Bindings rename: `GetPinnedMemories` →
  `GetGlobalMemories`; `DeletePinnedMemories` →
  `DeleteGlobalMemories`. New: `GetSessionMemories`,
  `DeleteSessionMemories`, `PinSessionMemory`,
  `PinFinding`.

A first-launch banner notifies the user that the v0.1.x stores
were ignored (with a tip: `~/Library/Application
Support/shell-agent-v2/pinned.json` is preserved on disk if
they want to recover entries manually). The banner is
dismissible.

---

## 12. Storage Layout

```
~/Library/Application Support/shell-agent-v2/
├── global_memory.json                    # Global Memory (NEW name; was pinned.json)
├── sessions/
│   └── <session-id>/
│       ├── chat.json                     # Records (unchanged)
│       ├── summaries.json                # contextbuild SummaryCache (unchanged)
│       ├── session_memory.json           # NEW: Session Memory
│       └── findings.json                 # MOVED: per-session Findings
├── objects/                              # objstore (unchanged)
└── config.json
```

Legacy files left on disk:
- `pinned.json` — read access only by an opt-in recovery tool
  (out of scope for v0.2.0; users can `cat` it manually)
- `findings.json` — same

All new writes go through `internal/atomicio.WriteFileAtomic`.

---

## 13. Threat Model (v0.2.0 Updates)

Pre-v0.2.0 attack: a malicious CSV cell quoted by the assistant
gets auto-extracted, pinned globally, and re-injected into all
future sessions as authoritative context.

**v0.2.0 mitigations on top of v0.1.26 hardening**:

| Mechanism | Effect on attack |
|---|---|
| `fact` / `context` route to Session Memory, not Global Memory | A successful injection through a CSV cell only contaminates the originating session, not all future sessions |
| Session deleted ⇒ Session Memory + Findings deleted | Attack window closes when session ends |
| Global Memory only receives `preference` / `decision` | Attacker must convince the extraction LLM to label the payload as a user preference (harder than `context`) |
| `promote-finding` `hasData`-gated | Cannot be invoked outside data-loaded sessions |
| `/finding` slash removed | One less LLM-influenced manual surface |
| Pin requires explicit user click | No auto-promotion from session-scoped to global |

The v0.1.26 self-referential filter, category allowlist, and
`nlk/guard` wrap are applied to **both** auto-extraction
streams (Global Memory and Session Memory).

**Deep dive**: [memory-injection-hardening.md](memory-injection-hardening.md).

---

## 14. Glossary

- **Records** — verbatim conversation turns; per-session
- **Session Memory** — auto-extracted session context
  (`fact`/`context`); per-session
- **Findings** — data-analysis discoveries; per-session
- **Global Memory** — cross-session user identity
  (`preference`/`decision`); the only globally-persistent
  facility
- **Pin** / **Pin to Global Memory** — UI action to promote a
  Session Memory or Findings entry to Global Memory
- **Source / Provenance** — origin label used to derive trust
- **Trust tag** — `[user-stated]` (high) or `[derived]`
  (LLM-routed)

---

## 15. References

- [memory-architecture-v2.md](memory-architecture-v2.md) —
  Records / contextbuild (unchanged)
- [memory-injection-hardening.md](memory-injection-hardening.md) —
  Pinned/Findings security model (v0.1.26)
- `internal/memory/global_memory.go` — GlobalMemoryStore (NEW)
- `internal/memory/session_memory.go` — SessionMemoryStore (NEW)
- `internal/findings/findings.go` — per-session findings store
- `internal/contextbuild/` — Memory v2 ContextBuilder
- `internal/agent/agent.go:extractMemories` — auto-extraction
  routing
- `internal/chat/chat.go:BuildSystemPrompt` — composition
