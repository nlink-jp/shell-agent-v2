# Frontend Decomposition — Design Document

> Date: 2026-04-30
> Status: Draft — pending implementation
> Scope: `app/frontend/src/App.tsx` only; CSS, build, and Wails
> bindings stay untouched

## 1. Problem

`App.tsx` is 1457 lines as of v0.1.14 and growing. Single file
holds:

- the `window.go.main.Bindings` global declaration (~50 lines)
- 10+ TypeScript interfaces (Settings, Finding, ObjectInfo, …)
- three hand-rolled subcomponents (`MessageItem`, `BulkActions`,
  `BackendBudgetEditor`)
- the `App` component itself: 30+ `useState`s, ~10 effects, all
  the event handlers, the entire sidebar tree, the chat pane,
  four overlay dialogs (MITL, Settings, lightbox, report
  viewer), and the cmd-popup.

Symptoms:

- Code review of any change touches the same file every time.
- Search-and-replace across the file is risky because some
  identifiers (`status`, `active`, `settings`) appear in many
  unrelated contexts.
- New contributors (and tooling) have to re-read 1k+ lines to
  understand a small change.
- Hot-reload latency in `wails dev` grows with file size.

## 2. Goals & Non-goals

### Goals

- Reduce `App.tsx` to a coordinating shell (~400–500 lines)
  that owns top-level state and renders sub-components.
- Split each major UI surface (Settings dialog, sidebar,
  overlays) into its own file.
- Keep the runtime behaviour byte-identical: same DOM, same
  CSS classes, same event order, same Wails bindings.

### Non-goals

- **No state management library.** Zustand / Redux / Context
  refactors are deferred. State stays as `useState` /
  `useRef` in `App`, passed down via props. The point is to
  make the file boundary clean, not to redesign data flow.
- **No CSS reorganisation.** `App.css` rules referenced by
  the moved JSX continue to work because class names are
  unchanged.
- **No new abstractions** that didn't exist before. Helper
  hooks (`useMITL`, `useSidebarPrefs`) are out of scope —
  consider after the file split, only if they make a follow-up
  feature simpler.

## 3. Target File Layout

```
app/frontend/src/
├── App.tsx                     ~450  (was 1457)
├── App.css                     unchanged
├── themes.css                  unchanged
├── ChatInput.tsx               unchanged
├── ObjectImage.tsx             unchanged
├── DataDisclosure.tsx          unchanged
│
├── types.ts                    ~120   NEW — Settings, Finding, ObjectInfo,
│                                       MessageData, ChatMessage, SessionInfo,
│                                       LLMStatus, etc.
├── bindings.ts                 ~70    NEW — `window.go.main.Bindings`
│                                       global declaration + the
│                                       union of method signatures.
│
├── components/
│   ├── MessageItem.tsx         ~110   moved from App.tsx
│   ├── BulkActions.tsx         ~60    moved from App.tsx
│   └── BackendBudgetEditor.tsx ~30    moved from App.tsx
│
├── sidebar/
│   ├── Sidebar.tsx             ~190   accordion + Sessions panel + Memory
│   │                                  panel + bottom-nav. Most of the
│   │                                  current 756–944 range.
│   └── (no further split for v1 — keep panels inline within Sidebar.tsx
│        unless they need re-use)
│
├── dialogs/
│   ├── SettingsDialog.tsx      ~280   the v0.1.10 Settings overlay
│   │                                  (1167–1415). Tabs: General /
│   │                                  Tools / MCP. Accepts
│   │                                  `{settings, onChange, onClose,
│   │                                  tools, mcpStatus}` props.
│   ├── MITLDialog.tsx          ~130   1047–1166. Approve / Reject /
│   │                                  Reject-with-feedback.
│   ├── Lightbox.tsx            ~30    1416–1422 + the existing
│   │                                  styling.
│   └── ReportViewer.tsx        ~50    1423–1456 (expanded report
│                                       full-screen viewer).
```

## 4. Phasing

Each phase is a self-contained commit, leaves the app fully
working, and produces no DOM diff.

### Phase 1 — Type extraction (low risk)

Move every `interface` and `type` from `App.tsx` to
`types.ts`. The `window.go.main.Bindings` `declare global`
goes to `bindings.ts` (kept separate so bindings can grow
without polluting domain types).

`App.tsx` imports from both. No JSX or behaviour changes —
this is a pure cut-and-paste with import paths added.

Drop ~150 lines from `App.tsx`.

### Phase 2 — Subcomponent extraction (low risk)

Move `MessageItem`, `BulkActions`, `BackendBudgetEditor` into
`components/`. Each becomes a default export. Their props are
already explicit, so the only change is the import path.

Drop another ~200 lines.

### Phase 3 — Settings dialog (medium risk)

The Settings dialog is the largest contiguous chunk. Extract
to `dialogs/SettingsDialog.tsx` taking these props:

- `settings: Settings`
- `tools: ToolInfo[]`
- `mcpStatus: MCPStatus[]`
- `onUpdate(patch: Partial<Settings>): void` — the existing
  `updateSetting` callback
- `onClose(): void`

Inside the dialog the state for `settingsTab` and
`mcpExpanded` becomes local. The bindings for tools / MCP
restart / theme save remain in `App` (called via the
`onUpdate` patch pattern that already exists).

Drop ~280 lines.

### Phase 4 — Overlay dialogs (low risk)

Extract `MITLDialog`, `Lightbox`, `ReportViewer` to
`dialogs/`. Each has a small fixed prop set.

Drop ~210 lines.

### Phase 5 — Sidebar (medium risk)

Sidebar is tightly coupled to several pieces of `App` state:
sidebar panel selection, sessions list, findings, pinned
memory, etc. Extract as `sidebar/Sidebar.tsx` accepting all
those as props plus the relevant action callbacks
(`onSelectSession`, `onNewSession`, `onDeleteFinding`, etc.).

This is the phase with the most prop-drilling. Defer
introducing context until phase 6 (out of scope here) if the
prop list becomes unwieldy.

Drop ~190 lines.

### Phase 6 (post-MVP) — Optional refinements

If after phases 1–5 there's still pain:

- Pull large per-overlay JSX into smaller files
  (`SettingsTabGeneral.tsx`, `SettingsTabTools.tsx`,
  `SettingsTabMCP.tsx`).
- Introduce `useSidebarPrefs()` hook to capture the
  GitHub #4 width / collapse persistence pattern.
- Introduce a `BindingsContext` if prop-drilling gets bad.

These are speculative and trigger only on an explicit
follow-up complaint, not preemptively.

## 5. Test Strategy

There are no frontend unit tests today; verification is
manual via dev mode. For each phase:

1. `cd app && make build` succeeds with no TypeScript errors.
2. Spin up the built app, walk through the same checklist:
   - Send a message (Vertex + Local, both backends)
   - Toggle `/model`, verify backend badge updates
   - Open Settings, change theme + sandbox image + LLM
     timeout, save, confirm persistence after restart
   - Trigger a MITL prompt (run `query-sql`), Approve and
     Reject paths
   - Click a finding's session-link, verify session switch
   - Resize sidebar, collapse, restart — confirm Issue #4
     persistence still works
3. Diff CSS class names that appear in the DOM (visual
   regression check) — none should differ across a phase.

Run after every phase, not just at the end.

## 6. Risks & Mitigations

| Risk | Phase | Mitigation |
|---|---|---|
| Prop-drilling explosion in Sidebar | 5 | Accept it for v1; introduce context only if list grows beyond ~12 props. |
| Circular imports between `types.ts` and component files | 1–5 | `types.ts` exports only types, never imports from other component files. |
| Hot-reload edge cases with Wails dev server | all | Test each phase via `make dev`, not just `make build`. |
| Subtle re-render perf change | 2, 5 | `MessageItem` is `memo`-ed; preserve that. Other components stay vanilla. |

## 7. Out of Scope

- Adding component tests (separate concern; absent today,
  not made worse by this refactor).
- Replacing `react-markdown` plugin imports.
- The Wails JS bindings file generation (already auto-generated
  by `wails build`, not touched).

## 8. Rollback Plan

Each phase is a single commit. If verification fails for
phase N, `git revert` the phase commit; the previous phases
remain shippable.
