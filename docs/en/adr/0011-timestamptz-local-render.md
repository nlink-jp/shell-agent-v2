# TIMESTAMPTZ rendering — display in local TZ — Design Note

**Status:** Design draft (2026-05-14); pending approval.
**Target:** v0.6.5 (point release on top of v0.6.4)
**Supersedes:** ADR-0010 §2 non-goals (the TIMESTAMPTZ deferral
specifically; the LIST / STRUCT / JSON-as-VARCHAR display polish
remains deferred).
**Reported by:** User — TIMESTAMPTZ values in DuckDB tables flow
through every result path as UTC (`Z` suffix), losing the
wall-clock representation users expect to see.

This note specifies how the `internal/analysis` `renderScalar`
helper handles `TIMESTAMPTZ` columns: convert to the runtime
`time.Local` location and format as RFC 3339 with offset.

---

## 1. Problem

DuckDB's TIMESTAMPTZ type normalises every value to UTC at
storage time. By the time `database/sql` returns the value to
Go code as a `time.Time`, the original timezone information is
gone — the only thing recoverable is the absolute UTC instant.

ADR-0010 deferred this on the assumption that "preservation"
required carrying the source TZ through ingest. After release
the user clarified that the actual pain is the display: an
analyst entering a query in Tokyo who sees `2026-05-14T03:34:56Z`
for a value that originally read `2026-05-14 12:34:56+09:00`
loses 100% of the wall-clock perception, even though the absolute
instant is correct.

Recovering the *original* TZ from a multi-TZ dataset is
impossible at the rendering layer (it would require either a
schema change or a sidecar column). But recovering a *useful*
wall-clock representation is straightforward: convert the UTC
`time.Time` to the runtime's local TZ and format with explicit
offset. For users whose data originates in their own TZ — the
overwhelming majority case for shell-agent-v2's local-first
positioning — this matches the original wall clock exactly.

---

## 2. Goals

1. **TIMESTAMPTZ values render in `time.Local`** in all three
   result paths (Preview / QuerySQL / QuerySQLToCSV), with
   explicit numeric offset (`+09:00` rather than `Z`).
2. **Round-trip the UTC instant unchanged** — converting display
   format does not change the absolute moment.
3. **`TIMESTAMP` (no TZ) is unchanged**. DuckDB intentionally
   distinguishes wall-clock (TIMESTAMP) from instant (TIMESTAMPTZ).
   We respect that.
4. **Test must be TZ-independent**. CI may run in any TZ; the
   assertions must work regardless of the host's `time.Local`.

Non-goals:

- **Configurable display TZ** (e.g., `analysis.display_timezone`
  settings knob). Easy follow-up if requested but not part of
  this change. `time.Local` is the right default and matches Go
  standard-library convention.
- **Source-TZ preservation across multi-TZ datasets**. Genuinely
  impossible without a schema-side change; out of scope.

---

## 3. Design

In `internal/analysis/render.go`, add one dispatch entry to
`renderScalar`:

```go
case dbTypeName == "TIMESTAMPTZ":
    if t, ok := v.(time.Time); ok {
        return t.In(time.Local).Format(time.RFC3339Nano)
    }
```

Behaviour:

- `time.RFC3339Nano` preserves microsecond precision when
  present and trims trailing zeros otherwise (so a plain-second
  value renders as `2026-05-14T12:34:56+09:00`, not
  `...+09:00.000000`).
- The `time.In(time.Local)` conversion is purely a display
  change: the absolute instant is preserved.
- Fall-through (the `if t, ok` guard fails) leaves the value
  alone, so future driver versions returning a string survive.

`TIMESTAMP` (no TZ) and `DATE` continue to take the default
`time.Time` path and marshal via the standard library's
`MarshalJSON` (which is `RFC3339Nano` UTC). Their semantics are
"wall clock" and "calendar day" respectively, neither of which
benefits from local-TZ conversion.

---

## 4. Testing

### 4.1 Pin TIMESTAMPTZ in the type sweep

Extend `engine_typesweep_test.go` `wants` table with a
`timestamptz_col` entry. To make assertions deterministic across
CI hosts:

- At test start, override `time.Local` to a fixed location
  (`Asia/Tokyo`, +09:00). Save and restore around the test.
- Expected value: `2026-05-14T12:34:56+09:00`. The original test
  fixture used `TIMESTAMPTZ '2026-05-14 12:34:56+09'`, so the
  UTC-normalised stored value (`03:34:56Z`) round-trips back to
  the original wall clock when displayed in +09:00.

### 4.2 Pin TIMESTAMP unchanged

Add an assertion entry for `timestamp_col` with the existing
`2026-05-14T12:34:56Z` form. This guards against accidentally
extending the local-TZ logic to TIMESTAMP, which would change
its semantics.

### 4.3 Test TZ override pattern

```go
prevLocal := time.Local
time.Local, _ = time.LoadLocation("Asia/Tokyo")
defer func() { time.Local = prevLocal }()
```

`time.Local` is a package-level variable; mutating it from a
test is safe under `go test`'s default sequential execution
(the analysis package does not run tests in parallel).

---

## 5. Risks

- **CI TZ override race**: if a future test in the analysis
  package runs in parallel with the sweep, the overridden
  `time.Local` could leak. Mitigated: analysis tests are
  sequential by default; we don't add `t.Parallel()`. A defensive
  `defer` restores the original.
- **Microsecond precision loss in `time.Local` conversion**:
  none — `time.In` preserves the underlying nanosecond field.
- **`time.LoadLocation` failure**: requires the system tzdata.
  macOS and Linux ship it; Windows ships a Go-internal copy.
  Failure path: the test would `t.Fatalf` cleanly. Production
  code path uses `time.Local` directly (no Load) so this is a
  test-only concern.

---

## 6. Rejected alternatives

### 6.1 Use DuckDB session `SET TimeZone`

Set DuckDB's session TZ and rely on it producing TZ-aware string
output. **Rejected**: through `database/sql` go-duckdb returns
`time.Time` regardless of the session setting, so the session
TZ does not propagate to our path.

### 6.2 Cast to VARCHAR server-side

`SELECT col::VARCHAR` to let DuckDB format the timestamp. **Rejected**:
collapses every column type to text (we lose Go `time.Time` for
the React side that uses the type info), and can't be retrofitted
into arbitrary user `SELECT *` queries.

### 6.3 Add a configurable display TZ now

`analysis.display_timezone` config knob. **Rejected for v0.6.5**
(still on the table as v0.7.x follow-up if requested): adds UI,
config schema, and migration surface for a feature whose default
already covers the overwhelming majority of cases.

### 6.4 Preserve source TZ as a sidecar column

Capture the offset string from the JSON source and store it as
`<col>_tz` next to the value. **Rejected**: requires LoadJSON /
LoadJSONL to pre-parse the JSON in Go (cannot be done via
`read_json_auto`), changes table schema in a way users would
need to learn, and only addresses the multi-TZ-dataset case
which is rare in this product's positioning.

---

## 7. Compatibility & rollout

- **Persistence format**: unchanged.
- **LLM-observable**: tool-result JSON for TIMESTAMPTZ columns
  changes from `2026-05-14T03:34:56Z` to `2026-05-14T12:34:56+09:00`
  (or whatever the host's local TZ produces). The absolute
  instant is identical. LLM behaviour on existing pinned-context
  sessions may shift slightly since the wall-clock string is
  different.
- **CSV-observable**: same change. Downstream parsers using
  RFC 3339 handle both `Z` and numeric offsets natively. Naive
  string-prefix matching ("starts with `2026-05-14`") still works
  if the source TZ matches the display TZ.
- **UI-observable**: Data-panel summaries show local-TZ wall
  clocks. Strict improvement for users whose data originates in
  their own TZ.
- **Rollout**: ships as v0.6.5. CHANGELOG calls out the format
  change explicitly with before / after example.

---

## 8. References

- ADR-0010 §2 (the deferral this supersedes).
- Reported by user: 2026-05-14, "TZ 保持については優先的に対応
  する必要があると考えられる" (TZ retention should be addressed
  with priority).
- DuckDB TIMESTAMPTZ semantics:
  https://duckdb.org/docs/sql/data_types/timestamp
- Go `time.Local`:
  https://pkg.go.dev/time#Local
- Affected sites (no new ones beyond ADR-0010):
  - `app/internal/analysis/render.go` (renderScalar dispatch)
  - `app/internal/analysis/engine_typesweep_test.go` (sweep
    assertion + TZ override)
