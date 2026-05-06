# Session Import / Export — Design Note

**Status:** Design confirmed (open questions resolved 2026-05-07); ready to implement.
**Targets:** v0.4.0 (post-v0.3.0)

This document specifies a **complete session export/import** feature
that lets the user package an entire session — conversation,
session memory, findings, summaries, sandbox files, and the
session-scoped DuckDB database — into a single portable bundle
that can be re-imported on the same or a different machine while
preserving the privacy flag.

---

## 1. Goals

- **Portability**: a session can be moved between machines (and
  OS architectures, given DuckDB's cross-platform binary format)
  without manual file plumbing.
- **Completeness**: every per-session artifact — chat history,
  per-session memory, findings, summary cache, sandbox `work/`
  directory, and the analysis DuckDB — is included so the
  imported session is functionally identical to the source.
- **Privacy preservation**: the `Private` flag is carried in the
  bundle so an imported private session does not silently start
  promoting facts to Global Memory after the move.
- **Single-step UX**: one click in the sidebar exports; one click
  imports. Slash-command equivalents (`/export`, `/import`) are
  available for keyboard-first users.

---

## 2. Use cases

- **Backup**: save a snapshot of an in-progress investigation
  before a high-risk operation (DB drop, sandbox `rm -rf`).
- **Cross-machine migration**: move an investigation from a
  laptop to a workstation (or vice versa).
- **Reproducible handoff**: hand off a debugging session to a
  collaborator who can re-import and resume from the exact same
  state (chat + analysis tables + sandbox files).
- **Privacy-aware archive**: archive a private session as a
  `.shellagent` bundle without re-exposing it to Global Memory
  on the next session load.

---

## 3. Bundle format

### 3.1 File extension

`.shellagent`

A custom extension chosen for two reasons:
- distinguishes the bundle from arbitrary `.zip` files in
  Finder / file dialogs, reducing accidental double-clicks
  that would unzip the bundle into the user's Downloads folder
  (the bundle is not meant to be unzipped manually);
- enables OS-level file association in a future iteration
  (out-of-scope for v0.4.0).

The bundle is internally a standard ZIP archive — any zip tool
can open it for inspection — but the application uses the
extension as a soft convention.

### 3.2 Internal structure

```
manifest.json           # required — validated first on import
chat.json               # required
session_memory.json     # required (may be empty array)
findings.json           # required (may be empty array)
summaries.json          # optional — present if any summary cached
analysis.duckdb         # optional — present if DuckDB ever opened
work/                   # optional — present if sandbox tools ran
    <user files, recursive>
objects/                # optional — present if session owns objstore objects
    index.json          # array of ObjectMeta entries (sessionID stripped)
    data/<id>           # raw blob, one file per object (32-hex ID; 12-hex legacy tolerated)
```

All paths inside the zip use forward slashes. Symbolic links
are not followed nor written into the bundle (security). The
extractor refuses entries whose normalized path escapes the
target directory (zip-slip mitigation).

### 3.3 manifest.json schema

```json
{
    "schema_version": 1,
    "exported_at": "2026-05-07T12:34:56Z",
    "exported_by_app_version": "0.4.0",
    "session": {
        "original_id": "abc123",
        "title": "Investigate slow queries",
        "private": false
    }
}
```

Field semantics:

| Field | Purpose |
|-------|---------|
| `schema_version` | Bundle schema version. v0.4.0 writes `1`. Import refuses any other value. |
| `exported_at` | RFC3339 UTC timestamp. Informational only. |
| `exported_by_app_version` | shell-agent-v2 version that produced the bundle. Informational; not used for compatibility checks (the schema version is). |
| `session.original_id` | The original session ID at the time of export. Preserved for traceability; not used as the imported session ID. |
| `session.title` | Title at export time. Used as the basis for the imported session's title (with collision suffix if needed). |
| `session.private` | Privacy flag. Carried verbatim into the imported session. |

Validation rules on import:

1. Bundle parses as a ZIP archive.
2. `manifest.json` exists at the archive root and parses as JSON.
3. `schema_version == 1` exactly. Any other value → reject with
   "unsupported bundle schema version: N".
4. `chat.json`, `session_memory.json`, `findings.json` exist and
   parse as JSON.
5. Optional files (`summaries.json`, `analysis.duckdb`, `work/*`,
   `objects/`) are accepted if absent.
6. Any zip entry whose normalized path escapes the target dir
   → reject with "bundle contains unsafe path".
7. If `objects/` is present, every entry referenced from
   `objects/index.json` must have a corresponding
   `objects/data/<id>` blob, and conversely every
   `objects/data/<id>` file must have an index entry. Any
   mismatch → reject with "bundle objects index/blobs out of
   sync".

---

## 4. Export operation

### 4.1 Common flow (any session)

1. Acquire **per-session export lock** (sync.Mutex keyed by
   session ID inside `internal/sessionio`).
2. Determine which artifacts exist in `sessions/<id>/`.
3. Enumerate objstore objects owned by this session via
   `objstore.Store.ListBySession(id)`. The returned slice is a
   point-in-time snapshot; the live index is RLocked during the
   call so a concurrent `Store()` cannot mutate it mid-iteration.
4. Build the manifest JSON.
5. Stream-write the ZIP archive to the destination path. The
   archive contains the per-session artifacts plus, if step 3
   returned anything, `objects/index.json` (sanitised metadata
   with `sessionID` stripped, since the bundle is itself
   session-scoped) and one `objects/data/<id>` blob per object,
   read via `objstore.Load(id)`.
6. Release the lock.
7. Audit log:
   `session exported: id=<id> private=<bool> bytes=<N> objects=<K> dest=<path>`.

### 4.2 Active session — additional steps

The "active session" is the one currently loaded into the agent
(its DuckDB connection may be open; background tasks may be
holding references to its memory stores).

The state machine integration:

1. **Wait for `agent.State == Idle`.** Per CLAUDE.md the agent
   has an Idle/Busy state machine; export is a writer that
   conflicts with any in-flight tool execution. If the agent
   is Busy at request time, the binding returns
   `ErrAgentBusy` and the UI surfaces "Cannot export while a
   request is in flight — wait for the agent to become idle."
   (We do not silently block the UI thread.)
2. **Take the agent's session-write lock** (the same lock that
   `Save()` acquires when persisting `chat.json` after each
   turn). This serialises export against any background task
   that might race a write — title generation, memory
   compaction, pinned-memory extraction.
3. **Flush in-memory state to disk**: `session.Save()`,
   `sessionMemory.Save()`, `findings.Save()`, `summaryCache.Save()`.
4. **Close `analysis.Engine`** for this session (if open). The
   engine exposes a `Close()` that closes the underlying
   `*sql.DB`. Closing flushes the WAL and releases the file
   handle so the binary copy below sees a consistent file.
5. **Copy** all per-session artifacts (chat.json,
   session_memory.json, findings.json, summaries.json,
   analysis.duckdb, work/) plus the bundle's `objects/`
   directory (per §4.1 step 5) into the ZIP stream.
6. **Mark Engine as needing reopen** — the engine is lazily
   initialised on next use (existing pattern), so no eager
   reopen is needed. The next analysis-tool call re-opens the
   file naturally.
7. **Release** the session-write lock and the per-session
   export lock.

### 4.3 Race conditions catalogued

| # | Scenario | Mitigation |
|---|----------|------------|
| R1 | User sends a message during export of active session | Agent is held in Idle for the duration of the export (state-write lock); send is queued at the input layer until the lock releases. |
| R2 | Background extraction (memory / pinned / title) runs while export is mid-copy | Background tasks acquire the same session-write lock before persisting; export holds it, so background writers block. |
| R3 | Sandbox container is mid-execution writing to `work/` | Sandbox execution is part of agent.Busy, so R1's gate already covers this. Export only proceeds when no sandbox is executing. |
| R4 | User exports session A while session B is active | Per-session locks are independent. Session A is dormant — no Engine open, no background tasks holding refs. Plain copy is safe. |
| R5 | Two simultaneous exports of the same session | Per-session export lock serialises them. The second waits. |
| R6 | DuckDB still has a lazy connection from earlier in this run | Step 4 of §4.2 closes it; the next user-triggered analysis call lazily reopens. |
| R7 | Disk full mid-write | ZIP write returns error → temp file is removed → original session untouched. Error surfaced to UI. |
| R8 | App crashes mid-export | Destination file is written via temp + rename (atomicio pattern). A partial temp file may remain; the user can delete it manually. The original session is unaffected. |
| R9 | Tool produces a new object via `objstore.Store()` while export is mid-copy | For active session: agent.Idle gate prevents it (no tool can run). For non-active session: no agent activity targets this session. `ListBySession`'s snapshot semantics make a stale snapshot impossible mid-iteration. |
| R10 | Concurrent `objstore.Delete()` (e.g. another session being deleted via `DeleteBySession`) | objstore is single-mutex protected; the snapshot from §4.1 step 3 is consistent. Deletion of *this* session's objects can only originate from the user explicitly deleting the session, which is gated by the per-session export lock. |

### 4.4 Non-active session export

No state-machine concern. The session is dormant: no Engine
open, no background tasks holding references, no agent state
machine to coordinate with. The export is a plain file copy
(under the per-session export lock from §4.1, to defend
against the unlikely R5).

### 4.5 Export filename

Default name proposed by the save dialog:
`<safe-title>-<YYYYMMDD-HHMMSS>.shellagent`

`<safe-title>` is the session title with characters
`/\:*?"<>|` and ASCII control codes replaced with `_`,
truncated to 64 characters. If the title is empty, falls
back to `session-<short-id>`.

The user can override the proposed name in the save dialog.

---

## 5. Import operation

1. User chooses a `.shellagent` file via native open dialog
   (filter: `*.shellagent`).
2. **Validate** per §3.3 (zip + manifest + schema_version +
   required files + zip-slip check). On any failure, surface
   the specific error to the UI; abort.
3. **Generate a new session ID** (UUID). The original ID is
   preserved only in the manifest, never reused — this avoids
   collision with an existing session whose ID happens to be
   the same.
4. **Resolve title collision**: if any session in the existing
   `ListSessions()` has the same Title as the bundle, append
   ` (imported)`. If a session with `<title> (imported)` also
   exists, append ` (imported 2)`, then `3`, and so on.
5. **Create** `sessions/<newID>/` directory.
6. **Extract** bundle entries (everything except `objects/`)
   into the new directory. The `work/` subtree is extracted
   as-is.
7. **Register objects** (if `objects/` is present in the bundle):
   for each entry in `objects/index.json`, generate a fresh
   object ID, register the blob via `objstore.Store(blob, type,
   mime, origName, newSessionID, newObjectID)`, and accumulate
   an `oldID → newID` map. Object IDs and references are always
   regenerated (no preserve-or-collide path); see §5.3 below.
8. **Rewrite chat.json**: set `id` field to `<newID>`; remap
   each `Record.ObjectIDs[]` via the map; sweep each
   `Record.Content` with the regex
   `\b(?:object:)?([a-f0-9]{12}|[a-f0-9]{32})\b` and replace
   any matching old ID with the corresponding new ID. (The
   12-hex branch covers legacy bundles produced before the
   ID width change.) Other fields — title, private — are
   preserved.
9. **Rewrite summaries.json** (if present): apply the same
   regex sweep to each `SummaryEntry.Summary` text. The
   summarizer LLM may have paraphrased markdown image refs
   from the source records into the summary, so this sweep
   is required to avoid dangling references on the imported
   side. (`session_memory.json` and `findings.json` are not
   swept — see §5.3 for why their write paths cannot embed
   refs.)
10. **Re-add** to the session registry by next `ListSessions()`
    call (the registry reads from disk; no in-memory cache to
    invalidate).
11. **Audit log**:
    `session imported: original_id=<orig> new_id=<new> private=<bool> bytes=<N> objects=<K>`.
12. **Auto-switch**: the binding returns the new session ID
    and the frontend immediately calls `LoadSession(newID)`.

### 5.1 Edge cases

| Case | Behaviour |
|------|-----------|
| Bundle is not a valid zip | Reject: "not a valid .shellagent bundle" |
| `manifest.json` missing or unparseable | Reject: "missing or corrupt manifest.json" |
| `schema_version` != 1 | Reject: "unsupported bundle schema version: N" |
| Required file missing | Reject: "bundle missing required file: X" |
| Zip-slip path | Reject: "bundle contains unsafe path: X" |
| New session dir already exists (UUID collision — vanishingly unlikely) | Retry once with a new UUID; then fail. |
| DuckDB file from different OS / arch | Accept. DuckDB v0.x+ binary format is portable across darwin/linux/x86/arm. Validation deferred to first analysis call. |
| Disk full during extract | Clean up the partial new session dir; surface error. |
| `objstore.Store()` fails partway through object registration | Roll back: `Delete()` any objects already registered for the new sessionID; remove the partial session dir; surface error. |
| Bundle declares an object in `objects/index.json` but blob file missing (or vice versa) | Reject during validation (§3.3 rule 7). |
| User cancels open dialog | No-op. |

### 5.2 Privacy posture on import

The `Private` flag from the manifest is preserved verbatim in
the imported session's `chat.json`. This means:

- A private session imported from another machine remains
  private — facts will not be promoted to Global Memory on
  this machine either.
- A non-private session imported from another machine remains
  non-private — facts produced in subsequent turns on this
  machine **will** be promoted to Global Memory under the
  normal rules. Users who want to firewall an import should
  toggle privacy in the UI before resuming the conversation
  (this affects future turns; past chat content is not
  retroactively scrubbed from Global Memory because the
  past content is in the bundle, not in this machine's
  Global Memory).

### 5.3 Object ID strategy

Imported objects always receive new IDs. Two reasons:

- **Collision is a real, deterministic case** — not just a
  cosmic-ray probability. If the user exports a session and
  then re-imports the same bundle on the same machine without
  first deleting the original session, every object ID in the
  bundle collides with an existing entry in the *global*
  objstore (which is shared across sessions). Generating new
  IDs makes that path trivially safe and lets the same bundle
  be imported multiple times.
- **objstore tags each object with exactly one `sessionID`** —
  used by `DeleteBySession` for cleanup. If we kept the
  original ID and updated the index entry to point at the new
  sessionID, we would silently steal ownership from the
  original session; deleting the imported session would then
  orphan the original. Fresh IDs sidestep the conflict.

The cost is a one-shot rewrite of references at import time.
Per the persistence audit, this is bounded to two files and
three text loci:

| File | Field | Why |
|------|-------|-----|
| `chat.json` | `Record.ObjectIDs[]` | Structured array, direct remap. |
| `chat.json` | `Record.Content` | User/assistant/tool message text may carry `![alt](object:ID)` markdown. |
| `summaries.json` | `SummaryEntry.Summary` | Summarizer LLM can paraphrase `object:ID` refs from source records into summary text. |

`session_memory.json`, `findings.json`, and
`global_memory.json` are explicitly **not** swept:

- `session_memory.json`: facts go through
  `sanitizeMemoryText()` which strips control chars and
  collapses whitespace; the extraction prompt forbids
  carrying markdown image refs into facts.
- `findings.json`: the system prompt instructs the LLM to use
  markdown image syntax only in chat content, not in finding
  bodies; the promote-finding tool stores the finding text
  verbatim but the LLM does not embed `object:` refs there.
- `global_memory.json`: out of scope for export; promotion
  flows pass through fact-sanitisation that drops markdown.

If a future change introduces an object-ref embedding into one
of these stores, this section and the rewriter must be
updated together.

---

## 6. UI

### 6.1 Sidebar — session list hover row

Each session row in the sidebar already exposes hover-only
action icons (rename, delete). Add a third:

- **Export** icon (📤 or ⬇) — clicking exports that session
  via the same flow as `/export` (save dialog with proposed
  default filename).

The icon is hidden when the session is in Busy state (the
export would refuse anyway with `ErrAgentBusy`); a tooltip
explains why.

### 6.2 Sidebar — bottom-nav

Below the existing `+ New Private Chat` button, add:

- **Import Chat** button with 📥 icon — opens a native open
  dialog filtered to `*.shellagent`. On successful import,
  auto-loads the new session.

### 6.3 Slash commands

- `/export` — exports the **current** session. If no session
  is loaded, the command is a no-op with a status message.
  If the agent is Busy, surfaces `ErrAgentBusy`.
- `/import` — opens the open dialog (same as the bottom-nav
  button).

### 6.4 File dialogs

Both dialogs use the Wails runtime save/open dialog APIs:

- Export: `SaveFileDialog` with `Filters: [{DisplayName: "shell-agent-v2 session", Pattern: "*.shellagent"}]` and `DefaultFilename` from §4.5.
- Import: `OpenFileDialog` with the same filter.

---

## 7. Audit log

Two new INFO-level log lines (privacy-safe — neither contains
fact content or message text):

- `session exported: id=<id> private=<bool> bytes=<N> objects=<K> dest=<path>`
- `session imported: original_id=<orig> new_id=<new> private=<bool> bytes=<N> objects=<K>`

`<path>` is the user-chosen destination (already a path the user
typed / chose, so logging it does not introduce new disclosure).
Both lines obey the v0.3.0 log-level filter.

---

## 8. Implementation phases

Phase A → B → C in order. A is fully testable without UI.

### Phase A — Bundle format + I/O

- New package `internal/sessionio/` with:
  - `Manifest` struct + `MarshalManifest` / `UnmarshalManifest`
  - `ExportSession(srcDir, destPath, sessionMeta, objects []ObjectExport) error`
  - `ImportSession(srcPath, destBaseDir, objstore ObjstoreWriter) (newID string, manifest Manifest, err error)`
  - Reference rewriter: regex sweep + structured remap helpers
    over `Record.ObjectIDs[]`, `Record.Content`, and
    `SummaryEntry.Summary`
  - Zip-slip safety
  - Atomic dest write (temp + rename)
  - `ObjstoreWriter` is a small interface (`Store(blob, type,
    mime, origName, sessionID, objectID)`) so the package
    stays decoupled from the live objstore for unit testing
- Tests:
  - Roundtrip: build a fixture session dir + fixture objects →
    export → import → diff source vs imported, verify all
    object refs in chat.json and summaries.json point at new
    (registered) IDs and resolve via the fake objstore.
  - Reject malformed bundles (each validation rule, including
    objects index/blob mismatches).
  - Reject zip-slip.
  - Title collision suffixing.
  - Reference rewriter unit tests: legacy 12-hex IDs, 32-hex
    IDs, `object:` prefix variants, IDs at word boundaries,
    IDs that should NOT be matched (e.g. random hex inside a
    longer string).

### Phase B — State-machine integration

- Per-session export lock in `internal/sessionio` keyed by ID.
- Hook into `agent` package: `ExportActiveSession` method
  acquires session-write lock, flushes, closes Engine, calls
  `objstore.ListBySession()` and `Load()` for each owned
  object, copies into the bundle, releases.
- `ImportSession` agent method: extracts via `sessionio` (which
  registers objects through the live objstore and rewrites
  references), then `LoadSession(newID)` to auto-switch.
- Tests:
  - Concurrent export + tool call → agent surfaces `ErrAgentBusy`.
  - Export of inactive session while different session is active
    → succeeds without disturbing the active session.
  - DuckDB Engine close+lazy-reopen verified by post-export
    analysis call.
  - Multi-import of the same bundle → both sessions get
    distinct new IDs, distinct new object IDs, no objstore
    collision.

### Phase C — Bindings + frontend

- `bindings.ExportSession(id, dest) error`.
- `bindings.ImportSession(src) (string, error)` returns new ID.
- Wails save/open dialogs.
- Sidebar export icon (hover row).
- Sidebar `Import Chat` button (bottom-nav).
- `/export` and `/import` slash commands.
- Frontend auto-load after import.
- Manual smoke per §10.

---

## 9. Out of scope (this release)

- **Encryption** of the bundle (would require a keyring; defer).
- **Bulk export / import** (multiple sessions in one bundle).
- **Cross-version schema migration** (v1 only; future versions
  will refuse v1 bundles unless a migrator is added).
- **Cloud sync** (no remote backend integration).
- **Selective export** (e.g. "export everything except DuckDB").
- **Diff / merge** between exports.
- **Bundle preview** before import (just imports immediately).

---

## 10. Manual smoke checklist

**Phase A/B (CLI-only):**
1. Roundtrip an inactive session → import succeeds, content
   matches.
2. Roundtrip an active session → DuckDB is closed and reopened
   on next analysis call; no data corruption.
3. Mangle the manifest version → import fails with the expected
   error.
4. Construct a zip-slip bundle → import refuses.
5. Roundtrip a session that contains an attached image and a
   `create-report`-generated report → import succeeds; the
   imported session's `Record.ObjectIDs[]` and any
   `![alt](object:ID)` markdown in `Record.Content` /
   `SummaryEntry.Summary` point at the *new* object IDs and
   the blobs resolve.
6. Bundle whose `objects/index.json` references a missing blob
   → import refuses with the expected error.
7. Multi-import: import the same bundle twice → both succeed
   with different new sessionIDs and different new objectIDs;
   no `objstore.Store` collision.

**Phase C (UI):**
1. Sidebar Export icon on an inactive session → save dialog
   appears with proposed filename → file lands at chosen path.
2. Sidebar Export icon while agent is Busy → tooltip "agent busy"
   shown; click is no-op or surfaces error.
3. `/export` command → same as Export icon for current session.
4. `Import Chat` button → open dialog filtered to `*.shellagent`
   → after import, auto-switches to imported session, sidebar
   list updated.
5. `/import` command → same as Import button.
6. Title collision: import a bundle whose title matches existing
   → imported title gets ` (imported)` suffix.
7. Privacy round-trip: export a private session → delete original
   → import → 🔒 indicator appears, Pin buttons hidden, audit log
   shows `private=true`.
8. Privacy round-trip non-private: export → import → no 🔒, normal
   memory routing resumes.
9. Cross-machine sanity (if available): export on one machine,
   import on another → opens cleanly, DuckDB queries work,
   any embedded image renders.
10. `app.log` shows the two new INFO lines per export and
    import, with `objects=K` matching the actual object count.
11. Image roundtrip via UI: send a message with an attached
    image → export → delete original session → import → image
    renders in the imported session, opens to identical bytes.

---

## Decisions resolved 2026-05-07

| # | Question | Decision |
|---|----------|----------|
| 1 | Bundle file extension | `.shellagent` (custom; soft convention) |
| 2 | DuckDB during active-session export | Close → copy → reopen (lazy). State machine: wait for Idle, hold session-write lock, defer reopen to next use. |
| 3 | Title collision on import | Auto-suffix ` (imported)`, ` (imported 2)`, … |
| 4 | Export filename | Auto-name `<title>-<YYYYMMDD-HHMMSS>.shellagent` + save dialog (user can override) |
| 5 | `work/` filtering | Include all (no size cap, no `.DS_Store` filter) |
| 6 | Post-import behaviour | Auto-switch to imported session |
| 7 | Schema version mismatch | Reject (no migration logic in v1) |
| 8 | Object ID strategy on import | Always regenerate new IDs; rewrite `Record.ObjectIDs[]` (structured), `Record.Content` (regex sweep), `SummaryEntry.Summary` (regex sweep). Bundle includes `objects/index.json` + `objects/data/<id>`. `session_memory.json` / `findings.json` skipped — their write paths cannot embed refs. |
