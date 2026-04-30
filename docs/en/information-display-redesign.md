# Information Display Redesign — Design Document

> Date: 2026-04-30
> Status: Draft — pending implementation
> Scope: Sidebar tabs, chat-pane data view, status surfaces

## 1. Problem Statement

Information surfaces have grown ad-hoc through v0.1.x and now mix
concerns in ways that confuse both users and code. Concretely:

- The sidebar `status` panel groups three unrelated things —
  Findings (global), Pinned Memory (global), Tokens (current
  session's last call) — under a generic name with no shared scope.
- The sidebar `objects` panel shows every object across every
  session, so as session count grows the list becomes a hay-stack;
  the LLM-facing `list-objects` already filters per-session
  (`tools.go:548-552`) but the UI does not, creating an asymmetry
  between what the user sees and what the LLM sees.
- DuckDB tables loaded into a session are invisible until the LLM
  is asked. After app restart and session reopen, users have no UI
  to confirm "my data is still there".
- Sandbox `/work` listings exist only via the `sandbox-info` tool;
  there is no UI surface.
- Token telemetry sits inside a content panel even though it is
  not navigable content.

Root cause: each new feature was bolted onto whichever panel had
free space rather than considering the user's mental model.

## 2. Design Principles

1. **Scope by location.** Sidebar holds information that persists
   across session switches (global). The chat pane holds
   information bound to the currently-selected session. Switching
   sessions visually resets the chat pane and leaves the sidebar
   stable.
2. **One concept per panel.** A panel name must describe a single
   class of thing. "status" violates this; "Memory" does not.
3. **Storage layout is independent from UX.** The central
   object store stays as physical layout (content-addressed dedup,
   cross-session export resolution). User-visible surfaces are
   per-session everywhere.
4. **Telemetry is not navigation.** Token counts and similar
   live-updating numbers go to a peripheral strip, not a
   navigable panel.
5. **Empty is invisible.** A section with zero items collapses
   away rather than rendering "(empty)" placeholder rows.

## 3. Final Layout

```
┌────────────────────┐  ┌────────────────────────────────────┐
│  Sidebar           │  │  Chat Pane                         │
│                    │  │                                    │
│  ▣ Sessions        │  │  [Session: Q3 Sales Analysis]      │
│  ▣ Memory          │  │  ┌── Data ▾ ────────────────────┐  │
│                    │  │  │  Objects (3)                 │  │
│                    │  │  │   ▸ chart.png    234 KB       │  │
│                    │  │  │   ▸ report.md      4 KB       │  │
│                    │  │  │  Tables (2)                  │  │
│                    │  │  │   ▸ sales       1.0M rows     │  │
│                    │  │  │   ▸ customers   10K rows      │  │
│                    │  │  │  /work (5 files)             │  │
│                    │  │  │   ▸ data.csv      1.2 MB      │  │
│                    │  │  └────────────────────────────┘  │
│                    │  │                                    │
│                    │  │  …chat messages…                  │
│                    │  │                                    │
│                    │  │  ┌── footer strip ───────────────┐ │
│                    │  │  │ vertex_ai · hot 12 · 4.2k tok │ │
│                    │  │  └─────────────────────────────┘ │
└────────────────────┘  └────────────────────────────────────┘
```

### 3.1 Sidebar — Sessions

Unchanged. Lists all sessions, click to switch, supports
new-session.

### 3.2 Sidebar — Memory

Replaces the current `status` panel. Contents: Findings (global)
+ Pinned Memory (global). Both are bulk-selectable / deletable
(existing UI). Token stats and Object listings are removed from
this panel.

### 3.3 Chat Pane — Data Disclosure

A collapsible `<details>`-style block at the top of the chat
panel. Contents are scoped to the currently-selected session.
Three sub-sections:

- **Objects** — items from `ListBySession(currentSessionID)`,
  filtered by type (image / blob / report). Click an image to
  open the existing lightbox; click a blob to download; click a
  report to open in the existing report viewer. Bulk
  selection / delete moved here from the old `objects` sidebar
  panel.
- **Tables** — DuckDB tables in the session's analysis engine.
  Each row: name, row count, column list. Click a table to
  preview the first 20 rows in a modal (read-only). No editing.
- **/work** — sandbox files, listed via the same
  `sandbox.WorkDir(sid)` walk that powers `sandbox-info`. Read
  only. Hide entirely if the sandbox engine is disabled.

The whole disclosure is collapsed by default; the section header
shows the total count ("Data (10)") so users see at a glance
whether anything is loaded.

If all three sub-sections are empty for a freshly-created session,
the disclosure renders as a single muted line "Data — empty"
rather than a heavy collapsed bar.

#### 3.3.1 Sub-section layout: stacked vs. tabbed

Phase 1–4 use a **vertically stacked** layout (Objects → Tables →
/work in one open panel). This is the simplest correct form when
each section has a handful of items.

A tabbed variant — three tabs at the top of the disclosure body
showing one section at a time — is the planned evolution path
for sessions where any single section grows long enough to push
the others off-screen (typically when Tables ≥ 5 or /work
file-count gets into the dozens). Switching to tabs is purely a
rendering change in the `DataDisclosure` component; the
underlying bindings and data model don't move. Tracked as Phase
6 in §6.

### 3.4 Chat Pane — Footer Strip

Single horizontal line below the message list, above the input.
Renders the current backend, hot/warm message counts, and the
last call's prompt/output token totals. Compact (one line, muted
text). Matches the existing data flow but moves the surface out
of the sidebar.

### 3.5 Settings Modal

Unchanged. Already separated from the navigation hierarchy.

## 4. Backend Bindings

### 4.1 Existing — keep as is

- `b.objects.ListBySession(sid)` — already exists, used by
  `toolListObjects`. Reuse for the UI Objects sub-section.
- `b.objects.LoadAsDataURL(id)` — used by lightbox. Unchanged.
- `b.objects.DeleteObjects(ids)` — used by bulk delete. Unchanged.
- `analysis.Engine.Tables()` — already exists. Returns
  `[]*TableMeta` with name + columns + row count.
- `analysis.Engine.QuerySQL(query)` — used by LLM, reuse for
  preview.

### 4.2 New — to be added

| Binding | Purpose | Backed by |
|---|---|---|
| `GetSessionObjects(sessionID) []ObjectInfo` | UI Objects sub-section listing | `objects.ListBySession` |
| `GetSessionTables(sessionID) []TableInfo` | UI Tables listing | `analysis.Tables()` |
| `PreviewTable(name string, limit int) PreviewResult` | First-N rows for the preview modal | `analysis.QuerySQL("SELECT * FROM " + name + " LIMIT ?")` |
| `GetWorkFiles(sessionID) []WorkFile` | UI /work listing | walk `sessions/<id>/work/` directly (host-side mount; no container hop, same as `sandbox-info`) |

DTOs:

```go
type TableInfo struct {
    Name     string   `json:"name"`
    RowCount int64    `json:"row_count"`
    Columns  []string `json:"columns"`
    Comment  string   `json:"comment,omitempty"`
}

type PreviewResult struct {
    Columns []string         `json:"columns"`
    Rows    [][]any          `json:"rows"`
    Total   int64            `json:"total"`
    Truncated bool           `json:"truncated"`
}

type WorkFile struct {
    Path  string `json:"path"`  // relative to /work
    Size  int64  `json:"size"`
    MTime int64  `json:"mtime"` // unix ms
}
```

`PreviewTable` enforces a hard cap (default 200 rows) to prevent
the frontend from receiving multi-megabyte payloads. Larger
explorations stay in the LLM-mediated `query-sql` flow.

## 5. Frontend Changes

### 5.1 Component Tree

```
App
├── Sidebar
│   ├── SessionsPanel        (unchanged)
│   └── MemoryPanel          (renamed from StatusPanel; trimmed)
└── ChatPane
    ├── SessionHeader
    ├── DataDisclosure       (NEW)
    │   ├── ObjectsSection   (moved from OldObjectsPanel)
    │   ├── TablesSection    (NEW)
    │   └── WorkFilesSection (NEW)
    ├── MessageList          (unchanged)
    ├── FooterStrip          (NEW; was inside StatusPanel as Tokens)
    └── InputBar             (unchanged)
```

### 5.2 Removals

- The third sidebar nav button ("Objects") — its content moves
  into `DataDisclosure > ObjectsSection`.
- `Tokens` section inside the old `status` panel — moves to
  `FooterStrip`.

### 5.3 New Sections

- `TablesSection`: list rows; click opens `PreviewTable` modal
  showing column headers + first 20 rows.
- `WorkFilesSection`: list rows; read-only. No download / preview
  yet (filed in §8 Open Questions).
- `FooterStrip`: 1-line muted text, refreshed on agent state
  change events.

### 5.4 State Management

The `DataDisclosure` component subscribes to:
- session switch event → refetch all three sections
- agent done / tool result events → refetch (for newly-loaded
  data, written objects, sandbox files)
- explicit "refresh" button on each section header for the user

To avoid spam, refetch is debounced 500ms.

## 6. Implementation Phases

Each phase is a self-contained commit / PR that leaves the app
shippable.

### Phase 1 — Backend bindings

- Add `GetSessionTables`, `PreviewTable`, `GetWorkFiles` bindings
  + DTOs.
- Unit tests for each (use t.TempDir + fake session).
- No frontend changes yet. Existing `Objects` panel keeps
  working; `list-objects` LLM tool is unaffected.

### Phase 2 — Data disclosure component (read-only)

- Add `DataDisclosure` with three sub-sections, all read-only.
- Render at top of chat pane, default collapsed.
- Keep existing `Objects` sidebar panel for now (parallel).
- Manual smoke: open session with data → see tables; sandbox on
  + ran code → see /work.

### Phase 3 — Move Objects to DataDisclosure

- Migrate the bulk-select + delete UI from sidebar `Objects` panel
  into the new `ObjectsSection` of `DataDisclosure`.
- Filter to current session via `GetSessionObjects(sid)` (new
  binding wrapping `ListBySession`, or reuse `GetObjects` if it
  already takes a sessionID).
- Remove the third sidebar nav button.
- Remove the now-unused `objects` sidebar panel JSX.

### Phase 4 — Memory rename + footer strip

- Rename `status` panel → `Memory`. Remove Tokens section from it.
- Add `FooterStrip` to chat pane bottom; render the data
  previously shown by Tokens.
- Keep `GetLLMStatus` binding identical — only the rendering
  location changes.

### Phase 5 — Documentation & terminology cleanup

- `docs/en/object-storage.md` (and JA twin): replace "central
  object repository" wording with "session objects" in the
  user-facing sections; keep "central blob store" only where the
  document is describing physical layout.
- `README.md` / `README.ja.md`: drop or rewrite any "central
  object repository" mention.
- `AGENTS.md`: update the Sidebar section to match the new layout.

### Phase 6 — Tabbed sub-sections (conditional, post-MVP)

Refactor `DataDisclosure`'s body from a vertical stack to a tab
strip (Objects / Tables / /work) showing one section at a time.
Trigger this phase when real-world sessions start having enough
tables or `/work` files that the stacked layout pushes other
sections off the screen — i.e. when an actual user complaint or
observation surfaces, not preemptively. The component contract
(props, bindings, refresh logic) stays unchanged; only the
inner layout switches.

## 7. Migration & Compatibility

- **No data migration.** `<DataDir>/objects/`, session DuckDB
  files, and sandbox `/work` directories are untouched.
- **No breaking config / API changes.** The Wails binding surface
  gains four new methods; nothing is removed.
- **LLM tools unchanged.** `list-objects` already filters
  per-session, so the model's view doesn't change. `query-sql`,
  `sandbox-info`, etc. work as before.
- **Existing user reports / saved markdown** — `object:ID`
  references still resolve via the unchanged `LoadAsDataURL`
  path, including `resolveObjectRefsForExport` for SaveReport.

## 8. Open Questions

1. **Where exactly does `DataDisclosure` render?** Above the
   messages (top-of-pane) vs. as a right-hand split. Default
   plan is top-of-pane collapsed. Side-split would be cleaner
   for wide screens but doubles layout complexity. Defer
   side-split until phase 6+ if at all.
2. **/work file actions.** Phase 1 is read-only. Should we add
   "open in default app" or "copy to clipboard" later? Probably
   yes for text files, no for binaries (security). Tracked as
   future work.
3. **Cross-session object lookup UI.** Currently zero UX flow
   that displays an object from a different session than the
   active one. Decision: leave the `LoadAsDataURL` global
   resolver in place (export needs it) but add no UI for
   browsing across sessions. If real demand emerges, a "Search
   all sessions" panel can be added later, conceptually as a
   global *search* surface rather than a global *list*.
_(Resolved during review: the originally-listed footer strip
overflow concern is accepted as "wrap to two lines is fine, no
media query needed". Item removed.)_

## 9. Test Plan

Per phase:

- **Phase 1**: bindings unit tests; `make test` green.
- **Phase 2**: spawn a dev session, load CSV via `load-data`, run
  some sandbox code, confirm DataDisclosure shows accurate
  counts; collapse/expand state persists across renders.
- **Phase 3**: bulk-delete objects from inside Data; confirm
  deleted IDs are gone from disk and from LLM `list-objects`
  output.
- **Phase 4**: switch sessions; footer updates; memory panel no
  longer shows tokens.
- **Phase 5**: docs grep clean for "central object repository"
  in user-facing sections.

Final: full app rebuild via `make build`; run on a session that
exercises all three Data sub-sections to eyeball the layout.

## 10. Out of Scope

- Search across sessions (deferred).
- Editing tables or `/work` files from the UI.
- Real-time updates without a refetch (websocket-style live
  push). The 500ms debounce on agent events is enough.
- Settings panel reorganization (separate concern).
