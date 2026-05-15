# Saved-query derived tables — filtered subsets for analyze-data — Design Note

**Status:** Implemented in v0.8.0 (2026-05-15).
**Target:** v0.8.0 (minor bump on top of v0.7.0 — one new
analysis tool, no engine API changes, no breaking changes).
**Reported by:** User — `analyze-data` currently runs over the
whole table (`SELECT * FROM "<table>"` in `toolAnalyzeData`,
`tools.go:539`). For a 27k-row log table the user often wants to
sliding-window-analyse only a slice (last 24 h, errors only,
one customer's events). Today the only workarounds are
(a) preprocess the data file outside the agent before `load-data`,
or (b) write a `quick-summary` SQL which is one-shot rather than
sliding-window.

This note specifies a single new tool, `save-query`, that
materialises the result of a SELECT statement as a new
DuckDB **base table** which `analyze-data` can then target
via its existing `table` parameter — no new tool category,
no engine schema changes, no new export/import code, no
behaviour change for the unfiltered case.

---

## 1. Problem

`analyze-data` runs deep sliding-window analysis on a loaded
table. Its current contract:

```go
// tools.go:539
query := fmt.Sprintf("SELECT * FROM \"%s\"", tableName)
results, err := a.analysis.QuerySQLForAnalyze(query)
```

The `table` parameter is the only knob. There is no way to ask
the agent for "the same sliding-window analysis but only on the
rows where `status = 'failed'` and `ts >= '2026-05-01'`".

The neighbouring tools have asymmetric coverage:

- `query-sql` — execute caller-supplied SELECT, return rows. Filter
  works, but the output is **raw rows in the chat**, not a
  sliding-window analysis. Useful for spot-checks; not for the
  audit-style "find anomalies" use case.
- `quick-summary` — execute caller-supplied SELECT and run a
  **one-shot** LLM summary. Filter works, but one-shot is too
  shallow for large filtered subsets (still loses the running-
  summary cross-window context that `analyze-data` builds up).
- `query-preview` — natural language → SQL → rows. Same
  one-shot limitation as quick-summary on the analysis side.
- `analyze-data` — the only sliding-window analyser, but
  whole-table only.

There is no way to combine "I want a filter" with "I want deep
sliding-window analysis".

---

## 2. Goals

1. **Filter then deep-analyse**: a user (or the LLM acting on the
   user's behalf) can define a filter, then run `analyze-data`
   over the filtered subset with all existing semantics intact
   (sliding-window, 3-tier dedup, Findings auto-promotion,
   running summary, `MaxAnalyzeRows` backstop).
2. **Reuse the existing table abstraction**: the filtered
   subset is just another row in the engine's `tables` map.
   `analyze-data`, `list-tables`, `describe-data`, `query-sql`,
   `quick-summary`, `query-preview` all work without
   modification because they only see "a table named X".
3. **Zero changes to engine internals**: no new metadata field,
   no `information_schema` migration, no view-vs-table
   conditionals. The existing `refreshTableMeta(name)` path is
   reused as-is.
4. **Zero changes to export / import**: the bundle already
   includes `analysis.duckdb` verbatim (`sessionio/export.go:38`).
   A derived table travels in that file like any other.

Non-goals:

- **Iteration via in-place overwrite.** `save-query` errors on
  name collision rather than silently overwriting. See §3.1.
- **Tool-level drop / cleanup.** No `drop-table` tool. Stale
  derived tables persist until `reset-analysis`. See §6.3.
- **DuckDB views.** Considered and rejected (§6.1). Views avoid
  row duplication but add a parallel metadata category, an
  external-reader-validator concern, and a downgrade
  half-translucent-catalog problem. Materialised tables are
  simpler in every dimension that matters to a single-user
  local tool.
- **WHERE-clause-only `analyze-data` parameter.** Rejected
  (§6.2): strictly weaker than full SELECT, and forces every
  caller to learn yet another parameter.
- **`sql` parameter on `analyze-data` directly.** Rejected
  (§6.4): clobbers `list-tables` discoverability of the filter,
  prevents re-use across multiple `analyze-data` invocations.

---

## 3. Design

### 3.1 Tool surface

One new analysis-engine tool, data-gated
(`HideUntilDataLoaded: true` — meaningless without a base table):

```yaml
name: save-query
parameters:
  sql:          required string  # full SELECT statement
  name:         required string  # new table identifier (alphanumeric + underscore)
  description:  optional string  # human-readable purpose
mitl: sql_preview                 # shows SQL in approval dialog
category: write
```

Behaviour:

1. Validate `name` matches `^[A-Za-z_][A-Za-z0-9_]*$`.
2. Validate `sql` is read-only via the existing `isReadOnlySQL`
   guard (rejects INSERT / UPDATE / DELETE / DROP / CREATE / ALTER
   / LOAD / INSTALL / PRAGMA prefixes).
3. Check `e.tables` (the engine's in-memory map) for a name
   collision:
   - If the name exists: reject with an error message that
     includes three suggested alternatives: `<name>_v2`,
     `<name>_filtered`, `<name>_derived`. The LLM picks one and
     re-issues. Explicit error rather than silent overwrite —
     a base table loaded via `load-data` should not be replaced
     by an accidental `save-query` collision.
   - If the name is free: proceed.
4. Execute `CREATE TABLE "<name>" AS <sql>`. The table name is
   sanitised via `sanitizeIdentifier`. The body SQL is *not*
   wrapped — the read-only validation above is the guard, and
   any JOIN or projection the LLM wrote stays intact.
5. Call `refreshTableMeta(name)` to register the new table in
   the engine's `tables` map. This is the same code path
   `load-data` already uses; no new method needed.
6. If `description` was provided, run the existing
   `SetTableDescription(name, description)`.
7. Return a markdown-formatted summary: table name, row count,
   column list, source SQL preview (first 200 chars),
   description if any.

Error cases:

- Name fails the identifier regex → "table name must be alphanumeric and underscores only; got: `<name>`".
- SQL is not read-only → "query must be a SELECT statement; INSERT/UPDATE/DELETE/DROP/etc. are rejected".
- Name collides with an existing table → "name `<name>` is already in use; try `<name>_v2`, `<name>_filtered`, or `<name>_derived` instead".
- DuckDB rejects the SQL (syntax, unknown column) → pass through the DuckDB error verbatim, prefixed `save query: `.
- Row count exceeds `MaxAnalyzeRows` (1,000,000) → include a warning in the success output: "this table has N rows; analyze-data caps at 1,000,000 and will refuse — narrow the filter further before running analyze-data on it." (We don't refuse the CREATE; the user might want it for query-sql exploration first.)

### 3.2 Engine

No changes. Step 4 and step 5 above use existing methods. The
`Engine.tables` map gains an entry exactly the same way a
`load-data` call does — same `refreshTableMeta` path, same
`information_schema.columns` + `SELECT COUNT(*)` lookups, same
`duckdb_tables().comment` description retrieval.

`Engine.Reset()` already drops every table in `e.tables` via the
existing loop; derived tables fall into that loop unchanged.

### 3.3 Tool descriptor updates

`internal/agent/tool_descriptors_analysis.go`:

- Add one new descriptor for `save-query` (data-gated,
  `Source: "analysis"`, `Category: "write"`,
  `MITLDefault: true`, `MITLCategoryOverride: "sql_preview"`).
- Edit `analyze-data.Description` to add one sentence: "For
  filtered analysis, use `save-query` first to materialise a
  SELECT result as a derived table, then pass that table's name
  here." (Tells the LLM the chain explicitly so the workflow is
  discoverable from a single descriptor read.)
- No edits to `list-tables` or `describe-data` descriptions —
  derived tables look identical to loaded ones at the metadata
  level.

`internal/agent/tools.go`:

- New handler `toolSaveQuery`. Follows the established pattern:
  unmarshal args, call into the engine, format result.
- No changes to `formatTableMeta` — derived tables render
  identically to base tables.

The descriptor registry's structural test (added in v0.6.0)
automatically catches misses in the parallel lists; adding one
descriptor to the list fully populates every consumer surface.

### 3.4 Frontend

No frontend changes. `save-query` surfaces in chat exactly like
every other analysis tool: through the tool bubble, the MITL
dialog (sql_preview variant inherited from `query-sql`), and the
Settings → Tools tab driven by the ToolDescriptor registry.

### 3.5 Sliding-window mechanics on a derived table

`analyze-data` calls `QuerySQLForAnalyze("SELECT * FROM \"<table>\"")`
unchanged. A derived table is a base table to DuckDB; the call
path is byte-identical to the existing one. Every existing test
for analyze-data (sliding-window cross-context, 3-tier dedup,
running-summary cache) holds without modification.

### 3.6 Configuration

No new config keys. The 1,000,000 row backstop
(`MaxAnalyzeRows`) applies to derived tables identically.

### 3.7 Memory model interaction

Findings auto-promoted from `analyze-data` are tagged with the
target table name. For a derived table the tag is the
`save-query`-supplied name, which is meaningful (the user can
later trace a finding back to the filter via `describe-data`
on that table). No memory-model change needed.

---

## 4. Testing

### 4.1 Tool-layer tests (`tools_save_query_test.go`)

- `toolSaveQuery` happy path: JSON args round-trip, output
  contains the new table's row count and column list, the
  table appears in `list-tables`.
- Name collision: existing table with the same name → error
  containing the literal suggestion list.
- Invalid identifier (`123`, `foo bar`, `drop table`): error
  cites the regex requirement.
- Non-SELECT body (`INSERT ...`, `DROP TABLE ...`): rejected
  by `isReadOnlySQL`, error explains read-only requirement.
- Valid SELECT, DuckDB rejects (unknown column): DuckDB error
  passes through with the `save query: ` prefix.
- Description plumbing: when `description` is supplied,
  `describe-data` on the new table includes it.

### 4.2 Integration test (extend existing tools_test.go)

- End-to-end: load CSV → save-query with a WHERE filter →
  analyze-data on the derived table name → assert the LLM
  adapter sees the filtered row count (mock backend records
  args), not the full count.

### 4.3 Export / Import round-trip

The bundle already includes `analysis.duckdb` verbatim
(`sessionio/export.go:38`). A derived table is a row in
DuckDB's `main` schema and travels exactly like a loaded one.

One regression test, co-located with existing export/import
tests: load CSV → save-query → ExportSession → ImportSession →
the derived table appears in `list-tables` on the imported
session with correct row count and columns. No bundle-format
changes; no version bump; no fixture-pinning needed.

### 4.4 Structural test

The existing `tool_descriptor_structural_test.go` asserts that
every name in `analysisDescriptors()` is reachable from every
consumer (analysisTools builder, MITL category map, dispatcher,
ListTools view). Adding `save-query` to the descriptor list
automatically exercises every parallel path. No new structural
test needed.

### 4.5 Doc-mirror check

`scripts/docs-mirror-check.sh` enforces en/ja parity. ADR-0013
EN/JA mirror plus an INDEX entry in both languages.

---

## 5. Risks

- **Disk growth from accumulated derived tables.** A user who
  runs many `save-query` calls without `reset-analysis` will
  accumulate tables in `analysis.duckdb`. Mitigation: derived
  tables are small (the user is filtering down, not up), the
  DuckDB file is per-session, and `reset-analysis` clears
  everything. No per-table drop tool because the marginal
  utility doesn't justify a second new tool in this release;
  if a user reports running out of disk we revisit (§6.3).
- **Name collision suggestion exhaustion.** A user with both
  `foo`, `foo_v2`, `foo_filtered`, and `foo_derived` already
  defined would hit the suggestion list but every suggestion
  already exists. The error message includes the literal three
  names; if all three are taken the user picks a fourth. We
  don't iteratively generate `foo_v3`, `foo_v4`, …; the LLM
  picks. The descriptor hints that the suggestions are
  illustrative, not exhaustive.
- **Materialisation cost on huge filters.** A 50M-row JOIN
  against a small lookup table that returns 10M rows would
  CTAS those 10M rows. DuckDB handles this transactionally and
  fast, but it does take memory and time. Mitigation:
  `MaxAnalyzeRows` caps at 1M downstream; if the user wants
  even a 10M-row exploration they can use `query-sql` first to
  test the filter shape before materialising. No engine guard
  at `save-query` time; the cost is user-intentional.
- **Accidental overwrite of a loaded table.** Section 3.1 makes
  this a hard error rather than silent OR REPLACE — the LLM
  must pick a fresh name. This is the deliberate trade-off
  against ergonomic iteration; the safety wins.

---

## 6. Rejected alternatives

### 6.1 DuckDB VIEW instead of materialised table

Use `CREATE VIEW <name> AS SELECT ...` so the filter is a lazy
SQL definition rather than a row copy. **Rejected**: views look
attractive because they avoid row duplication, but they add a
disproportionate amount of complexity:

- New metadata category (`IsView`, `ViewSQL` on `TableMeta`).
- `rebuildTableMeta` must move from `SHOW TABLES` to
  `information_schema.tables` to discover views.
- `refreshTableMeta` branches on `BASE TABLE` vs `VIEW`.
- View bodies can reference external readers (`read_csv_auto`
  with host file paths); these break silently after
  export/import. A validator is needed.
- Downgrade puts the DuckDB catalog in a half-translucent
  state: views are physically present but the older engine's
  `SHOW TABLES` doesn't list them. Either a SchemaVersion bump
  is needed, or the half-translucent state is accepted as a
  doc footnote.
- `Reset()` must drop views before tables to avoid CASCADE
  edge cases.

The row-duplication cost the view design was meant to avoid is
*not real for the use case*: filtering reduces row count by
definition, DuckDB is per-session local, and disk is cheap.
The complexity tax pays for an imagined benefit. CTAS is
strictly simpler in every dimension that affects this codebase.

### 6.2 `where` parameter on `analyze-data`

Add `analyze-data(prompt, table, where?)`. **Rejected**: WHERE
alone is strictly weaker than full SELECT. The user's real
filters include JOIN against lookup tables and derived columns;
a `where` parameter handles trivia and forces fallback for
anything else, ending in two surfaces. `save-query` collapses
both cases into one mechanism.

### 6.3 Add a `drop-table` tool in the same release

Symmetric pair: `save-query` + `drop-table` so the LLM can
clean up individual derived tables. **Rejected for v0.8.0**:
adds a second new tool whose only purpose is housekeeping in a
per-session DB that already has `reset-analysis`. Disk pressure
is hypothetical; ship the core feature first, add cleanup if a
user actually reports the symptom.

### 6.4 `sql` parameter on `analyze-data` directly

`analyze-data(prompt, sql?)` where `sql` is an alternative to
`table`. **Rejected**: clobbers `list-tables` discoverability
(the filter is invisible to the user), prevents reuse across
multiple `analyze-data` invocations (each call re-supplies the
SQL), and complicates the descriptor with a "two of these but
not both" rule. `save-query` then `analyze-data` is two clean
steps with a discoverable intermediate.

### 6.5 Don't add a tool, just educate the LLM

Document in `analyze-data.Description` that the user can
pre-filter outside the agent before `load-data`. **Rejected**:
this is the workaround we already have. It doesn't solve the
problem of "filter using data already in the agent" which is
where the bulk of the friction lives.

### 6.6 Silent CREATE OR REPLACE TABLE

`save-query` overwrites on collision. **Rejected** in favour of
error-with-suggestion (§3.1). Silent overwrite of a loaded
table by an accidental derived-table collision is a real risk;
the safety margin is worth the small ergonomic cost of suffix
iteration.

---

## 7. Compatibility & rollout

- **Persistence format**: no changes. `analysis.duckdb` already
  stores all tables identically; a derived table is a row in
  `main` schema like any loaded one.
- **Bundle format / SchemaVersion**: no changes. v0.7.0 bundles
  and v0.8.0 bundles are byte-compatible because derived tables
  are indistinguishable from loaded ones at the bundle level.
  Forward, round-trip, and backward compatibility all hold
  trivially.
- **Config schema**: no changes.
- **LLM-observable**: one new tool in the listing.
  `analyze-data.Description` gains a one-sentence pointer.
  Existing analyses produce byte-identical results.
- **UI-observable**: one new tool in Settings → Tools. MITL
  dialog uses the existing `sql_preview` rendering.
- **Import / Export**: trivially. Derived tables are tables.
- **Rollout**: ships as v0.8.0. CHANGELOG, README, README.ja
  gain a `save-query` entry with one usage example. Reference
  doc at `docs/{en,ja}/reference/data-analysis.md` gains a
  "filtered analysis" subsection.

---

## 8. References

- Reported by user: 2026-05-15, "analyze-data はテーブル全体しか
  解析できないので、絞り込みできると効率が上がる". Design discussion
  converged on save-query CTAS after considering view, where-param,
  inline-sql, and educate-only variants.
- `feedback_full_restructure_over_patch` — when a tool grows
  beyond its original shape, prefer a new tool over a parameter
  bolt-on. Applied here: rejected §6.2, §6.4 in favour of a
  new tool.
- `feedback_doc_completeness` — release-ready docs at scaffold
  time; ADR + reference doc + README sections + CHANGELOG entry
  land in the same release.
- v0.6.0 ToolDescriptor registry (`docs/en/adr/0007`): one new
  descriptor fully populates every consumer surface via the
  single registry source of truth. No parallel-list updates
  needed.
- v0.4.4 analyze-data row-cap (`docs/en/adr/0005`):
  `MaxAnalyzeRows` governs `QuerySQLForAnalyze` and applies to
  derived tables identically. Filters that exceed the cap are
  the user's responsibility to narrow.
- Affected sites:
  - `app/internal/agent/tool_descriptors_analysis.go` — one new
    descriptor, one description tweak on `analyze-data`.
  - `app/internal/agent/tools.go` — `toolSaveQuery`.
  - `app/internal/agent/tools_save_query_test.go` — new test
    file per §4.1.
  - `app/internal/agent/tools_test.go` (or peer) — extend with
    §4.2 integration case and §4.3 export/import round-trip.
  - `docs/{en,ja}/INDEX.md` — add ADR-0013 entry.
  - `docs/{en,ja}/reference/data-analysis.md` — new "Filtered
    analysis via save-query" subsection.
  - `README.md`, `README.ja.md` — add `save-query` to the
    analysis-tool list.
  - `CHANGELOG.md` — v0.8.0 entry.
