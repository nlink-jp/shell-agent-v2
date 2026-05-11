# Tool registry refactor (v0.6.0)

## 1. Motivation

v0.5.0 → v0.5.1 shipped six bug fixes in quick succession.
Five of them shared the same root cause: a tool name needed
to be added to a parallel list and the developer forgot
exactly one of the five (or six) places that need to mirror
each other. The symptoms were varied — "unknown tool" at
dispatch time, tools missing from the Settings → Tools
panel, broken bubble previews — but the underlying mistake
was always "we added the tool to N-1 places, not N".

This document specifies a refactor that makes the parallel-
list pattern structurally impossible. Each tool gets defined
once in a `ToolDescriptor` value; every existing surface
that used to enumerate tool names (LLM tool-def builder,
inner dispatcher, outer dispatcher case-label, MITL default
map, MITL category switch, Settings UI catalogue, builder-of-
builders) becomes a *view function* that derives its output
from the canonical descriptor slice.

No behaviour changes for the LLM or end user. The same tools
appear in the same order with the same descriptions; the same
MITL defaults fire on the same dispatches; the same Settings
toggles work the same way. The refactor is purely a code-
hygiene change with a structural safety guarantee: future
tools can be added by editing one descriptor list, and the
compiler verifies the rest.

## 2. Current state

`docs/en/markdown-attachments.md` shipped three new tools
(`analyze-text`, `grep-text`, `get-text`). Doing so correctly
required edits in **seven** places, of which the developer
missed two on the first attempt:

| # | Location | What it carries | Drift risk |
|---|----------|-----------------|------------|
| 1 | `internal/agent/tools.go:30` `analysisToolMITLDefault` | `map[string]bool` of per-tool MITL defaults | **CRITICAL** — silently flips the security default for missing tools |
| 2 | `internal/agent/tools.go:53` `analysisToolMITLCategory()` switch | UI-side category overrides (`sql_preview`, `analysis_plan`) | LOW — falls through to "write" if missed |
| 3 | `internal/agent/tools.go:81` `analysisTools()` builder | `llm.ToolDef[]` with full descriptions + JSON Schema parameters | **CRITICAL** — tool isn't visible to the LLM at all if missed |
| 4 | `internal/agent/tools.go:310` `executeAnalysisTool()` switch | Inner dispatcher, calls the `toolXxx()` Go handler | **CRITICAL** — returns "unknown analysis tool: %s" if missed |
| 5 | `internal/agent/agent.go:1743` `executeTool()` outer case-label | Outer dispatcher; routes to `executeAnalysisTool()` or other branches | **CRITICAL** — returns "unknown tool %q" if missed (the v0.5.1 bug) |
| 6 | `internal/agent/agent.go:980` `ListTools()` analysis section | Hand-coded `ToolInfoItem` per tool for Settings → Tools UI | **CRITICAL** — tool invisible in Settings if missed (the v0.5.1 bug) |
| 7 | `internal/findings/findings.go:43` `Source*` constants | Provenance enum for auto-promoted findings | LOW — only matters for tools that emit findings |

Plus a few **secondary** drift surfaces that aren't strictly
parallel lists but tend to bit-rot in the same way:

- **Description duplication**: `analysisTools()` (#3) and
  `ListTools()` (#6) each have a complete prose description
  per tool. If the description is edited in one, the other
  diverges silently.
- **Test count assertions**: `tools_test.go` asserts the
  exact tool count (9 in legacy no-data, 17 in legacy with-
  data). Every new tool requires three places to be updated
  in the same commit.

### 2.1. Sandbox tools — half-refactored already

`internal/agent/sandbox_tools.go` shows a partial improvement:

- `sandboxToolDefs()` is the canonical list (one `llm.ToolDef`
  per tool).
- `ListTools()` *iterates* `sandboxToolDefs()` rather than
  re-listing the names — drift-free.
- BUT `executeSandboxTool()` switches on names independently,
  so a name typo or new tool addition still risks the
  "unknown sandbox tool" failure.

So the sandbox surface has 2 parallel lists (defs + switch)
instead of 6 — better, but not yet single-source.

### 2.2. Shell tools and MCP — already correct

`internal/toolcall/Registry` and `internal/mcp/Guardian` both
follow the ideal pattern: tools are defined in *one* place
(parsed from script headers or returned by the MCP server),
and every surface — `executeTool()` dispatch, `ListTools()`
UI — *looks them up by name*. New tools appear automatically.

These are the model we want for analysis and sandbox.

### 2.3. Special-case tools that defeat naïve consolidation

Four tools are listed under `analysisTools()` and `ListTools()`
analysis section but are dispatched directly by `executeTool()`
without going through `executeAnalysisTool()`:

- `resolve-date` (builtin, calls `chat.ResolveDate()`)
- `list-objects` (objstore read)
- `get-object` (objstore read)
- `register-object` (objstore write + inline MITL check)

These don't depend on the analysis engine (`a.analysis`), so
they don't fit the "delegate to executeAnalysisTool" pattern
that requires the engine to exist. The refactor must
preserve this routing nuance — the descriptor needs to
express both "what source category" (for UI grouping) and
"what handler" (for dispatch), and the two can be loosely
coupled.

## 3. Design principles

- **Single source of truth per tool.** Each tool is defined
  exactly once. Every existing surface becomes a projection.
- **Behaviour-preserving.** No LLM-visible change, no
  Settings-visible change, no on-disk format change, no
  test-fixture change (apart from count assertions becoming
  dynamic). The same tools, in the same order, with the same
  MITL defaults.
- **Additive migration.** Phase boundaries are picked so each
  intermediate commit compiles, passes all tests, and runs
  the app identically. We don't need a flag day.
- **Compose with the good patterns we already have.** Shell
  tools and MCP guardians are *not* refactored — they already
  have single-source registries. The new `ToolDescriptor`
  becomes one of several tool sources the outer dispatcher
  consults, alongside the existing dynamic registries.
- **Compile-time safety where possible, runtime where
  necessary.** Handler closures are typed; tool names are
  still string keys (no compiler check for typos there), but
  the consequence of a typo becomes "the tool doesn't exist
  in the descriptor map" rather than "the tool is half-
  registered across N lists".

## 4. Architecture

### 4.1. ToolDescriptor

```go
// ToolDescriptor is the single source of truth for a tool —
// the same value backs the LLM tool-def, the Settings UI
// entry, the MITL default, and the dispatch handler.
type ToolDescriptor struct {
    // --- Identity ---
    Name string

    // --- LLM-facing ---
    Description string
    Parameters  any // JSON Schema (`map[string]any` etc.)

    // --- UI / classification ---
    // Category is "read" | "write" | "execute" — drives the
    // generic MITL confirmation dialog. Specific MITL
    // categories (sql_preview, analysis_plan) are signalled
    // via MITLCategoryOverride; Category remains the fallback
    // / default.
    Category string

    // Source is "analysis" | "builtin" | "sandbox" | "shell"
    // | "mcp". Surfaces in ToolInfoItem for the Settings UI
    // to group entries by origin. NOT used by the dispatcher
    // — that uses Handle directly.
    Source string

    // --- MITL ---
    MITLDefault bool

    // MITLCategoryOverride is non-empty when the UI should
    // render a specialised confirmation (currently:
    // "sql_preview" for query-sql, "analysis_plan" for
    // analyze-data). Empty falls back to Category.
    MITLCategoryOverride string

    // --- Visibility ---
    // HideUntilDataLoaded is true for tools that legacy mode
    // hides until the analysis engine has loaded at least
    // one table. The name mirrors the existing config flag
    // `cfg.Tools.HideAnalysisToolsUntilDataLoaded` so the
    // policy and its consumer line up. Most tools leave this
    // false (always visible).
    HideUntilDataLoaded bool

    // --- Dispatch ---
    // Handle is the tool's executor. Closures capture the
    // *Agent at descriptor-construction time so the
    // descriptor list can be a method on *Agent.
    Handle func(ctx context.Context, args string) (string, ActivityEventStatus)
}
```

### 4.2. Per-agent descriptor list

The full descriptor list lives on the agent so handler
closures can capture `a` for objstore / analysis engine /
session access:

```go
func (a *Agent) buildToolDescriptors() []ToolDescriptor {
    return concat(
        a.builtinDescriptors(),
        a.analysisDescriptors(),
        a.sandboxDescriptors(), // nil if a.sandbox == nil
    )
}
```

Each per-source builder returns its tools; the agent's
constructor (`New()`) calls `buildToolDescriptors()` once
and caches the result in `a.toolDescriptors` plus a
`map[string]ToolDescriptor` index for O(1) lookup.

### 4.3. View functions become projections

The six surfaces from §2 collapse into derivations:

```go
// Replaces analysisTools() — derives LLM tool defs from
// the descriptor list, filtering by visibility.
// legacyMode is cfg.Tools.HideAnalysisToolsUntilDataLoaded;
// renamed locally to avoid shadow-clashing with the field
// HideUntilDataLoaded on each descriptor.
func (a *Agent) toolDefsForLLM(hasData, legacyMode bool) []llm.ToolDef {
    out := make([]llm.ToolDef, 0, len(a.toolDescriptors))
    for _, d := range a.toolDescriptors {
        if d.HideUntilDataLoaded && legacyMode && !hasData {
            continue
        }
        out = append(out, llm.ToolDef{
            Name: d.Name, Description: d.Description, Parameters: d.Parameters,
        })
    }
    return out
}

// Replaces analysisToolMITLDefault + the sandbox-prefix
// lookup — both now go through the descriptor.
func (a *Agent) toolMITLDefaultFromRegistry(name string) (bool, bool) {
    d, ok := a.toolDescriptorByName(name)
    if !ok {
        return false, false
    }
    return d.MITLDefault, true
}

// Replaces analysisToolMITLCategory() switch.
func (a *Agent) toolMITLCategory(name string) string {
    d, _ := a.toolDescriptorByName(name)
    if d.MITLCategoryOverride != "" {
        return d.MITLCategoryOverride
    }
    return d.Category
}

// Replaces the executeTool() outer case-label + the inner
// executeAnalysisTool() switch.
func (a *Agent) dispatchDescriptor(ctx context.Context, tc llm.ToolCall) (string, ActivityEventStatus, bool) {
    d, ok := a.toolDescriptorByName(tc.Name)
    if !ok {
        return "", ActivityStatusError, false // not a descriptor tool, caller tries other sources
    }
    if a.IsToolMITLRequired(tc.Name) {
        if rejection := a.requestMITL(tc.Name, tc.Arguments, a.toolMITLCategory(tc.Name)); rejection != "" {
            return rejection, ActivityStatusError, true
        }
    }
    result, status := d.Handle(ctx, tc.Arguments)
    return result, status, true
}

// Replaces ListTools() analysis + sandbox sections —
// iterates descriptors. Shell tools and MCP tools are still
// listed via their existing dynamic registries.
func (a *Agent) ListTools() []ToolInfoItem {
    items := make([]ToolInfoItem, 0, len(a.toolDescriptors))
    hasData := a.analysis != nil && a.analysis.HasData()
    legacyMode := a.cfg.Tools.HideAnalysisToolsUntilDataLoaded
    for _, d := range a.toolDescriptors {
        if d.HideUntilDataLoaded && legacyMode && !hasData {
            continue
        }
        items = append(items, ToolInfoItem{
            Name:        d.Name,
            Description: d.Description,
            Category:    d.Category,
            Source:      d.Source,
            MITLDefault: d.MITLDefault,
        })
    }
    // Then iterate shell + MCP + sandbox-not-yet-migrated
    // sources as today.
    return items
}
```

### 4.4. Outer dispatcher

`executeTool()` becomes a small router that tries each tool
source in turn:

```go
func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, ActivityEventStatus) {
    tc.Arguments = normalizeToolArgs(tc.Arguments)

    // 1. Descriptor registry (analysis + builtin + sandbox).
    if result, status, handled := a.dispatchDescriptor(ctx, tc); handled {
        return result, status
    }
    // 2. MCP guardians (existing dynamic registry).
    if strings.HasPrefix(tc.Name, "mcp__") {
        return a.dispatchMCP(ctx, tc)
    }
    // 3. Shell-tool registry (existing).
    if tool, ok := a.toolRegistry.Get(tc.Name); ok {
        return a.dispatchShell(ctx, tc, tool)
    }
    return fmt.Sprintf("Error: unknown tool %q", tc.Name), ActivityStatusError
}
```

Three explicit branches, each lookup-by-key. No giant case
labels enumerating tool names.

### 4.5. Where each tool lives after the refactor

| Tool | Source | Handler |
|------|--------|---------|
| resolve-date | builtin | `chat.ResolveDate` wrapped |
| list-objects | builtin | `a.toolListObjects` |
| get-object | builtin | `a.toolGetObject` |
| register-object | builtin | `a.toolRegisterObject` (+ inline MITL via descriptor) |
| load-data | analysis | `a.toolLoadData` |
| describe-data | analysis | ... |
| query-sql | analysis | ... |
| query-preview | analysis | ... |
| suggest-analysis | analysis | ... |
| quick-summary | analysis | ... |
| list-tables | analysis | ... |
| reset-analysis | analysis | ... |
| create-report | analysis | ... |
| promote-finding | analysis | ... |
| analyze-data | analysis | ... |
| analyze-text | analysis | `a.toolAnalyzeText` |
| grep-text | analysis | `a.toolGrepText` |
| get-text | analysis | `a.toolGetText` |
| sandbox-* (8 tools) | sandbox | individual `a.toolSandbox*` |

Shell-tool descriptors are built dynamically by wrapping
`toolcall.Tool` values (existing registry, no behaviour change
in the registry itself). MCP-tool descriptors are NOT
materialised — MCP tools stay in the dispatcher's prefix
branch because they're discovered at runtime and have no
static metadata to put in a descriptor at build time.

## 5. Migration phases

Each phase is one or two commits, builds clean, tests pass.
No phase introduces an LLM-visible change.

### Phase 1: Introduce `ToolDescriptor` type and helper map (no consumers yet)

- New file `internal/agent/tool_descriptor.go` with the
  struct definition and `toolDescriptorByName(name) (ToolDescriptor, bool)`
  helper.
- Agent struct gains `toolDescriptors []ToolDescriptor` and
  `toolDescriptorIndex map[string]int`.
- `New()` initialises these to empty slices for now.
- No behaviour change. Compiles, passes all tests.

### Phase 2: Migrate analysis + builtin tools

Broken into ten small sub-commits so each step independently
compiles, tests pass, and bisect can land on the precise
view-function migration that breaks anything. Each sub-commit
is < 100 lines net change.

- **2a** New file `internal/agent/tool_descriptors_analysis.go`
  defines `a.analysisDescriptors()` returning the 14 analysis
  tools as `ToolDescriptor` values. No consumer yet — just the
  data.
- **2b** New file `internal/agent/tool_descriptors_builtin.go`
  defines `a.builtinDescriptors()` returning the 4 intercepted
  tools (resolve-date, list-objects, get-object, register-object).
  No consumer yet.
- **2c** `New()` populates `a.toolDescriptors` from the two
  builders, plus the `toolDescriptorIndex` map for O(1)
  lookup. Helper `toolDescriptorByName()` exposed. Still no
  consumer of the new data — old paths run unchanged.
- **2d** Migrate `analysisTools()` to derive its `[]llm.ToolDef`
  output from the descriptor list. Bit-identical output before
  / after — verified by manual diff during the commit.
- **2e** Migrate `executeAnalysisTool()` to dispatch via
  `descriptor.Handle` instead of a switch. Switch deleted in
  the same commit.
- **2f** Migrate the outer `executeTool()` analysis case-label
  to call `dispatchDescriptor()`. Case label deleted.
- **2g** Migrate `analysisToolMITLDefault` map lookups to a
  derived view from descriptors. Map deleted.
- **2h** Migrate `analysisToolMITLCategory()` switch to derive
  from `descriptor.MITLCategoryOverride`. Switch deleted.
- **2i** Migrate `ListTools()` analysis + builtin sections to
  iterate the descriptor list. Hand-listing deleted.
- **2j** Update `tools_test.go` count assertions to derive
  from `len(a.toolDescriptors)` rather than hard-coded 9 / 17.
  Final cleanup commit; all paths now derive from descriptors.

### Phase 3: Migrate sandbox tools

Mirrors Phase 2's sub-commit shape. Six sub-commits:

- **3a** New file `internal/agent/tool_descriptors_sandbox.go`
  defines `a.sandboxDescriptors()` returning the 8 sandbox
  tools as `ToolDescriptor` values.
- **3b** `New()` conditionally appends them to
  `a.toolDescriptors` when `a.sandbox != nil`.
- **3c** Rewrite `executeSandboxTool()` to dispatch via
  descriptor `Handle` instead of a switch (or delete it
  entirely and let `dispatchDescriptor()` handle sandbox
  names too).
- **3d** Migrate `sandboxToolDefs()` to derive its output
  from the descriptor list.
- **3e** Migrate `ListTools()` sandbox section to be part of
  the unified descriptor iteration. Separate sandbox loop
  deleted.
- **3f** Remove the outer dispatcher's `sandbox-*` prefix
  branch (now redundant — `dispatchDescriptor()` handles
  sandbox names through the descriptor index).

### Phase 4: Skipped

The original plan considered wrapping shell tools as
descriptors so `ListTools()` would become a single uniform
loop. After review, this was dropped: shell tools already
follow the ideal pattern (`internal/toolcall/Registry` is a
single source of truth, `executeTool()` already does
single-line lookup, `ListTools()` already iterates the
registry). Adding a descriptor wrapper would only convert one
clean iteration into another with extra wrapper code, with no
drift-prevention payoff.

Same reasoning applies to MCP tools — they're discovered at
runtime from guardian processes, no static descriptor possible.

### Phase 5: Tests + docs

- Update `tools_test.go` so count assertions reference the
  descriptor slice rather than literal `9` / `17`.
- New `tool_descriptor_test.go` with:
  - `TestToolDescriptors_UniqueNames` — no duplicates.
  - `TestToolDescriptors_AllHaveHandlers` — every entry's
    `Handle` is non-nil.
  - `TestToolDescriptors_MITLDefaultsMatchHistoricalMap` —
    asserts the new descriptor MITL defaults match the
    pre-refactor `analysisToolMITLDefault` map for every
    name that was in the old map. This is the migration
    safety net.
  - `TestToolDescriptors_DescriptionsMatchLLMOutput` — sanity
    that `toolDefsForLLM()` output is bit-identical to the
    pre-refactor `analysisTools()` output (modulo Description
    text now coming from a single source, which is the whole
    point).

- Update `docs/en/architecture.md` §3 (packages) and the
  "Recent design notes" list. Add the new file to AGENTS.md
  pointers.

### Phase 6: Release

`v0.6.0` chore commit, CHANGELOG entry, tag, push, gh
release with asset, submodule bump, check-org.sh. Same nine-
step pattern as every previous release.

## 6. Tests

### 6.1. Pre-existing tests that must keep passing

- `TestAnalysisToolsFiltering` — count assertions become
  derived from descriptor slice length.
- `TestAnalysisTools_HideFlagRestoresLegacyBehaviour` —
  same.
- `TestListTools_MITLDefaultMatchesGate` — verifies that
  every tool the UI shows has its `MITLDefault` matching
  what the dispatcher would actually do. Becomes a stronger
  guarantee post-refactor because both sides derive from the
  same descriptor.
- `TestIsToolMITLRequired_AnalysisDefaultsMatchTable` —
  refactored to use the descriptor MITL defaults rather than
  the old map.

### 6.2. New tests

Four structural tests that protect against the v0.5 drift bug
classes by construction. No migration-only tests (the historic
MITL-default-map snapshot was considered and rejected — once
the refactor lands, the four tests below already cover the
contract surface).

- **`TestToolDescriptors_UniqueNames`**: no two descriptors
  share a name; protects against accidental duplicate entries.
- **`TestToolDescriptors_AllHaveHandlers`**: every descriptor's
  `Handle` is non-nil; protects against "I added the entry
  but forgot to wire the handler".
- **`TestDispatchDescriptor_RoutesAllNamesInLLMToolDefs`**:
  every name that `toolDefsForLLM()` returns must be
  dispatchable by `dispatchDescriptor()`. Catches the v0.5.1
  "case label missed" class of bug structurally.
- **`TestListTools_ContainsAllDescriptors`**: every descriptor
  appears in `ListTools()` output (modulo HideUntilDataLoaded
  filtering). The v0.5.1 "Settings tab missing tool" bug
  becomes structurally impossible.

## 7. Compatibility

- **Public API**: no Go-side public API change. `Agent`
  methods that backed bindings (`SendWithImages`, `LoadSession`,
  `ListTools`, etc.) keep their signatures and observable
  behaviour. `ToolInfoItem` / `ToolDef` / `MessageData` field
  shapes unchanged.
- **On-disk format**: no change. Chat records, objstore
  index, session memory — all untouched.
- **LLM-facing**: tool defs go out in the same order with the
  same descriptions and parameter schemas. The model sees
  no difference.
- **Settings UI**: same tool list, same MITL toggles, same
  category labels. Pure rendering — no React or CSS change
  needed.
- **Bindings.ts / wails-generated types**: no change. The
  refactor is pure Go-internal.

## 8. Risks

- **Closure-capture bugs.** Each descriptor's `Handle`
  captures `a *Agent` at construction. If `a` is recreated
  (e.g., re-init across sessions), the old closure could
  point at the dead instance. Mitigation: descriptors are
  rebuilt in `Agent.New()`, and `Agent` is never re-init in
  place — only freshly constructed. Tests cover this via
  the existing session-restore path.
- **Description drift between the LLM and the UI**.
  Currently they're independently maintained; after refactor
  they share a single `Description` field. This is a *fix*
  for v0.5.0's already-divergent descriptions (some had
  "image / blob / report" vs "image / blob / report /
  markdown"). The refactor commit will pick one canonical
  text per tool, which may produce a few user-visible string
  changes in Settings. CHANGELOG must call this out.
- **Closure init ordering.** Descriptors reference
  `a.toolLoadData` etc. as method values. Method values can
  be captured at any time after `a` exists, so this is safe;
  but if the agent ever splits into a phased init (e.g.,
  analysis engine isn't ready until first load), the
  descriptor list still gets built up front. Handlers will
  see `a.analysis == nil` and return the same error they do
  today.
- **Phase 2 is the largest body of change** (~600 lines net),
  but split across ten sub-commits (2a–2j) each < 100 lines
  net. Review burden per sub-commit is small; bisect can
  isolate any regression to one of the ten view-function
  migrations.

## 9. Out of scope

- **Shell-tool registry redesign.** Already single-source;
  Phase 4 is optional polish, not a behaviour change.
- **MCP tool descriptor materialisation.** MCP tools are
  discovered at runtime; static descriptors don't fit. The
  outer dispatcher keeps its `mcp__` prefix branch.
- **Tool category vocabulary changes.** Stays "read" /
  "write" / "execute"; MITL specials stay
  "sql_preview" / "analysis_plan". A future release can
  unify the vocabulary if needed.
- **LLM-side tool-call schema redesign.** The `llm.ToolDef`
  struct stays as-is. Vertex / local backends see no change.
- **Settings UI redesign.** The Settings → Tools tab keeps
  its layout; the underlying tool list just becomes a
  derived view.
- **Performance optimisation.** Descriptor lookup is
  `O(1)` via the index map; the iteration in `ListTools()`
  is `O(N)` where N ≈ 20-30. Negligible.

## 10. Resolved decisions

All four open questions from the design-review round are
resolved here for posterity.

1. **Phase 4 (shell-tool descriptor view): skipped.** Shell
   tools (`internal/toolcall/Registry`) already follow the
   ideal single-source pattern — the v0.5 drift bugs were not
   in this surface. Wrapping `toolcall.Tool` as a
   `ToolDescriptor` would convert one clean iteration into
   another with extra wrapper code, no drift-prevention
   payoff. Same logic applies to MCP guardians (which are
   dynamic and can't have static descriptors).
2. **Migration-only tests: dropped.** The originally proposed
   `TestToolDescriptors_MITLDefaultsMatchHistoricalMap` and
   `TestToolDescriptors_DescriptionsMatchLLMOutput` were
   migration safety nets intended for one-release use. The
   four structural tests in §6.2
   (UniqueNames / AllHaveHandlers /
   RoutesAllNamesInLLMToolDefs / ContainsAllDescriptors)
   already cover the contract surface; no need to keep dead
   weight.
3. **`ToolDescriptor.MITLCategoryOverride`: confirmed as the
   chosen mechanism.** Empty string falls back to `Category`
   (read/write/execute → generic confirmation dialog).
   Non-empty values like `"sql_preview"` and `"analysis_plan"`
   trigger frontend-side specialised dialogs (SQL preview,
   analysis-plan editor). Future special UIs add new strings
   here without touching dispatcher code.
4. **Phase 2 commit granularity: ten small sub-commits
   (2a–2j).** Each < 100 lines net, each independently
   compiles and passes tests. Bisect-friendly; no squash.
