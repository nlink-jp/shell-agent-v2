# Memory Model — shell-agent-v2

> Date: 2026-05-03
> Status: Current as of v0.1.28
> Audience: contributors and operators who need to understand
> how shell-agent-v2's memory pieces fit together. Deep-dive
> design documents are linked from each section.

shell-agent-v2 has **three distinct memory facilities** that
many users initially conflate. This document is the single
entry point: what each one is, how they're created, where they
live, and how they get assembled into the LLM's system prompt.

For deep design rationale see:

- [memory-architecture-v2.md](memory-architecture-v2.md) — Records
  / contextbuild design
- [memory-injection-hardening.md](memory-injection-hardening.md) —
  threat model & defenses (v0.1.26 Security Round 3)

---

## 1. Three Facilities at a Glance

| Facility | Scope | Stored at | What it holds | Set by |
|---|---|---|---|---|
| **Records** | per-session | `sessions/<id>/chat.json` (+ `summaries.json`) | Verbatim conversation history (immutable, append-only) | Agent loop on each turn |
| **Pinned Memory** | cross-session | `pinned.json` | Long-term facts about the user (preferences, decisions, context) | `extractPinnedMemories` after each assistant turn (auto), `Set()` (manual) |
| **Findings** | cross-session | `findings.json` | Analysis insights worth reusing (anomalies, patterns, decisions about data) | `promote-finding` tool (LLM), `/finding` slash (user), `analyze-data` auto-promote |

All three flow into the LLM context, but through **different
channels**:

- Records → user/assistant/tool message turns (after possible
  summarization by `contextbuild`)
- Pinned + Findings → injected into the **system prompt**

---

## 2. Records (Conversation History)

The literal log of user / assistant / tool messages plus
metadata (timestamps, attached image object IDs, tool-call
records). Single source of truth for the session.

**Persistence**: append-only JSON at
`~/Library/Application Support/shell-agent-v2/sessions/<id>/chat.json`.
Atomic writes via `internal/atomicio.WriteFileAtomic`.

**Compaction model** (v0.1.26+ default `Memory.UseV2: true`):
records are **immutable**. They are never replaced by summary
records. When the LLM context budget is exceeded, older raw
records are folded into a **derived summary** that lives in a
separate cache (`sessions/<id>/summaries.json`), keyed by a
content hash so it can be reused across runs and across
backends. The legacy v1 path (in-place `Tier` mutation,
`PromoteOldestHotToWarm`) is preserved for old session files
but no longer touched on writes.

**Time markers**: each record gets a `[YYYY-MM-DD HH:MM TZ]`
prefix only at meaningful boundaries (>30 min gap, tool/report
roles). Summary blocks get a range header
`[Summary of N earlier turn(s) — from … to …]`.

**Tier field**: legacy artefact. Not written by new code, not
read by the v2 path. Older sessions on disk that contain
`Tier=hot/warm/cold` records still load and render.

**Deep dive**: [memory-architecture-v2.md](memory-architecture-v2.md).

---

## 3. Pinned Memory (Cross-Session User Facts)

Long-lived facts the agent remembers about the user across
every session. These persist even after a session is deleted.

**Schema** (`internal/memory/pinned.go`):

```go
type PinnedFact struct {
    Fact            string    // English form (for analysis)
    NativeFact      string    // User's language (for display)
    Category        string    // preference | decision | fact | context
    SourceTime      time.Time // When the original conversation occurred
    CreatedAt       time.Time // When pinned

    // Provenance (v0.1.26+)
    SessionID       string    // Originating session ID
    SourceTurnIndex int       // Index in that session's Records
    Source          string    // user_turn | assistant_turn | manual
    ToolOriginated  bool      // Surrounding window included tool output
}
```

**Categories** (LLM-assigned, allowlist-enforced):

| Category | Purpose | Example |
|---|---|---|
| `preference` | User preferences and habits | "User prefers Go over Python" |
| `decision` | Architectural / design decisions | "Chose DuckDB over SQLite" |
| `fact` | Factual context | "User is in Tokyo" |
| `context` | Situational awareness | "User analyses Q1 sales data" |

**Auto-extraction** (`extractPinnedMemories`, runs after every
assistant turn):

1. Take the last 4 hot-tier records (mixture of user/assistant)
2. Number them `[turn N|role]:`, wrap with `nlk/guard.Tag` to
   isolate as data
3. Ask the LLM (same backend) to extract facts in the format
   `category|turn-N|english fact|native expression`
4. For each returned line:
   - Drop if category is outside the allowlist
   - Drop if `IsSelfReferential(fact)` matches (talks about the
     assistant, model, system prompt, THINK tag, tools, etc.)
   - Determine source: parse `turn-N` → originating role; if
     unparseable or origin is assistant but the fact's English
     keywords / Japanese trigrams overlap with a user turn,
     attribute to `user_turn` (content-based attribution
     refinement)
5. Add via `pinned.Add()`, dedup by exact `Fact` text, FIFO
   evict if over `MaxPinnedFacts` (default 100)
6. Save with atomic write, fire `pinned:updated` event so the
   sidebar refreshes in real time

**Manual operations**:

- `Set(key, content)` — UI manual pin, stamped `Source: manual`
- `Delete(key)` / `DeleteByKeys([]string)` — UI manual delete

**System prompt rendering** (`FormatForPrompt`):

```
- [user-stated] [preference] User prefers Go over Python (ユーザーはPythonよりGoを好む) (learned 2026-05-03)
- [derived] [context] User is currently analysing Q1 sales data (learned 2026-05-03)
```

The leading `[user-stated]` / `[derived]` is a **trust tag**
derived from `Source`. See §6.

**Sidebar display**: each row shows fact, category badge, trust
badge, and `learned YYYY-MM-DD` (v0.1.28+).

**Deep dive**: [memory-injection-hardening.md](memory-injection-hardening.md)
for filters and threat model.

---

## 4. Findings (Cross-Session Analysis Insights)

Insights derived from data analysis, worth surfacing across
sessions. Findings carry an `OriginSessionID` so the user can
trace where an insight came from.

**Schema** (`internal/findings/findings.go`):

```go
type Finding struct {
    ID                 string   // f-YYYYMMDD-NNN[-hex]
    Content            string
    OriginSessionID    string
    OriginSessionTitle string
    Tags               []string // freeform; severity tags ("critical", "high", …) get coloured badges
    CreatedAt          string   // RFC3339
    CreatedLabel       string   // "2026-05-03 (Friday)"

    // Provenance (v0.1.26+)
    Source         string  // llm_promoted | manual
    ToolOriginated bool
}
```

**Creation paths**:

- **`promote-finding` tool** — LLM-callable. Default MITL **on**
  (v0.1.26 hardening): every promotion shows a confirmation
  dialog with the proposed content. Source: `llm_promoted`.
- **`/finding <text>` slash command** — user manual. Source:
  `manual`.
- **`analyze-data` auto-promote** — when sliding-window analysis
  surfaces structured `Finding` records in its result, they get
  added in bulk. Source: `llm_promoted`.

All three call sites notify the frontend via `findings:updated`
(v0.1.28+) so the sidebar refreshes immediately instead of
waiting for a session switch.

**Capacity**: FIFO eviction at `MaxFindings` (default 200).
Daily ID counter rolls over to `f-YYYYMMDD-NNNNNN-<6 hex>` past
999/day.

**System prompt rendering**:

```
- [derived] [2026-05-03 (Friday)] 2025年Q1において大阪のWidget-Cで… (from: Sales Analysis, session: sess-1234)
- [user-stated] [2026-05-02 (Thursday)] User identified anomaly in… (from: Manual Review, session: sess-5678)
```

**Sidebar display**: content, trust badge, date, originating
session title, severity-coloured tag badges.

---

## 5. System Prompt Assembly

`chat.Engine.BuildSystemPrompt(pinnedContext, findingsContext)`
in `internal/chat/chat.go:110` is the single composition point:

```
{base system prompt}

{temporal context}
{Location: ... if set}
{sandbox guidance, if Sandbox.Enabled}

Important facts you remember about the user:
{pinned.FormatForPrompt()}

Analysis findings from other sessions:
{findings.FormatForPrompt()}
```

The result is the `system` message in the LLM call. Records do
**not** flow through here — they go in as separate
user/assistant/tool messages from `BuildMessages`
(`agent.buildMessagesV2` for the v2 path).

**Side effect**: `BuildSystemPrompt` rotates the `nlk/guard.Tag`
nonce so subsequent `WrapUserToolContent` calls in the same
turn use a fresh nonce.

**Token budget**: each `FormatForPrompt` is internally bounded
to **16 KiB** with newest-first inclusion plus an elision marker
(`(N earlier facts elided to fit budget)`). This prevents an
oversize Pinned/Findings store from crowding out the
conversation budget.

---

## 6. Provenance & Trust Tags

Every Pinned and Finding entry carries a `Source` field. The
trust tag is derived (`pinned.go:trustTag`,
`findings.go:trustTag`):

| Source value | Trust tag | Meaning |
|---|---|---|
| `user_turn` | `[user-stated]` | Extracted from a user-role record (or content-overridden to user) |
| `manual` | `[user-stated]` | Pinned/promoted by user via UI or slash command |
| `assistant_turn` | `[derived]` | Extracted from an assistant-role record — content traces through the LLM |
| `llm_promoted` | `[derived]` | Promoted by an LLM tool call — content originated from the LLM's analysis |
| empty (legacy) | `[derived]` | Pre-v0.1.26 entry without Source — defaults to lower trust |

**Why this matters**: anything that ever passes through an
LLM-generated turn might carry attacker-influenced bytes (a
quoted CSV cell, an MCP response, image OCR, a fetched web
page). The `[derived]` tag warns future LLM contexts that the
content is not necessarily user-stated. See §9.

---

## 7. Capacity & Retention

Current state: **FIFO caps only**. No time-based decay. No
importance scoring beyond Category.

| Setting | Default | Where |
|---|---|---|
| `Memory.MaxPinnedFacts` | 100 | `internal/config/config.go` |
| `Memory.MaxFindings` | 200 | same |
| `PinnedFormatBudget` | 16 KiB | `internal/memory/pinned.go` |
| `FindingsFormatBudget` | 16 KiB | `internal/findings/findings.go` |

**Eviction policy**: oldest entry first (FIFO) on `Add` overflow.
A future `(B)` importance score or `(C)` per-category TTL is
documented in the memory plan but not implemented.

`PinnedFormatBudget` / `FindingsFormatBudget` apply at render
time; even if the store has 100 entries, only the most recent
that fit in 16 KiB are emitted to the system prompt.

---

## 8. Storage Layout

```
~/Library/Application Support/shell-agent-v2/
├── pinned.json            # PinnedStore — global cross-session facts
├── findings.json          # findings.Store — global cross-session insights
├── sessions/
│   └── <session-id>/
│       ├── chat.json      # session.Records (immutable log)
│       └── summaries.json # contextbuild SummaryCache (derived, regeneratable)
├── objects/               # objstore: images, reports, blobs (16-byte IDs)
└── config.json
```

All JSON writes go through `internal/atomicio.WriteFileAtomic`
(tmp + rename + parent-dir fsync) so a crash mid-write leaves
the previous file intact (security-hardening-2.md C4 / H10).

---

## 9. Threat Model (Summary)

Pinned facts and findings are re-injected into every future
session's system prompt as authoritative context. That makes
them a structural prompt-injection vector: anything that ever
appears in an *assistant* turn — including assistant-summarised
tool output — can be picked up by `extractPinnedMemories` or by
a `promote-finding` LLM call and then steer every future
session.

**v0.1.26 defenses** (Security Round 3):

| Defense | Layer |
|---|---|
| Provenance attribution | Every fact carries `Source` so trust is recoverable |
| Self-referential filter | `IsSelfReferential` drops "the assistant / THINK / system prompt / …" facts at extraction |
| Category allowlist | Only `preference|decision|fact|context` survive |
| `nlk/guard` wrap | Extraction prompt treats conversation tail as data |
| `promote-finding` MITL ON | LLM-promoted findings need user confirmation |
| FIFO retention caps | Bounded store growth |
| 16 KiB FormatForPrompt budget | Bounded prompt growth |

Auto-extraction itself remains on (per-pin MITL would destroy
chat UX). Residual risk on the auto-extraction path is
recoverable via the sidebar audit + bulk-delete UI.

**Deep dive**: [memory-injection-hardening.md](memory-injection-hardening.md).

---

## 10. Design — Two-Tier Scope (Proposed, post-v0.1.28)

> Status: **design only**, not yet implemented.

The current model treats *every* pinned fact and finding as
**global** — visible in every future session's system prompt.
This works but pressures the global pool: tasks-context items
("user has 3 datasets loaded for Q1 analysis") consume the same
budget as identity items ("user prefers Go").

The proposed evolution introduces a `Scope` field with two
values:

- **`global`** — current behaviour. Always injected into every
  session.
- **`session`** — only injected when the originating session is
  the active one. Auto-deleted with the session.

**Default mapping by category** (Pinned):

| Category | Default scope |
|---|---|
| `preference` | global |
| `decision` | global |
| `fact` | session |
| `context` | session |

**Findings**: default `session` for all (analyses are
case-bound). Manual promotion to global if cross-case relevance.

**Manual override**: sidebar each row gets a `[global]` /
`[session]` badge plus a "Pin to global" button (session →
global) and an "Unpin" toggle (global → session).

**Extraction prompt revision**: the LLM is told the category →
scope mapping so its category choice carries scope intent.

**Backwards compat**: existing entries (no `Scope` field)
default to `global` — no data loss, current behaviour preserved.

**Open questions** (to be resolved in the next planning round):

- Should there be auto-promotion (e.g., same fact appears in N
  sessions → bump to global)?
- Storage: single file with `Scope` field vs split files?
- Sidebar layout: two sub-sections (Global / This Session) vs
  single list with badge filter?

When this design moves to implementation, this section will be
updated to reflect what was actually built and the open
questions resolved.

---

## 11. Glossary

- **Records** — verbatim conversation turns; per-session
- **Pinned Memory** — cross-session facts about the user
- **Findings** — cross-session analysis insights
- **Pin to global** — (proposed) promote a session-scoped item
  to global scope
- **Scope** — (proposed) `global` (cross-session) or `session`
  (session-bound)
- **Source / Provenance** — origin label (`user_turn` etc.)
  used to derive the trust tag
- **Trust tag** — `[user-stated]` (high trust) or `[derived]`
  (LLM-routed; potentially attacker-influenced)
- **Tier** — legacy hot/warm/cold field on Records, vestigial
  under Memory v2
- **Memory v2 / `UseV2`** — the contextbuild path: records
  immutable, summaries derived per-call from a content-keyed
  cache

---

## 12. References

- [memory-architecture-v2.md](memory-architecture-v2.md) —
  Records, contextbuild, summary cache design
- [memory-injection-hardening.md](memory-injection-hardening.md) —
  Pinned/Findings security model (v0.1.26)
- `internal/memory/pinned.go` — PinnedStore implementation
- `internal/findings/findings.go` — findings.Store implementation
- `internal/contextbuild/` — Memory v2 ContextBuilder
- `internal/agent/agent.go:1820+` — `extractPinnedMemories`
- `internal/chat/chat.go:110` — `BuildSystemPrompt` composition
