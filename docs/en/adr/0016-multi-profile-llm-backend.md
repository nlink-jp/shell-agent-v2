# Multi-profile LLM backend — Design Note

**Status:** Implemented in v0.12.0 (2026-05-19).
**Target:** v0.12.0 (minor bump — config schema migration, new
session-level persisted state, new `/profile` command, new Settings
tab; no breaking Wails API removals).
**Reported by:** User — "I want to attribute Vertex AI charges to
different GCP projects per piece of work, and also keep separate
local-LLM endpoints (one home rig, one work laptop). Today
shell-agent-v2 holds a single pair of `(Local, VertexAI)` configs
in `config.json` and there is no way to switch between sets without
hand-editing the file."

This note specifies a **profile-as-pair** model: a *profile* is a
named bundle of `(LocalConfig, VertexAIConfig)` plus a
`default_backend` flag. The user can define multiple profiles,
each session references exactly one profile, and `/model`
continues to toggle Local↔Vertex *within* the session's current
profile. The "two backends always exist" invariant from v0.11.x is
preserved — there is never a session whose backend the user
selected disappears unilaterally.

---

## 1. Problem

Today (`app/internal/config/config.go:99-104`):

```go
type LLMConfig struct {
    DefaultBackend LLMBackend     `json:"default_backend"`
    Local          LocalConfig    `json:"local"`
    VertexAI       VertexAIConfig `json:"vertex_ai"`
}
```

There is exactly one `Local` and one `VertexAI` in the whole config.
That has two consequences the user hit in practice:

1. **No billing / project isolation for Vertex AI.** When the same
   user does paid client work that should bill GCP project A, and
   personal experiments that should bill personal project B, today
   they must hand-edit `config.json` between sessions. There is no
   per-session attribution and no way to make "use project A" a
   property of a session that survives app restart.

2. **No multi-endpoint local LLM.** A home desktop running LM
   Studio on `localhost:1234` and a laptop hitting a colleague's
   server on `10.0.0.5:8000` cannot coexist in one config. The
   user is forced into Settings every context switch.

The user's first instinct ("just allow multiple Local / multiple
VertexAI configs independently") collides with the v0.11.x
behaviour that `/model` toggles between *the* Local and *the*
VertexAI: with independent multiples the pair becomes ambiguous,
and worse, a session could find its persisted backend deleted out
from under it.

### Why simpler fixes were rejected

- **Add a second pair of `Local2` / `VertexAI2` fields.** Doesn't
  scale beyond two; encodes "two" in the schema.
- **Replace single Local / VertexAI with `[]Local` / `[]VertexAI`
  independently.** Breaks the v0.11.x invariant that exactly one
  Local and one VertexAI always exist. `/model` becomes
  ambiguous ("which Local?"). A session referencing a specific
  Local-by-id could lose it on config edit.
- **Drop the backend abstraction; let each session pick any
  endpoint+project tuple ad-hoc.** Loses the Local/VertexAI
  dichotomy that the rest of the codebase (`llm.Backend`,
  `ContextBudgetFor(backend)`, retry policy per backend type)
  relies on. Too large a refactor for the user's actual need.

---

## 2. Goals

1. **Multiple `(Local, VertexAI)` profiles.** The user can define
   N profiles in Settings, each with its own Local config and its
   own VertexAI config.
2. **Per-session profile reference, persisted.** Each session
   remembers which profile it was created against. Closing the
   app and reopening preserves the binding.
3. **`/model` semantics unchanged.** `/model local` / `/model
   vertex` continues to toggle between *the* Local and *the*
   Vertex of the session's profile, in-process. No persistence
   change to `/model`.
4. **New `/profile <name>` command.** Switches the current
   session's profile reference; persisted to `session.json`.
   After switching, the next `/model` operates on the new
   profile's pair.
5. **Default profile invariant.** Exactly one profile is the
   default. It cannot be deleted from the UI. Newly-created
   sessions get the default profile. If a session's referenced
   profile is deleted, it falls back to the default.
6. **Clean v0.11.x → v0.12.0 migration.** Existing `config.json`
   auto-upgrades to a single "Default" profile holding the prior
   Local + VertexAI. Existing sessions without `session.json`
   resolve to the default profile on load.
7. **Separation of session config from chat records.** Profile
   binding lives in a new file `session.json` alongside `chat.json`,
   not embedded in `chat.json`. Keeps the conversation transcript
   file untouched by configuration changes.
8. **GUI-discoverable session controls.** The current session's
   profile and active backend are visible in the status bar at all
   times, and clicking the badges opens an inline **Session
   Control Popover** that lets the user switch profile and toggle
   Local↔Vertex without leaving the chat (no full Settings dialog
   round-trip). Power users keep `/profile` and `/model`; casual
   users get a one-click affordance.

Non-goals:

- **Per-session `active_backend` persistence.** v0.11.x `/model`
  is ephemeral / process-global within the active backend
  variable; this ADR preserves that. The session remembers its
  *profile*, not which side of the profile is currently active.
  Adding active-side persistence can be a future ADR if anyone
  asks.
- **Profile-level overrides for memory / sandbox / tools.** Out
  of scope. A profile is only a `(Local, VertexAI, default_backend)`
  triple; the rest of `Config` stays global.
- **Importing / exporting profiles across machines.** Out of
  scope. Profiles are local-only configuration. Bundle
  export/import (`ExportSession`) does not carry the source
  profile reference, and import binds to the destination
  machine's current default profile.
- **Per-profile model lists, model-fetching UI, etc.** Out of
  scope. A profile carries the same single `Model` field per
  side that exists today.

---

## 3. Design

### 3.1 Data model

New `LLMProfile` type replaces the bare `Local` / `VertexAI`
fields in `LLMConfig`:

```go
// LLMProfile is one (Local, VertexAI) pair plus the side that
// /model lands on when a session is first loaded against this
// profile.
type LLMProfile struct {
    ID             string         `json:"id"`              // UUID v4 (immutable)
    Name           string         `json:"name"`            // display name (mutable, no uniqueness req'd but UI nudges unique)
    DefaultBackend LLMBackend     `json:"default_backend"` // "local" | "vertex_ai"
    Local          LocalConfig    `json:"local"`
    VertexAI       VertexAIConfig `json:"vertex_ai"`
}

// LLMConfig becomes a profile list + default pointer.
type LLMConfig struct {
    DefaultProfileID string       `json:"default_profile_id"` // must reference an entry in Profiles
    Profiles         []LLMProfile `json:"profiles"`
}
```

Invariants enforced by the loader:
- `len(Profiles) >= 1` after load (synthesised on migration if 0).
- `DefaultProfileID` references an existing profile (auto-repaired
  to `Profiles[0].ID` on load if dangling).
- Within each profile, both `Local` and `VertexAI` are always
  present as structs (even if empty / unconfigured). This is the
  v0.11.x invariant carried forward.

### 3.2 session.json

New file per session, sibling to `chat.json`:

```
~/Library/Application Support/shell-agent-v2/sessions/<id>/
├── chat.json              # unchanged (records, title, private)
├── session.json           # NEW (this ADR)
├── session_memory.json
├── findings.json
├── summaries.json
└── work/
```

`session.json` schema (v1):

```json
{
  "schema_version": 1,
  "profile_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
}
```

That's it. Two fields. No `title`, no `private` (those stay in
`chat.json` for backwards-compatibility — moving them would force
every existing session to rewrite, which this ADR explicitly
avoids). No `active_backend` (see §2 non-goals). No
`profile_name_hint` (debug-only field rejected as adding
maintenance burden without load-bearing value).

`schema_version` is the forward-compatibility hook. v1 is the
only version this ADR defines; future fields will bump it.

I/O is atomic (tmp + rename + fsync), same pattern as `chat.json`
(`memory.go:Save`).

### 3.3 Profile resolution on session load

```
LoadSession(sessionID):
  1. Read chat.json → Session struct (unchanged)
  2. Read session.json:
     2a. Not exists (v0.11.x session, OR new session pre-first-save)
         → resolve profile_id = config.LLM.DefaultProfileID
         → write session.json with that profile_id (lazy migrate)
     2b. Exists, parse OK → resolve profile_id from file
     2c. Exists but malformed → log warn, fall back to default, rewrite
  3. Validate profile_id is in config.LLM.Profiles:
     3a. Yes → use profile
     3b. No (profile was deleted) → log info, set profile_id =
         DefaultProfileID, rewrite session.json (fallback persisted)
  4. Instantiate agent.backend per profile.DefaultBackend
     (LocalConfig / VertexAIConfig sourced from the resolved profile)
```

The lazy migrate in (2a) means every v0.11.x session writes its
`session.json` the first time it's opened in v0.12.0. No batch
migration step; no flag day. Idempotent on subsequent loads.

### 3.4 /model and /profile

`/model local` and `/model vertex` keep their v0.11.x behaviour:
process-global toggle of `agent.backend`. The change is that the
new `Local` / `VertexAI` configs they instantiate come from the
current session's *profile's* pair, not the (now removed) top-
level pair.

New `/profile <name>`:

```
/profile                  → list profiles, mark current with •
/profile <name>           → switch current session's profile to <name>
/profile <name> default   → also set <name> as the global default
```

Switching profile (`/profile <name>`):
1. Resolve `<name>` to a profile UUID via case-insensitive name match.
   Defensive: if ambiguous (two profiles with same name —
   reachable only via hand-edited `config.json` since the Settings
   Save path auto-disambiguates, see §3.5), error and ask for a
   partial UUID prefix.
2. Update session.json with the new `profile_id`. Atomic write.
3. Re-instantiate `agent.backend` to the new profile's
   `default_backend` side (resets `/model` toggle to the new
   profile's default — i.e. switching profile mid-session is a
   stronger statement than `/model`, and the user is opting in
   to the new profile's preferred side).
4. Emit `agent:profile:changed` Wails event so the UI can update
   the status bar badge.

Profile switch is NOT allowed during `state == StateBusy` or
during `extractionInFlight` (same gate as session switch — ADR-
0015 §4.5). Reused mechanism: `/profile` is a chat-input command
parsed by the same path as `/model`, so the existing busy-state
guard naturally applies.

### 3.5 Profile CRUD via Settings

Settings dialog gains a new "LLM Profiles" tab that replaces the
existing "Local" + "Vertex AI" tabs:

```
┌─ LLM Profiles ────────────────────────────────────────────┐
│                                                           │
│  Profiles:                                                │
│    ● Default               (default)    [Edit] [Delete*]  │
│    ○ Production GCP                     [Edit] [Delete]   │
│    ○ Personal Lab                       [Edit] [Delete]   │
│                                                           │
│    [+ New profile from template ▼]                        │
│         ├─ Default (LM Studio + gemini-2.5-flash)         │
│         ├─ Clone of: Production GCP                       │
│         └─ Empty                                          │
│                                                           │
│  Selected: Production GCP                                 │
│  ──────────────────────────────────────────────────────── │
│  Name:            [Production GCP        ]                │
│  Default side:    ( ) Local  (•) Vertex AI                │
│  Set as default profile:  [ ]                             │
│                                                           │
│  ┌─ Local ─────────────────────────────────────────────┐  │
│  │ Endpoint, model, API key env, context budget, …    │  │
│  └────────────────────────────────────────────────────┘  │
│  ┌─ Vertex AI ────────────────────────────────────────┐  │
│  │ Project ID, region, model, context budget, …       │  │
│  └────────────────────────────────────────────────────┘  │
│                                                           │
│  [Cancel]                              [Save changes]     │
└───────────────────────────────────────────────────────────┘
```

\* The default profile's Delete button is disabled (tooltip:
"This is the default profile. Pick a different default before
deleting."). Setting another profile as default re-enables the
old one's Delete.

Edit flow: profile fields edit in-place; Save validates and
rewrites `config.json` (atomic, existing `Save()`). Other sessions
in memory are unaffected — they only see the change on next
session load (since profile lookup happens at LoadSession).

**Name auto-disambiguation.** If the user attempts to Save (create
or rename) a profile whose Name (case-insensitive) collides with
another profile's Name, the save proceeds but the Name is
automatically rewritten by appending ` (N)` with the smallest
integer N ≥ 2 that yields a free name — same convention as the
macOS Finder for duplicate-name file copies. A non-blocking yellow
inline toast surfaces the rewrite: `Renamed to "Production GCP
(2)" because "Production GCP" was already taken.`

Algorithm:
```go
// DisambiguateName returns desired if no other profile (excluding
// the one identified by selfID, if any) has that Name (case-
// insensitive). Otherwise it returns "<desired> (N)" with the
// smallest integer N ≥ 2 that resolves the collision. Matches
// the macOS Finder duplicate-name suffix convention.
func DisambiguateName(profiles []LLMProfile, desired, selfID string) string
```

Renames preserve the profile's UUID, so all sessions referencing
the renamed profile keep their binding intact regardless of the
suffix.

This makes the "two profiles with identical Name" case unreachable
through the normal UI path; the §3.4 ambiguity branch in
`/profile <name>` becomes defensive code that only fires when a
user hand-edits `config.json` to introduce duplicates.

Delete flow: confirmation dialog warns "X sessions reference this
profile; they will fall back to default on next load." The
fallback is lazy (§3.3 step 3b), not eager — we don't open every
session file to rewrite. The next time each session is loaded, it
rewrites its own `session.json`.

### 3.6 Migration from v0.11.x

On `config.Load()`, if the parsed `LLMConfig` has the old shape
(presence of top-level `default_backend` / `local` / `vertex_ai`
fields, OR absence of `profiles` array), synthesise a single
profile:

```go
LLMProfile{
    ID:             uuid.New().String(),
    Name:           "Default",
    DefaultBackend: old.DefaultBackend,
    Local:          old.Local,
    VertexAI:       old.VertexAI,
}
```

Set `LLMConfig.DefaultProfileID` = that UUID. Persist immediately
(rewrite `config.json`). The old top-level fields are *removed*
from the on-disk JSON after migration so the dual-shape ambiguity
doesn't linger. Old fields kept in the in-memory struct as
`json:"-"` is unnecessary — Go's default unmarshal silently
discards unknown fields, so a one-shot migration is enough.

Detection: parse into a transitional struct that has both shapes
as optional fields:

```go
type migrationLLMConfig struct {
    // New shape
    DefaultProfileID string       `json:"default_profile_id,omitempty"`
    Profiles         []LLMProfile `json:"profiles,omitempty"`
    // Old shape
    DefaultBackend LLMBackend     `json:"default_backend,omitempty"`
    Local          *LocalConfig   `json:"local,omitempty"`
    VertexAI       *VertexAIConfig `json:"vertex_ai,omitempty"`
}
```

If `len(Profiles) == 0 && (Local != nil || VertexAI != nil ||
DefaultBackend != "")`, do the synthesis. Otherwise use the new
shape as-is.

Sessions need no migration step. v0.11.x sessions without
`session.json` get one written on first load (§3.3 step 2a)
pointing at the freshly-synthesised "Default" profile.

### 3.7 Export / Import

Export (`bindings.ExportSession`) currently bundles
`chat.json` + `session_memory.json` + `findings.json` +
`summaries.json` + `work/`. Add `session.json` to the bundle? No:

- Profile UUIDs are not stable across machines.
- Exporting `profile_id` would either fail validation on import
  (UUID not present) or risk silently binding the imported
  session to a same-named-but-different profile, which is worse
  than the no-binding case.
- Profile-level config (endpoints, project IDs, API key env vars)
  is exactly the kind of machine-local data that shouldn't
  travel.

So: export omits `session.json` from the bundle. Import generates
a fresh `session.json` pointing at the destination machine's
current `DefaultProfileID`. The imported session inherits the
local default — same behaviour as importing a v0.11.x bundle into
v0.12.0.

Documentation note added to ExportSession: "session bundles are
profile-agnostic; the importing machine binds the session to its
current default profile."

### 3.8 Backend instantiation refactor

`agent.setBackend(LLMBackend)` (`agent.go:2388`) currently
reaches directly into `Config.LLM.Local` / `Config.LLM.VertexAI`.
It must now take the profile-bound configs:

```go
// Before
func (a *Agent) setBackend(b LLMBackend) error {
    switch b {
    case BackendLocal:
        return a.useLocalBackend(a.config.LLM.Local)
    case BackendVertexAI:
        return a.useVertexAIBackend(a.config.LLM.VertexAI)
    }
}

// After
func (a *Agent) setBackend(b LLMBackend) error {
    profile, err := a.config.ResolveProfile(a.session.ProfileID)
    if err != nil { return err }
    switch b {
    case BackendLocal:
        return a.useLocalBackend(profile.Local)
    case BackendVertexAI:
        return a.useVertexAIBackend(profile.VertexAI)
    }
}
```

`Config.ResolveProfile(id)` returns the matching profile or the
default if id is empty/unknown.

The `Agent` struct gains no new fields — profile_id lives on the
session-level state. `Agent.session` becomes the source of
truth; `agent.session.ProfileID` is set during LoadSession from
`session.json` (§3.3).

### 3.9 Wails events

New events:

| Event | Payload | When |
|-------|---------|------|
| `agent:profile:changed` | `{profile_id, profile_name, default_backend}` | After `/profile <name>`, `SwitchSessionProfile`, or session load resolves to a different profile than the one currently in `agent.session.ProfileID` |
| `agent:backend:changed` | `{backend}` | After `/model` or `SwitchSessionBackend` toggles the active backend within the current profile |
| `config:profile:list_changed` | `{profile_ids: []string}` | After Settings save adds/removes/renames profiles |

Existing events unchanged. The `agent:profile:changed` event also
fires after a profile deletion triggers the active session to
fall back to default. The `agent:backend:changed` event drives the
status bar backend badge and the Session Control Popover's radio
state; today's `agent.backend` toggle has no dedicated event
(frontend infers state from `Bindings.GetState()` polls), so this
ADR adds the explicit event for both the popover and any future
listeners.

### 3.10 Status bar + Session Control Popover

The status bar shows the current session's profile and backend
side-by-side; both are interactive:

```
[Profile: Production GCP ▾] [Vertex AI · gemini-2.5-flash ▾]
```

Clicking either badge opens an inline **Session Control Popover**
anchored below the status bar. Critically, this is *not* the full
Settings dialog — it is a chat-local affordance for the two
operations a user does often (switch profile, toggle Local↔Vertex)
without context-switching out of the conversation.

Mock (profile badge clicked):

```
┌─ Session Control ──────────────────────────────┐
│  Profile                                       │
│  [Production GCP                          ▾]   │
│    └ default side: Vertex AI                   │
│                                                │
│  Active backend (this session, ephemeral)      │
│  ( ) Local       — google/gemma-4-26b-a4b      │
│  (•) Vertex AI   — gemini-2.5-flash            │
│                                                │
│  [Edit profiles in Settings →]      [Close]    │
└────────────────────────────────────────────────┘
```

Semantics:

- **Profile dropdown.** Lists all defined profiles. Selecting
  one is equivalent to `/profile <name>`: atomically rewrites
  `session.json`, emits `agent:profile:changed`, resets the
  active backend to the new profile's `default_backend` side
  (same "stronger statement than /model" semantics as §3.4).
- **Active backend radio.** Equivalent to `/model local` /
  `/model vertex`: ephemeral process-state toggle within the
  current profile. Not persisted to `session.json` (consistent
  with §2 non-goal). The model name beside each option is the
  profile's `Local.Model` / `VertexAI.Model`, read-only here.
- **Edit profiles in Settings →** link. Opens the LLM Profiles
  tab in the Settings dialog with the current profile selected.
  This is the only entry point for profile CRUD; the popover
  itself does not edit profile contents.
- **Close / outside click / Esc.** Dismisses the popover.

Busy-state gating: while `state == StateBusy` or
`extractionInFlight == true`, both controls render disabled with
tooltip "Busy — please wait." This reuses the same gate as
`/profile` and `/model` chat commands (§3.4).

Implementation: two new Wails bindings, both thin wrappers around
the same handlers `/profile` and `/model` use, so there is a
single source of truth for busy-state guard, event emission, and
side effects:

```go
// Bindings:
func (b *Bindings) SwitchSessionProfile(profileID string) error
func (b *Bindings) SwitchSessionBackend(backend string) error
```

The popover renders profile names by listing
`Bindings.ListProfiles()` (read-only summary; same binding the
Settings tab uses). The current selection is computed from
`agent.session.ProfileID` and `agent.backend` and updated via the
existing `agent:profile:changed` / `agent:backend:changed`
events.

Rationale: the status badges are passive "what am I in?"
information; the popover is the active "change it" surface. They
share the same anchor and the same visual vocabulary so users
don't need to learn two affordances.

---

## 4. Edge cases

1. **Two profiles requested with the same `Name`.** The Settings
   Save path auto-suffixes the second one with ` (2)`, ` (3)`, …
   (smallest free integer), matching the macOS Finder
   duplicate-name convention. A non-blocking yellow toast tells
   the user "Renamed to 'Production GCP (2)' because the name was
   taken." Profile UUIDs are independent, so session bindings
   are unaffected. The §3.4 `/profile <name>` ambiguity-error
   branch becomes defensive code that only fires when
   `config.json` is hand-edited to introduce duplicates outside
   the Settings flow.

2. **Deleting the active session's profile from Settings.** The
   session in memory keeps running on the old profile's configs
   (already loaded into `agent.useLocalBackend` / `useVertexAIBackend`
   closure state). On next session load (after switch or app
   restart), the fallback in §3.3 step 3b kicks in. No live
   disruption to the current chat.

3. **Renaming a profile.** UUID stable, only `Name` changes. All
   sessions referencing it continue to resolve correctly. The
   `config:profile:list_changed` event triggers the UI to refetch
   profile names so badges update.

4. **`/profile` with zero arguments.** Lists all profiles with
   the current session's profile marked. No state change.

5. **`/profile <nonexistent>`.** Error message; no state change.

6. **`/profile` during `StateBusy` or extraction in flight.**
   Reject with `ErrBusy`. Reuses the existing busy-state guard
   in `Agent.handleCommand` / `Agent.handleModelCommand`.

7. **Setting a new default while the current session uses the
   old default.** No effect on the current session (it still
   references the old default's UUID). The change affects:
   - Newly created sessions (get the new default).
   - Sessions whose referenced profile was deleted (fall back to
     the new default).

8. **Last remaining profile.** Cannot be deleted (UI gates).
   Conceptually: if the user removed it, what would the next
   `/model` toggle against? Forced minimum of one profile.

9. **Profile deletion with no sessions referencing it.** Direct
   delete; no fallback work needed.

10. **Profile deletion with N sessions referencing it.**
    Confirmation: "N session(s) reference this profile. They will
    use the default profile on next load. Continue?" Lazy fallback
    (§3.3 step 3b).

11. **Importing a v0.11.x export bundle into v0.12.0.** Bundle has
    no `session.json`. Import creates one pointing at the
    importing machine's default profile. Same as case (2a) above.

12. **`/model` referencing a side that the profile has not
    configured (e.g. empty VertexAI ProjectID).** Same as today
    — the backend's own readiness check (`useVertexAIBackend`
    refuses empty ProjectID) surfaces an error to the user. The
    profile abstraction adds no new failure mode.

13. **Config save concurrent with session load.** Sessions are
    loaded on user action (sidebar click); Config save is on
    Settings Save click. Both are user-driven and serial in
    practice. Defensive lock: `config.LLM.Profiles` is read-only
    after `Load()` returns until Settings Save triggers a fresh
    `Load()` cycle. Existing pattern from v0.11.x. No new locking
    needed.

14. **Migration of a torn old config.json.** If
    `applyBackendInheritance()` produces zero per-backend tokens,
    the migrated profile inherits those zeros — same behaviour as
    today. Migration is structure-only, not value-normalising.

---

## 5. Rejected alternatives

### 5.1 Independent `[]Local` + `[]VertexAI`

Rejected (§1). Breaks `/model`'s pair invariant. A session
referencing a specific local-by-id could see it vanish on config
edit.

### 5.2 Embed `profile_id` in `chat.json`

Rejected. Mixes conversation transcript with configuration state.
Forces every existing chat.json to grow a field, complicates
back-compat reads. The user explicitly preferred separating
session-level configuration into `session.json` (this ADR's §3.2).

### 5.3 Persist `active_backend` (the /model toggle) in `session.json`

Considered. Pros: session remembers whether the user was on
Local or Vertex; opening a session three days later lands on the
same side. Cons: behaviour change from v0.11.x where `/model` is
ephemeral; user explicitly asked for minimum session.json fields;
adds a write on every `/model` invocation. Deferred. Can be
added in a future ADR by bumping `session.json` schema_version
to 2.

### 5.4 Profile bundled with memory / sandbox / tools overrides

Considered. Pros: full per-context isolation (different sandboxes
per profile, different MCP profiles per profile, …). Cons:
massive scope expansion, conflicts with the v0.11.x global Memory
v2 design, harder to reason about. The user's stated need is
billing + endpoint separation, which only needs LLM config
profiling. Deferred indefinitely.

### 5.5 UUID-less stable string IDs (e.g. slugs from Name)

Rejected. Renames would orphan sessions silently. UUID v4 is
opaque and immutable; the human label is `Name` which can
change freely without breaking references.

### 5.6 Profile selector as a sidebar dropdown

Considered as one of three GUI surfaces (chat command, sidebar
dropdown, status-bar popover). Rejected in favour of the
status-bar popover (§3.10) because:
- The sidebar's job is the session list; a profile dropdown
  there competes with it visually and conflates "which session
  am I in" with "what config is this session running on".
- The popover surface is co-located with the existing backend
  badge, so a user already looking at "what am I running on"
  finds the change affordance one click away.
- The popover scales naturally to additional per-session
  controls in future ADRs (e.g. session-level sandbox toggle)
  without giving each its own sidebar slot.

`/profile` matches the precedent set by `/model` for power users;
the status-bar popover serves casual users. The Settings dialog
remains the entry point for creating, editing, deleting
profiles — operations that are intentionally heavier and
shouldn't live in a popover.

---

## 6. Tests / invariants

### 6.1 Backend (Go)

**`internal/config/`**:
- `TestMigrate_V011LegacyShape_CreatesDefaultProfile` — old
  `{default_backend, local, vertex_ai}` JSON migrates to one
  profile with synthesised UUID + Name "Default".
- `TestMigrate_V011LegacyShape_PreservesAllFields` — migrated
  profile's Local + VertexAI deep-equal the legacy values.
- `TestMigrate_AlreadyMigrated_NoChange` — config with
  `profiles[]` already present passes through unchanged.
- `TestMigrate_DanglingDefaultProfileID_RepairsToFirst` —
  `default_profile_id` references missing UUID → repaired to
  `profiles[0].ID`, warning logged.
- `TestResolveProfile_KnownID_ReturnsProfile`
- `TestResolveProfile_UnknownID_ReturnsDefault`
- `TestResolveProfile_EmptyID_ReturnsDefault`
- `TestDisambiguateName_NoCollision_ReturnsDesired`
- `TestDisambiguateName_OneCollision_AppendsSuffix2`
- `TestDisambiguateName_ChainOfCollisions_FindsLowestFree` —
  given names `["Foo", "Foo (2)", "Foo (3)"]`, request "Foo"
  → "Foo (4)".
- `TestDisambiguateName_GapInChain_FillsLowestFree` — given
  `["Foo", "Foo (3)"]` (no "Foo (2)"), request "Foo" → "Foo (2)"
  (Finder convention).
- `TestDisambiguateName_CaseInsensitive` — given `["Foo"]`,
  request "foo" → "foo (2)".
- `TestDisambiguateName_SelfIDExcluded` — rename of an existing
  profile whose current Name matches the requested one is a
  no-op (returns desired unchanged).

**`internal/memory/`**:
- `TestSessionConfig_LoadMissing_ReturnsZero` — no session.json
  → returns `SessionConfig{}` and no error (caller fills in
  default).
- `TestSessionConfig_LoadMalformed_ReturnsError` — corrupted JSON
  → error surfaces; caller decides recovery.
- `TestSessionConfig_SaveAtomic` — concurrent saves don't tear
  file (same pattern as `TestSession_Save_Atomic`).

**`internal/agent/`**:
- `TestAgent_LoadSession_NoSessionJSON_CreatesDefault` — v0.11.x
  session loaded under v0.12.0 → session.json written pointing
  at default profile.
- `TestAgent_LoadSession_DeletedProfile_FallsBackToDefault` —
  session.json references UUID not in config → falls back +
  rewrites session.json.
- `TestAgent_NewSession_GetsDefaultProfile` — `NewSession` /
  `NewPrivateSession` produce sessions with default profile.
- `TestAgent_ProfileCommand_Switch` — `/profile foo` updates
  session.json + emits `agent:profile:changed` + resets backend
  to new profile's default side.
- `TestAgent_ProfileCommand_AmbiguousName_Errors`
- `TestAgent_ProfileCommand_UnknownName_Errors`
- `TestAgent_ProfileCommand_DuringBusy_Rejected`
- `TestAgent_ProfileCommand_DuringExtraction_Rejected`
- `TestAgent_SetBackend_UsesActiveProfilesPair` — `/model vertex`
  after `/profile B` uses B's VertexAI, not A's.

**`bindings.go`**:
- `TestBindings_ExportSession_OmitsSessionJSON` — bundle has no
  session.json entry.
- `TestBindings_ImportSession_AssignsDefaultProfile` — imported
  session's new session.json points at current default.

### 6.2 Frontend (manual smoke)

- Open Settings → LLM Profiles tab. Create a second profile via
  "Clone of Default". Modify VertexAI ProjectID. Save.
- Status-bar badges show profile name + backend + model.
- Click profile badge → Session Control Popover opens; profile
  dropdown shows both profiles; backend radio shows current
  selection.
- In popover, pick the other profile from the dropdown → status
  bar updates immediately; chat input remains in the same
  session; next LLM call uses the new profile's configs (verify
  via a `/model vertex` tool call hitting the new ProjectID).
- In popover, toggle backend radio Local ↔ Vertex → same effect
  as `/model`; popover and status bar both reflect change.
- Click "Edit profiles in Settings →" link → popover closes,
  Settings dialog opens on LLM Profiles tab with current profile
  pre-selected.
- During an agent turn (Busy or extractionInFlight), open popover
  → controls render disabled with tooltip; clicks blocked.
- Press Esc with popover open → popover closes; no state change.
- `/profile` (no args, chat command) lists both profiles with
  the active one marked — same data as the popover dropdown.
- `/profile <other>` switches via chat; popover dropdown
  (if visible) reflects the change.
- Settings → delete non-default profile while it has an active
  session referencing it elsewhere → confirmation dialog;
  delete; next time that session is opened, it falls back to
  default + status bar badge + popover dropdown reflect fallback.
- Try to delete the default profile → button disabled.
- Set a non-default profile as default; old default becomes
  deletable.

### 6.3 Structural

- `len(Config.LLM.Profiles) >= 1` after `Load()` returns.
- `Config.LLM.DefaultProfileID` resolves to an entry in
  `Profiles` after `Load()`.

---

## 7. Compatibility

### Breaking

- **`config.json` schema.** Top-level `llm.default_backend` /
  `llm.local` / `llm.vertex_ai` are gone after migration. Users
  who hand-edit the file post-upgrade must use the new
  `llm.profiles` shape. The one-shot migration runs on first
  load under v0.12.0; running v0.11.x on a v0.12.0-migrated
  config would parse `llm` as empty (the old shape's fields are
  missing) — `LLMConfig` zero values, no Local/VertexAI active.
  **Downgrading from v0.12.0 → v0.11.x is not supported** without
  manual config rollback. CHANGELOG entry documents this.

### Non-breaking

- **`chat.json` / `session_memory.json` / `findings.json` /
  `summaries.json` schemas:** unchanged.
- **Wails binding signatures:** unchanged. `Send`, `Abort`,
  `LoadSession`, `NewSession`, `ExportSession`, `ImportSession`,
  `IsBusy` all keep their current signatures.
- **New bindings:**
  - Profile CRUD (used by Settings → LLM Profiles tab):
    `ListProfiles() []ProfileSummary`,
    `CreateProfile(req CreateProfileReq) (ProfileSummary, error)`,
    `UpdateProfile(id string, req UpdateProfileReq) error`,
    `DeleteProfile(id string) (DeleteProfileResult, error)`,
    `SetDefaultProfile(id string) error`.
  - Session control (used by Session Control Popover, sharing
    the same handlers as the `/profile` / `/model` chat commands):
    `SwitchSessionProfile(profileID string) error`,
    `SwitchSessionBackend(backend string) error`.

### Migration

- One-shot at first v0.12.0 load of a v0.11.x `config.json`.
  Idempotent: second load is a no-op.
- Per-session lazy migrate of `session.json` (created on first
  load of each v0.11.x session in v0.12.0).
- No data loss possible — migration is structural restructure,
  not value transformation.

---

## 8. Phasing

Single PR (or one branch with sequenced commits). Suggested
commits:

1. `feat(config): LLMProfile type + multi-profile schema + migration`
   — pure config-layer change. Tests for migrate + resolve.
2. `feat(memory): SessionConfig type + session.json IO`
   — pure memory-layer change. Tests for load/save.
3. `refactor(agent): plumb profile through setBackend + LoadSession`
   — agent reads session.json on load, instantiates backend from
   the resolved profile's pair. Tests for fallback + new-session
   default.
4. `feat(agent): /profile command`
   — chat command parser + handler + busy-state gate. Tests for
   all `/profile` paths.
5. `feat(bindings): profile CRUD + ExportSession omits session.json`
   — Wails bindings + events. Tests for CRUD + export omission +
   import default assignment.
6. `feat(frontend): LLM Profiles tab + status badges + Session Control Popover`
   — Settings UI rewrite (LLM Profiles tab replacing the two
   per-backend tabs) + ChatStatusBar profile/backend badges +
   Session Control Popover component + handlers for
   `agent:profile:changed` / `agent:backend:changed` /
   `config:profile:list_changed`.
7. `docs: ADR-0016 status update + CHANGELOG v0.12.0 +
   architecture reference`
   — ADR Status → Implemented; architecture §2 mentions profile
   resolution; CHANGELOG entry.

---

## 9. Out of scope

- Per-session `active_backend` persistence (the `/model` toggle).
  Tracked as a possible v0.12.x add via session.json
  schema_version=2.
- Profile-level overrides for memory / sandbox / tools / MCP
  profiles. Each is a separate concern with its own design
  pressure; would balloon this ADR.
- Cross-machine profile sync / sharing. Profiles are local-only
  configuration.
- Profile-level model lists or model-fetching UI. The single
  `Model` field per side stays.
- Telemetry on which profile is used most. Privacy-leaning
  defaults: no telemetry of any kind.
