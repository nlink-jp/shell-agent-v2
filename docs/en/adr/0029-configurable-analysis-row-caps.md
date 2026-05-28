# ADR-0029: Configurable analysis row caps (query + export)

- Status: Accepted
- Deciders: magi
- Related: ADR-0005 (analyze-data row cap — §5 foreshadowed this knob), ADR-0004 (sandbox uid mapping), `docs/en/reference/...` (analysis engine)

## 1. Context

GitHub issue #14 (user report):

> When extracting data via SQL and handing it off to the sandbox,
> the row limit is set to 10,000. This limit is frequently hit and
> data analysis cannot proceed. Because the cap is hardcoded, it
> cannot be changed via settings, forcing the user to give the
> agent awkward "split the data" instructions, or the agent
> proposes convoluted workarounds on its own. The data-analysis
> experience is poor.

The relevant path is `export-sql-to-csv` (`sandbox_tools.go` →
`Engine.QuerySQLToCSV`), which runs a SELECT and writes the rows
as CSV into the per-session sandbox `/work` directory. It is
capped at the same `MaxQueryRows = 10000` constant the interactive
chat-output tools use:

```go
// app/internal/analysis/engine.go
const MaxQueryRows = 10000          // chat-output cap
...
// QuerySQLToCSV
for rows.Next() {
    if rowCount >= MaxQueryRows {   // ← export path shares the chat cap
        return columns, rowCount, fmt.Errorf("query result exceeds %d rows; ...", MaxQueryRows)
    }
```

## 2. Root cause

There are conceptually **three** row-fetch paths, but only two caps:

| Path | Method | Rows enter the chat? | Current cap |
|------|--------|----------------------|-------------|
| Interactive query (`query-sql`, `query-preview`, `quick-summary`) | `QuerySQL` | **Yes** — JSON-serialised into the tool result | `MaxQueryRows` = 10 000 |
| Sliding-window analysis (`analyze-data`) | `QuerySQLForAnalyze` | No — chunked into per-window LLM calls | `MaxAnalyzeRows` = 1 000 000 |
| CSV export to sandbox (`export-sql-to-csv`) | `QuerySQLToCSV` | **No** — written to a file in `/work` | `MaxQueryRows` = 10 000 ❌ |

ADR-0005 already established the principle: *the 10 000-row cap
exists to stop an unbounded SELECT from flooding the LLM context;
it is correct only for paths whose rows land in the chat.* The
export-to-CSV path violates that principle — its rows never enter
the chat, yet it is throttled by the chat cap. This is the same
class of bug ADR-0005 fixed for `analyze-data`, left unfixed for
the export path because the export tool postdates ADR-0005.

Two distinct problems, then:

1. **Mis-shared cap (a latent bug).** `export-sql-to-csv` should
   use a backstop like `analyze-data`, not the chat cap.
2. **No configurability.** Even the chat cap is occasionally too
   low for power users, and ADR-0005 §5 explicitly deferred a
   config knob until a field report arrived. Issue #14 is that
   report.

## 3. Decision

Make both caps configurable, and fix the export path to default
to the high backstop (so the user's pain is gone even without
touching config).

### 3.1. Config surface

Add an `analysis` block to `config.Config`, following the
established `AgentConfig.MaxToolRounds` pattern (zero → resolved
default):

```go
// app/internal/config/config.go
type AnalysisConfig struct {
    // MaxQueryRows caps interactive chat-output queries
    // (query-sql / query-preview / quick-summary). 0 → DefaultMaxQueryRows.
    MaxQueryRows int `json:"max_query_rows,omitempty"`
    // MaxExportRows caps export-sql-to-csv (rows written to the
    // sandbox /work dir, never entering the chat). 0 → DefaultMaxExportRows.
    MaxExportRows int `json:"max_export_rows,omitempty"`
}

const DefaultMaxQueryRows  = 10_000     // chat-output: unchanged
const DefaultMaxExportRows = 1_000_000  // sandbox handoff: matches MaxAnalyzeRows

func (a AnalysisConfig) MaxQueryRowsResolved() int  { /* 0 → default */ }
func (a AnalysisConfig) MaxExportRowsResolved() int { /* 0 → default */ }
```

`Config` gains `Analysis AnalysisConfig json:"analysis,omitzero"`.
Old configs without the block resolve to the defaults — no
migration needed.

### 3.2. Engine: per-instance caps, not package constants

`MaxQueryRows` becomes the package default `DefaultMaxQueryRows`;
the `Engine` carries instance caps so each session's engine can be
configured independently:

```go
type Engine struct {
    ...
    maxQueryRows  int // 0 → DefaultMaxQueryRows
    maxExportRows int // 0 → DefaultMaxExportRows
}

// SetRowCaps configures the query / export caps. 0 keeps the default.
func (e *Engine) SetRowCaps(maxQuery, maxExport int)

func (e *Engine) MaxQueryRows() int  // resolved
func (e *Engine) MaxExportRows() int // resolved
```

- `QuerySQL` → bounded by `e.MaxQueryRows()` (was the const).
- `QuerySQLToCSV` → bounded by `e.MaxExportRows()` (**changed**:
  was `MaxQueryRows`). This is the core fix.
- `QuerySQLForAnalyze` → unchanged, keeps the `MaxAnalyzeRows`
  package var (its test seam `setMaxAnalyzeRowsForTesting` stays).

`MaxAnalyzeRows` is intentionally **not** folded into the config
in this ADR — it is a pure memory backstop the user can't
meaningfully reason about, and ADR-0005 §7 keeps it out of scope
until evidence says otherwise. Issue #14 is about the export and
query paths, not analyze.

### 3.3. Wiring

- `bindings.go::switchAnalysis` calls
  `b.analysis.SetRowCaps(cfg.Analysis.MaxQueryRowsResolved(),
  cfg.Analysis.MaxExportRowsResolved())` after constructing the
  per-session engine, so every new session's engine honours the
  config.
- `SaveSettings` applies the new caps to the **currently live**
  engine as well, so a change takes effect without a session
  switch or app restart (consistent with how logger level and
  sandbox config already apply live).

### 3.4. Settings UI

Add a *Data analysis* section to the General tab (next to
*Agent loop*), with two number inputs:

- **Max rows per query result** → `max_query_rows`, default 10 000.
  Hint: rows returned into the chat; raising it sends more data to
  the LLM and uses more context.
- **Max rows for CSV export to sandbox** → `max_export_rows`,
  default 1 000 000. Hint: rows written to a file in the sandbox;
  these never enter the chat, so the ceiling is memory, not
  context.

`SettingsData` (the bindings DTO) gains `MaxQueryRows` and
`MaxExportRows`, mirrored in `frontend/src/types.ts`'s `Settings`.

### 3.5. Why default the export cap to 1 000 000

The export path is byte-for-byte analogous to `analyze-data`:
rows go to a file, not the chat, so the only ceiling that matters
is memory. ADR-0005 §3.3 already analysed this regime and picked
1 000 000 as the `analyze-data` backstop; reusing the same number
keeps the two file-bound paths consistent and means the user's
27 000-row workload (issue #14's implied scale) just works with no
config. Unlike `analyze-data`, `QuerySQLToCSV` streams row-by-row
straight into `encoding/csv` (it never materialises a
`[]map[string]any`), so its per-row memory cost is far lower than
the analyze path's — 1 000 000 rows is comfortable.

## 4. Alternatives considered

- **Just raise the export default to 1 000 000, no config.** Fixes
  the reported pain but ignores the explicit "設定で変更したい"
  (want to change it in settings) ask. Rejected.
- **One shared `max_rows` knob for all paths.** Conflates the
  chat-context concern with the memory concern; a user raising the
  export cap would unknowingly flood their LLM context on the next
  `query-sql`. Rejected — the two caps protect different
  resources.
- **Per-call `max` parameter on the tool (LLM-supplied).** Makes
  the limit the model's problem, which is exactly the "awkward
  instructions / convoluted workarounds" the issue complains
  about. Rejected.
- **Config-file-only, no UI.** Matches `MaxOutputBytes` /
  `MaxToolCallArgsBytes`. Rejected for this case: the user hits
  this interactively and mid-analysis; a GUI knob is the
  appropriate affordance (decided with the reporter).

## 5. Compatibility

- **On-disk config**: additive. Missing `analysis` block →
  defaults. No migration.
- **Public Go API**: `MaxQueryRows` const is renamed to
  `DefaultMaxQueryRows` and `QuerySQLToCSV`'s cap changes from
  10 000 to the resolved export cap (default 1 000 000). Only
  in-repo callers (agent, bindings, tests) are affected; updated
  in the same change.
- **Behaviour visible to user**: `export-sql-to-csv` now succeeds
  on result sets between 10 001 and 1 000 000 rows out of the box.
  `query-sql` / `query-preview` / `quick-summary` are unchanged at
  the default. Both caps are now tunable in Settings → General →
  Data analysis.
- **Backward-compat option** (per org convention): the previous
  10 000 export behaviour is recoverable by setting
  `max_export_rows: 10000` in config or Settings — no behaviour is
  removed, only the default raised.

## 6. Tests

- `config`: `TestAnalysisConfig_Resolved` — zero → defaults;
  explicit values pass through; round-trips through JSON
  marshal/unmarshal.
- `analysis` engine:
  - `TestQuerySQLToCSV_AllowsBeyondQueryCap` — load >10 000 rows;
    assert `QuerySQLToCSV` writes them all (regression pin for the
    mis-shared cap).
  - `TestQuerySQLToCSV_RespectsExportCap` — via `SetRowCaps` with a
    small export cap, assert the row-limit error fires at the
    export cap, not the query cap.
  - `TestQuerySQL_StillCapsAtQueryDefault` — guard the chat cap is
    unchanged at `DefaultMaxQueryRows`.
  - `TestSetRowCaps_ZeroKeepsDefault` — 0 args resolve to the
    package defaults.
- Update existing references to the renamed `MaxQueryRows` const
  (`security_test.go`, `engine_analyze_test.go`).

The export-cap test injects a small cap via `SetRowCaps` rather
than materialising a million rows (same spirit as
`setMaxAnalyzeRowsForTesting`).

## 7. Out of scope

- **Configurable `MaxAnalyzeRows`** — still a pure memory
  backstop; ADR-0005 §7 stance unchanged.
- **Per-table or per-query overrides** — global config is enough
  for the reported need.
- **Streaming the analyze path from DuckDB** — orthogonal, see
  ADR-0005 §7.
