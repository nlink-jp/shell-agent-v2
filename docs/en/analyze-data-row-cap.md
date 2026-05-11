# analyze-data row cap (v0.4.4)

## 1. Symptom

A user loaded a 27,000-row table and ran `analyze-data` with
the intent of letting the sliding-window summarizer chunk the
data. The tool failed up front with:

```
query result exceeds 10000 rows; refine query
(e.g. add LIMIT or WHERE)
```

This is the exact opposite of what the sliding-window feature
is meant to do — `analyze-data` exists *because* tables can be
larger than a single LLM context, and the summarizer is
designed to walk over them in chunks.

## 2. Root cause

`analyze-data` fetches all rows from the target table through
the same code path that interactive `query-sql` uses:

```go
// app/internal/agent/tools.go:807-808 (toolAnalyzeData)
query := fmt.Sprintf("SELECT * FROM %q", tableName)
results, err := a.analysis.QuerySQL(query)
```

`Engine.QuerySQL` is hard-capped at `MaxQueryRows = 10000`:

```go
// app/internal/analysis/engine.go:295,479-482
const MaxQueryRows = 10000
...
if rowCount >= MaxQueryRows {
    return nil, fmt.Errorf("query result exceeds %d rows; refine query (e.g. add LIMIT or WHERE)", MaxQueryRows)
}
```

The 10,000-row cap is **correct** for the three interactive
chat-output callers (`query-sql`, `query-preview`, `quick-summary`):
their results are JSON-serialised back into the LLM's tool
result, and an unbounded SELECT could trivially exhaust memory
or blow past the model's context window. The cap is **wrong**
for `analyze-data`, where the rows never enter the chat — they
are chunked into 100-row windows and consumed by the
summarizer's per-window LLM calls.

The sliding-window feature is therefore unreachable for any
table larger than 10,000 rows, which is the only regime where
the feature is interesting.

## 3. Fix

Split the cap into two named constants and add a dedicated
`Engine` method for the analyze path:

```go
// app/internal/analysis/engine.go
const MaxQueryRows    = 10_000     // unchanged: interactive chat output
const MaxAnalyzeRows  = 1_000_000  // new: sliding-window analyze backstop

func (e *Engine) QuerySQLForAnalyze(query string) ([]map[string]any, error) {
    return e.querySQLBounded(query, MaxAnalyzeRows, /*hint*/ analyzeRefineHint)
}
```

The existing `QuerySQL` is unchanged in behaviour. Internally,
both share a `querySQLBounded(query, max, hintWhenExceeded)`
helper so the read-only check, statement preparation, scanning
loop, and value coercion stay in one place — but this is a
refactor for code hygiene, not a behaviour change. If the
refactor causes too much noise relative to the fix, the
implementation may inline the loop in `QuerySQLForAnalyze` and
leave the existing function alone.

`tools.go::toolAnalyzeData` switches its single call site to
the new method:

```go
// app/internal/agent/tools.go:808
results, err := a.analysis.QuerySQLForAnalyze(query)
```

The other three callers (`toolQuerySQL`, `toolQueryPreview`,
`toolQuickSummary`) keep using `QuerySQL` and the 10,000-row
cap — those are correct as they stand.

### 3.1. Why a dedicated method, not a parameter on QuerySQL

A signature change like `QuerySQL(query string, max int)` would
force every existing caller to choose a number, when "10,000
because chat output" is the right answer in three out of four
cases. A named method keeps the intent visible at the call site:
`QuerySQLForAnalyze(...)` reads as *"fetch rows for
sliding-window analysis, where memory is the only ceiling that
matters"*.

### 3.2. Why a high number, not unbounded

Removing the cap entirely would be wrong: a `SELECT * FROM`
against a billion-row table would crash the agent process with
OOM and take any in-flight session state with it. The cap
exists to be a **memory-safety backstop**, not a query-shape
suggestion. The error message reflects that distinction (see
§3.3).

### 3.3. Picking the value

Memory cost per row, when materialised through the existing
`[]map[string]any` representation, is roughly:

| Shape | Per-row cost | 27 k rows | 100 k rows | 1 M rows |
|-------|--------------|-----------|------------|----------|
| Narrow (10 cols, ~30 B avg) | ~700 B raw + Go map overhead ≈ 1.4 KB | 38 MB | 140 MB | 1.4 GB |
| Wide (50 cols, ~30 B avg)   | ~3.2 KB                                | 86 MB | 320 MB | 3.2 GB |
| Wide+long values (50 cols, ~200 B avg) | ~13 KB                       | 350 MB | 1.3 GB | 13 GB |

The summarizer then converts to `[]string` (JSON encoding)
which roughly doubles peak working-set during the conversion.
On a typical 16-32 GB Mac, the ceiling we can actually afford
without paging is somewhere around 1-2 GB working set for the
analyze step.

The practical ceiling imposed by **LLM call latency** is much
lower than the memory ceiling. With the default
`SummarizerConfig` (100 rows/window, 10% overlap → step 90):

| Rows | Windows | LLM time @ 5 s/call | LLM time @ 15 s/call |
|------|---------|---------------------|----------------------|
| 27 k  | ~300    | 25 min              | 75 min               |
| 100 k | ~1,100  | 90 min              | 4.5 h                |
| 500 k | ~5,500  | 7.5 h               | 23 h                 |
| 1 M   | ~11,000 | 15 h                | 46 h                 |

**Proposal: `MaxAnalyzeRows = 1_000_000`.**

Rationale: the cap should be a real memory backstop, not a
shadow rate-limit on LLM calls. The user's wall-clock budget
already disqualifies anything past ~100 k rows in practice; the
cap exists for the case where someone accidentally points
analyze-data at a billion-row table. 1 M rows worst-case
(~13 GB on the wide+long shape) will OOM a 16 GB Mac, but a
narrow 1 M row case (~1.4 GB) is fine, and we want to give
narrow-table users the room without prompting for a config knob.

If user testing finds 1 M is too aggressive (sustained crashes
on actual workloads), drop to 500,000 in v0.4.5. The constant
is private to the package; lowering it later is non-breaking.

### 3.4. Error message

When the cap is exceeded, `QuerySQLForAnalyze` returns:

```
table is too large for analyze-data (>1000000 rows);
pre-aggregate via query-sql first (GROUP BY, sample, or
date-range filter) before re-running analyze-data
```

The wording deliberately differs from `QuerySQL`'s
"refine query (e.g. add LIMIT or WHERE)" — adding `LIMIT` to
analyze-data would defeat the sliding-window's whole purpose
(you'd analyse the first 10,000 rows and pretend they
represent the table). Pre-aggregation via `query-sql` to
materialise a smaller derived table is the correct workaround,
and the error says so.

## 4. Tests

### 4.1. Engine tests (`internal/analysis/engine_test.go` or new `engine_analyze_test.go`)

- **`TestQuerySQLForAnalyze_AllowsBeyond10k`** — load 12,000
  rows; assert `QuerySQLForAnalyze("SELECT * FROM t")` returns
  exactly 12,000 rows with no error. Pins the regression: the
  pre-fix code path errors out at 10,001.
- **`TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows`** — load
  `MaxAnalyzeRows + 100` rows (might be expensive; alternatively
  shrink the constant temporarily via build tag, or use a
  helper that injects a smaller cap for the test only). Assert
  the specific error string about pre-aggregation.
- **`TestQuerySQLForAnalyze_RejectsWrite`** — verify the
  read-only enforcement is still in place (no write SQL allowed
  even on the analyze path).
- **`TestQuerySQL_StillCapsAt10k`** — explicit guard that the
  interactive cap is unchanged. Existing
  `TestQuerySQLRowLimit` may already cover this; re-check that
  `MaxQueryRows == 10000` and update if the constant grew an
  alias.

### 4.2. Agent test (`internal/agent/tools_test.go` or new file)

Stretch goal: add a smoke test that exercises `toolAnalyzeData`
end-to-end with a mock LLM and a 12 k-row table, asserting it
completes without the SQL row-limit error and produces a report
covering the expected number of windows. If the existing test
harness for `analyze-data` is heavy or absent, defer to the
Engine-level coverage above; the wiring is a single line and is
exercised by the manual smoke pass in §6.

### 4.3. Memory budget

Loading 1 M rows into Go in a unit test would be slow and
memory-heavy enough to flake CI. The `TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows`
test should use one of:

1. A `t.Helper()` that exposes a smaller cap via an unexported
   `setMaxAnalyzeRowsForTesting(t, 100)` (test-only seam,
   reverted on `t.Cleanup`).
2. A build-tag-gated test that skips outside `-tags=slowtest`.

Option 1 is preferred — keeps the test fast and deterministic
without growing the build matrix.

## 5. Compatibility

- **Public API**: adds one new method (`QuerySQLForAnalyze`)
  and one new exported constant (`MaxAnalyzeRows`). No removal
  or signature change. Downstream consumers (only `bindings.go`
  and the agent package, both in this repo) need no changes
  except the one call site in `toolAnalyzeData`.
- **On-disk format**: unchanged. No DuckDB schema, manifest, or
  config field is touched.
- **Behaviour change visible to user**: `analyze-data` now
  succeeds on tables >10,000 rows (subject to the new
  1,000,000-row backstop). All other tools behave identically.
  The new error message is only reachable when
  `MaxAnalyzeRows` is hit, which is impractical except by
  deliberate stress.
- **Settings UI**: no new knobs in v0.4.4. If field reports
  prove the fixed value insufficient, a `Settings → Tools →
  analyze-data max rows` knob is the obvious follow-up; the
  Engine method's signature is already shaped to accept a
  per-call max if we later want to plumb config through.

## 6. Manual smoke

1. Load a 27,000-row CSV via `load-data`.
2. Run `analyze-data` with a meaningful perspective (e.g.,
   "find spikes in error rates by day").
3. Confirm no `query result exceeds 10000 rows` error.
4. Confirm the per-window progress bubble (`window 1/300`,
   `window 2/300`, ...) updates in place — that path was
   wired in v0.4.1 and must not regress.
5. Confirm the final report and any auto-promoted findings
   appear in the chat pane and Findings panel respectively.
6. Cancel mid-run (close session / abort) and verify no
   leaked goroutines (existing agent state-machine
   `postTasksWg.Wait` covers this; smoke just needs visual
   confirmation that the bubble lands in `error` /
   `cancelled` state).

For the upper-bound error path, a synthetic test is
sufficient — generating 1,000,001 rows by hand for the smoke
pass is overkill.

## 7. Out of scope

- **Configurable `MaxAnalyzeRows`** — wait for evidence the
  fixed value is wrong before adding a knob.
- **Streaming the rows from DuckDB straight into the
  summarizer** — would let us scale arbitrarily, but the
  summarizer takes `[]string` and would need refactoring to
  consume an iterator. The expected gain (avoid materialising
  the full slice) only matters past ~500 k rows, which the
  LLM-time table in §3.3 already rules out as practical.
- **Auto-sampling for huge tables** — "if N > 100 k, randomly
  sample 100 k" is a separate UX feature, not a fix. It would
  also surprise users who think analyze-data scanned every row.
- **Per-row token cost estimation** — the summarizer trusts
  the configured window size; budgeting at the per-row level
  is its own design problem (see token-budget feedback memory).
- **Lifting the 10,000-row chat-output cap** for `query-sql` /
  `query-preview` / `quick-summary` — those caps are correct.

## 8. Open questions for review

1. **`MaxAnalyzeRows` value** — is 1,000,000 the right ceiling,
   or should it start at 500,000? See §3.3 for the trade-off.
2. **Error message wording** — does the suggested "pre-aggregate
   via query-sql first" text adequately point users at the
   workaround, or should it name a specific helper / link to a
   docs section?
3. **Test seam vs build tag** — §4.3 prefers an unexported
   `setMaxAnalyzeRowsForTesting` helper. Acceptable, or prefer
   a `slowtest` build tag for purity?
4. **Refactor scope** — extract a shared `querySQLBounded`
   helper, or inline the loop in `QuerySQLForAnalyze`? The
   first is cleaner; the second is a smaller diff for a
   bug-fix release.
