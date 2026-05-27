# ADR-0027: Global Memory export / import

- Status: Accepted
- Deciders: magi
- Related: ADR-0001 (session import/export — the file-dialog + envelope precedent), ADR-0019 (LLM-driven memory tool), ADR-0028 (drops the unused provenance fields, removing the cross-machine hazard this feature would otherwise have to sanitise), `docs/en/reference/memory-model.md` (4-facility model)

## 1. Context

Global Memory is the only **cross-session** memory facility: user-identity
preferences and durable decisions persisted in
`{DataDir}/global_memory.json` as a JSON array of `GlobalMemoryEntry`
(`app/internal/memory/global_memory.go`). It survives across sessions and
is injected into every system prompt.

Today there is no way to move Global Memory off the machine. Sessions can
be exported/imported as `.shellagent` bundles (ADR-0001), but that flow
**deliberately excludes** Global Memory (ADR-0001 §5.3). A user requested
the ability to **back up Global Memory and carry it to another machine**.

Constraints from the existing store:

- Entries are keyed by `Fact` text; dedup is exact-string. There are no
  IDs or embeddings (nothing to regenerate on import).
- `SourceTime` / `CreatedAt` are user-facing ("learned YYYY-MM-DD").
- After ADR-0028, an entry's only fields are `Fact`, `NativeFact`,
  `Category`, `SourceTime`, `CreatedAt`, `Source`, `ToolOriginated` — all
  portable; no machine-local session back-references remain to sanitise.
- `GlobalMemoryStore.Add` already dedups by `Fact`, stamps zero
  timestamps with `time.Now()`, and FIFO-evicts past `MaxEntries`
  (`global_memory.go:126`).

## 2. Decision

Add a dedicated **Global Memory export/import**, separate from session
bundles, surfaced as two buttons in the sidebar Memory tab.

### 2.1 File format — versioned envelope

A single JSON file (default extension `.json`) wrapping the entries in a
self-describing envelope:

```json
{
  "kind": "shell-agent-v2-global-memory",
  "schema_version": 1,
  "exported_at": "2026-05-27T10:00:00Z",
  "exported_by_app_version": "0.15.0",
  "entries": [ { /* GlobalMemoryEntry verbatim */ } ]
}
```

| Field | Purpose |
|-------|---------|
| `kind` | Discriminator. Import rejects any file whose `kind` is not `shell-agent-v2-global-memory`, so a session bundle or arbitrary JSON cannot be mis-imported. |
| `schema_version` | `1`. Import rejects any other value ("unsupported global-memory export schema version: N"); no migration logic in v1. |
| `exported_at` | RFC3339 UTC. Informational. |
| `exported_by_app_version` | Producing app version. Informational. |
| `entries` | The `GlobalMemoryEntry` slice, **verbatim** — every field including provenance, so an export is a faithful snapshot. |

Rationale for the envelope over a raw `[]GlobalMemoryEntry` array (the
rejected alternative): the `kind`/`schema_version` pair lets import fail
fast and legibly on the wrong file, and leaves room for future format
changes — at the cost of one wrapper object, which is acceptable.

### 2.2 Export

Serialise `GlobalMemoryStore.All()` into the envelope and write it to a
user-chosen path via the Wails `SaveFileDialog` (the ADR-0001 pattern).
Default filename `global-memory-<YYYYMMDD-HHMMSS>.json`. No state-machine
gate is needed: Global Memory is not session-scoped and is not held open
by the analysis engine; reads are a cheap in-memory snapshot.

### 2.3 Import — merge, skip duplicates

Parse + validate the envelope, then fold each entry into the live store
via the existing `Add` semantics: **append new facts, skip any whose
`Fact` text already exists.** This is the safest policy (chosen over
overwrite and wholesale replace): importing never destroys or mutates a
fact the user already has, so re-importing the same file, or importing
several files in sequence, is idempotent and non-destructive.

Per-entry handling on import:

- **Dedup**: exact `Fact`-text match against the current store → skip
  (counts toward `skipped`).
- **Timestamps**: preserved from the file. `Add` only stamps *zero*
  timestamps, so a faithful `SourceTime`/`CreatedAt` survives; an entry
  exported without timestamps gets stamped at import time.
- **Category**: coerced to `decision` if not in
  `ValidGlobalMemoryCategories` (mirrors `Set`), keeping the store
  invariant.
- **Empty `Fact`**: skipped (counts toward `skipped`); an empty fact is
  not a usable memory.
- **All remaining fields are portable** (ADR-0028 removed the machine-local
  session back-references), so import folds each entry verbatim — there is
  nothing to sanitise. `Source` (and thus the `[user-stated]` /
  `[derived]` trust tag) and `ToolOriginated` are preserved as-is.

Import returns a summary `{ added, skipped }` so the UI can report
"Imported N facts (M duplicates skipped)". After import the store is
`Save()`d atomically and the frontend re-fetches via `GetGlobalMemories()`
(same refresh path the direct-edit UI already uses — no new event needed).

### 2.4 UI

Two buttons in the sidebar Global Memory section header
(`frontend/src/sidebar/Sidebar.tsx`), beside the existing bulk-action
affordances:

- **Export** → `Bindings.ExportGlobalMemory()` → save dialog.
- **Import** → `Bindings.ImportGlobalMemory()` → open dialog → the binding
  reports the outcome ("Imported N facts, M skipped") or the rejection
  reason via a native `wailsRuntime.MessageDialog`, then the frontend
  refreshes the list. (Implementation note: JS `window.alert()` is not
  reliably rendered in the Wails v2 WKWebView — the app uses native
  dialogs / inline UI elsewhere for the same reason — so feedback is shown
  natively from the Go binding rather than via `alert()`.)

## 3. Consequences

- Global Memory becomes portable for backup and cross-machine migration —
  closing the gap ADR-0001 left open.
- Import is non-destructive and idempotent; there is no way to lose
  existing facts through import.
- Because entries carry no session back-reference at all (ADR-0028), the
  cross-machine collision question does not arise here — there is no
  machine-local data to mishandle.
- No new runtime event; the frontend refresh reuses `GetGlobalMemories()`.
- `MaxEntries` FIFO eviction still applies during import — importing more
  than the cap evicts oldest-first, exactly as normal `Add` does.

## 4. Implementation

- `app/internal/memory/global_memory.go`
  - `GlobalMemoryExport` struct (envelope) + `GlobalMemoryExportKind`,
    `GlobalMemoryExportSchemaVersion` consts.
  - `MarshalGlobalMemoryExport(entries, appVersion string) ([]byte, error)`
    — builds the envelope, `exported_at = time.Now().UTC()`.
  - `ParseGlobalMemoryImport(data []byte) ([]GlobalMemoryEntry, error)` —
    validates `kind` and `schema_version`, returns entries.
  - `(s *GlobalMemoryStore) Import(entries []GlobalMemoryEntry) (added, skipped int)`
    — category coercion + empty-fact skip + `Add` (dedup/stamp/FIFO).
- `app/internal/agent/agent.go`
  - `GlobalMemoryExportJSON(appVersion string) ([]byte, error)` →
    `MarshalGlobalMemoryExport(a.globalMemory.All(), …)`.
  - `GlobalMemoryImportJSON(data []byte) (added, skipped int, err error)`
    → parse → `a.globalMemory.Import(...)` → `a.globalMemory.Save()`.
- `app/bindings.go`
  - `ExportGlobalMemory() (string, error)` — `SaveFileDialog`
    (`*.json`, default name), write `0600` (matches the store's own perms;
    the file may contain personal facts).
  - `ImportGlobalMemory() (GlobalMemoryImportResult, error)` —
    `OpenFileDialog` (`*.json`), `os.ReadFile`, agent import; returns
    `{Added, Skipped int}`. Cancelled dialog → zero result, nil error.
- `app/frontend/src/bindings.ts`, `types.ts` — add the two method
  declarations + `GlobalMemoryImportResult` type.
- `app/frontend/src/sidebar/Sidebar.tsx` — Export/Import buttons in the
  Global Memory section header (always-visible `.memory-io-actions`, not
  the hover-only `.session-actions`). `App.tsx` wires the handlers and
  refreshes the list after import; result/error dialogs are shown natively
  by the Go binding (see §2.4).

### Tests (mandatory)

- `global_memory_test.go`:
  - Marshal → Parse round-trip preserves all fields incl. timestamps and
    provenance.
  - `Import` dedup: importing entries that overlap an existing store adds
    only the new ones; `added`/`skipped` counts correct.
  - `Import` preserves non-zero timestamps; stamps zero ones.
  - `Import` coerces invalid category to `decision`; skips empty `Fact`.
  - `ParseGlobalMemoryImport` rejects wrong `kind`, wrong
    `schema_version`, and non-JSON, with distinct errors.
  - FIFO eviction still bounded by `MaxEntries` after a large import.
- `agent` test: `GlobalMemoryImportJSON` persists (re-`Load` sees the
  merged set) and returns correct counts.

## 5. Out of scope

- Overwrite / replace-all import modes (only merge-skip in v1).
- Encryption of the export file.
- Selective export (subset of facts).
- Session-bundle integration (Global Memory stays a separate file).
- Slash-command equivalents (`/export-memory`); buttons only in v1.

## 6. Manual smoke checklist

1. Export with a populated Global Memory → file written at chosen path;
   open it, confirm envelope shape and that every fact is present.
2. Import the just-exported file back → "0 added, N skipped" (all
   duplicates); store unchanged.
3. Delete a few facts, re-import the file → exactly the deleted facts
   return; others skipped.
4. Import a file with a fact whose category is garbage → entry appears as
   `decision`.
5. Import a session `.shellagent` bundle or arbitrary JSON → rejected with
   a legible "not a global-memory export" / schema error; store untouched.
6. Cross-machine (if available): export on A, import on B → facts appear;
   trust tags ([user-stated]/[derived]) preserved.
7. Cancel the save / open dialog → no-op, no error toast.
