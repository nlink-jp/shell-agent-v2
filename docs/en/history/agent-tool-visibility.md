# Agent Tool Visibility — Design Document

> Date: 2026-05-02
> Status: Proposed for v0.1.21
> Scope: Remove the `hasData`-based dynamic filter on the
> analysis-tool set so the LLM can plan multi-step "load → query →
> analyse → report" workflows up front. Keep an opt-in escape hatch
> for users on weaker local backends.

## 1. Background

`agent.analysisTools(hasData bool)` returns a different tool list
depending on whether the active session has any data loaded:

- **`hasData == false`**: `load-data`, `reset-analysis`,
  `create-report`, `list-objects`, `get-object` (5 tools).
- **`hasData == true`**: the above plus `describe-data`,
  `query-sql`, `list-tables`, `query-preview`, `suggest-analysis`,
  `quick-summary`, `promote-finding`, `analyze-data`
  (13 tools total).

Inline comment (`internal/agent/tools.go:14-16`):

> When no data is loaded, only load-data and reset-analysis are
> exposed to keep the tool count low for local LLMs.

The rationale dates to early gemma2 / early gemma3, when the
local backend's tool-calling reliability degraded sharply past
~10 tools. The agent loop calls `buildToolDefs()` every round, so
the round-by-round re-evaluation papered over the planning gap:
the LLM would call `load-data`, see new tools appear next round,
then call `query-sql`. Tool chaining via re-evaluation works.

What it doesn't fix is **planning visibility**:

1. The LLM cannot accurately enumerate downstream steps in its
   first response. "I'll load this CSV and then run a SQL grouping
   by month" is a guess from the model — `query-sql` isn't in its
   tool list when it says that.
2. False declines: when a user phrases a query like "give me the
   monthly sales totals" without attaching data, the model sees
   no `query-sql`, no `analyze-data`, and may apologise that it
   can't perform that operation — even though `load-data` is
   sitting right there as the natural first step.
3. Tool-name hallucination: weaker local models may call a
   non-existent `query-sql` tool, get an `unknown tool` error,
   and lose a round to recovery.
4. `load-data`'s description doesn't currently advertise what
   becomes available after a successful load. The model has to
   trust that "the analysis database" implies queryable tooling,
   without ever seeing it listed.

## 2. Why now

The standard local model is now **gemma-4-26b-a4b** (MoE,
~4B active parameters per token). Modern local models in the
gpt-oss / gemma-4 / qwen3 generation comfortably handle 30–40
tools without the selection-accuracy regression that motivated
the original filter. The 30+ tool ceiling is exceeded today
already when MCP guardians + sandbox + analysis tools combine
in a single session, and v0.1.20 verification traces show
gemma-4 picking the right tool consistently across long
sequences (`load-data → describe-data → query-sql ×5 →
sandbox-export-sql → sandbox-run-python → create-report` etc.).

The trade-off has flipped: hiding 8 tools no longer measurably
helps selection, while the planning cost is real on every fresh
session.

## 3. Goals / Non-goals

### Goals

1. Expose every analysis tool to the LLM every round, regardless
   of whether data has been loaded.
2. Keep an opt-in escape hatch for users on weaker local
   backends — a config flag that restores the old filter,
   default OFF (new behaviour wins).
3. Update `load-data`'s description so the model can plan a
   load-then-query workflow in its first reply without guessing
   what comes next.
4. Verify against gemma-4-26b-a4b on (a) "summarise this CSV"
   with no attachment (does the model now propose `load-data`
   instead of declining?), and (b) tool selection accuracy at
   30+ total tools (no regression).

### Non-goals

- **No new analysis tool.** Same 11 tools, same shapes.
- **No change to MITL gating.** The Phase B unification from
  v0.1.20 already routes every analysis tool through
  `IsToolMITLRequired`; that stays.
- **No frontend change.** The Settings → Tools list already
  shows every analysis tool; what changes is whether the LLM
  sees them as callable per round.
- **No retroactive enforcement.** Existing sessions get the new
  behaviour on their next message; no migration.

## 4. Detailed design

### 4.1 Backend change (single function)

`internal/agent/tools.go`:

```go
// analysisTools returns tool definitions for data analysis.
//
// All 11 tools are exposed every round so the LLM can plan a
// load-then-analyse workflow up front. The tools themselves
// short-circuit (with a clear error the model can react to)
// when called against an empty session — load-data fills the
// session, after which the others operate normally.
//
// The legacy hasData-based filter is preserved behind
// cfg.Tools.HideAnalysisToolsUntilDataLoaded for users on
// weaker local backends where the extra tool count measurably
// hurts selection accuracy. Default off.
func analysisTools(hasData, hideUntilDataLoaded bool) []llm.ToolDef {
    tools := []llm.ToolDef{
        loadDataDef,        // always exposed
        resetAnalysisDef,
        createReportDef,
        listObjectsDef,
        getObjectDef,
    }
    if hideUntilDataLoaded && !hasData {
        return tools
    }
    return append(tools, dataDependentTools()...)
}
```

The signature gains a second bool. Callers in `agent.go`
(`buildToolDefs`, `ListTools`) read
`a.cfg.Tools.HideAnalysisToolsUntilDataLoaded` and pass it
through.

### 4.2 Config flag

`internal/config/config.go`:

```go
type ToolsConfig struct {
    ScriptDir                       string             `json:"script_dir"`
    MCPProfiles                     []MCPProfileConfig `json:"mcp_profiles"`
    DisabledTools                   []string           `json:"disabled_tools,omitempty"`
    MITLOverrides                   map[string]bool    `json:"mitl_overrides,omitempty"`
    // HideAnalysisToolsUntilDataLoaded restores the pre-v0.1.21
    // behaviour where data-dependent analysis tools (query-sql,
    // describe-data, analyze-data, ...) only appear in the LLM
    // tool list after a successful load-data. Default false.
    // Opt-in for users on weaker local backends where exposing
    // 30+ tools measurably hurts selection accuracy. See
    // docs/en/agent-tool-visibility.md.
    HideAnalysisToolsUntilDataLoaded bool              `json:"hide_analysis_tools_until_data_loaded,omitempty"`
}
```

No UI surface (Settings dialog). Power-user knob only;
documented in README + CHANGELOG. If demand surfaces post-
release, add a Settings → General toggle then.

### 4.3 `load-data` description update

Current:

> Load a data file (CSV, JSON, JSONL) from the HOST filesystem
> into the analysis database. Creates or replaces the table.
> Only use this for absolute host paths the user supplied, or
> files explicitly attached to the conversation. For files
> inside the sandbox /work directory ... call
> sandbox-load-into-analysis instead — load-data cannot reach
> into the container.

Add a trailing sentence:

> Once loaded, the table is queryable via `query-sql`,
> `describe-data`, `list-tables`, `query-preview`,
> `suggest-analysis`, `quick-summary`, and `analyze-data`; use
> `promote-finding` to save insights, `create-report` to
> assemble a report.

Also update tool-doc reference inside the system prompt
(`internal/chat/chat.go` `sandboxGuidance` is conditional;
analysis tools have no such inlined block — the description on
each tool def is the only LLM-visible documentation).

### 4.4 Tests

Update existing tests:

- `TestAnalysisToolsFiltering` and
  `TestAnalysisToolsFilteringWithNewTools` currently assert "5
  tools when hasData=false, 13 when true". Refactor to:
  - Default config (`hide=false`): 13 tools regardless of
    `hasData`.
  - Legacy config (`hide=true`): 5 vs 13 split.
- New test
  `TestAnalysisTools_FullSetByDefault_AllowsPlanning` confirms
  `query-sql` etc. are present even when no data has been
  loaded.
- New test
  `TestAnalysisTools_HideFlagRestoresLegacyBehaviour` sets the
  flag and asserts the old split.

`TestListTools_MITLDefaultMatchesGate` (the v0.1.20
contract test) keeps working because it iterates over
`ListTools()` regardless of count.

## 5. Touched files

| File | Change |
|---|---|
| `internal/agent/tools.go` | `analysisTools` signature + body; refactor inline tool defs into named slices; update `load-data` description |
| `internal/agent/tools_test.go` | refactor existing filtering tests + add 2 new |
| `internal/agent/agent.go` | thread `cfg.Tools.HideAnalysisToolsUntilDataLoaded` through `buildToolDefs` and `ListTools` |
| `internal/config/config.go` | new field on `ToolsConfig` |
| `internal/config/config_test.go` | default value test |
| `bindings.go` | (no change — tool-list flow unchanged at binding layer) |
| `frontend/src/dialogs/SettingsDialog.tsx` | (no change — flag is config-only) |
| `docs/en/agent-tool-visibility.md` | this file |
| `docs/ja/agent-tool-visibility.ja.md` | translation |
| `CHANGELOG.md` | v0.1.21 entry |
| `AGENTS.md` | gotcha update |
| `README.md` / `README.ja.md` | mention new behaviour + escape hatch |
| `TODO.md` | remove the deferred entry (now done) |

## 6. Test plan

### Unit

- `go test ./internal/agent/ -tags no_duckdb_arrow -race` —
  refactored filtering tests + 2 new.
- `go test ./internal/config/ -race` — default value test.

### Manual smoke (gemma-4-26b-a4b)

1. **Fresh session, no data, ask analytical question**. Type
   "今月の売上トップ商品を教えて" without attaching data. The
   model should propose `load-data` (asking for the file path)
   rather than declining.
2. **Fresh session, attach CSV in same message**. "Load
   `/Users/.../sales.csv` and tell me the top product." The
   model should chain load-data → describe-data → query-sql in
   a single planned response, not in multiple round-trips.
3. **Selection accuracy at 30+ tools**. With sandbox enabled
   and at least one MCP guardian active, count the tool list:
   should be ≥ 30. Run a load + query + report sequence and
   confirm no `unknown tool` errors and no obviously-wrong
   tool picks.
4. **Legacy mode**. Set
   `tools.hide_analysis_tools_until_data_loaded: true` in
   `config.json`, restart, repeat (1) — the model should now
   either decline or only know about `load-data`.

## 7. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Some weaker local model degrades on the wider tool list | Document the escape hatch prominently in README + CHANGELOG; encourage one-line config edit if users see regression |
| Existing user `MITLOverrides` get more entries surfaced now that the tools are always visible in the dispatcher's view | No behavioural change — `IsToolMITLRequired` already consults overrides; the resolution is identical |
| `load-data` description becomes long enough to bloat the system prompt | Net change ~50 tokens. Negligible against typical 4K–8K context budgets |
| Model calls `query-sql` etc. against an empty session | Each tool already returns an explicit error ("no tables loaded" / "table not found"); model sees the error and recovers, same as today's any-tool-error flow |

## 8. Out of scope

- A Settings → General toggle for the flag. Add later if demand
  appears.
- Per-tool visibility flags (e.g. "hide `analyze-data` even
  when data is loaded"). The existing `DisabledTools` config
  already covers that case.
- Rewriting `analysisTools` into a registration pattern.
  Future cleanup; the current inline-defs style is fine for
  this change.
