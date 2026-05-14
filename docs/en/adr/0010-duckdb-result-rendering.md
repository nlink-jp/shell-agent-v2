# DuckDB result rendering — type-dispatched scalar conversion — Design Note

**Status:** Design draft (2026-05-14); pending approval.
**Target:** v0.6.4 (point release on top of v0.6.3)
**Reported by:** User — load-data on a 17 MB JSON file produced a
table whose GUID column showed binary garbage in the Data panel.

This note specifies how the `internal/analysis` package converts
DuckDB scalar values to Go values that survive the three result
paths (Data-panel preview, LLM tool-result JSON, CSV export). The
existing code does a blind `[]byte → string` conversion that
corrupts every DuckDB type whose `database/sql` representation is
binary or a non-stringifiable struct.

A discovery sweep (one synthesized row per supported DuckDB type,
read back through all three paths) widened the bug class from the
single reported symptom (UUID) to **six data-correctness bugs and
several display-quality issues**. This ADR addresses Phase 1 (the
six correctness bugs); Phase 2 (display polish) is deferred and
will be its own ADR.

---

## 1. Problem

A user reported that loading a 17 MB JSON array via `load-data`
produced a table whose `guid` column displayed unprintable
characters in the Data-panel summary. Initial hypotheses (large-
row truncation, parallel JSON reader misalignment) were ruled out
by reproducing the load against the DuckDB CLI directly — every
combination of `read_json_auto` parameters produced clean data.
The corruption was specific to shell-agent-v2's read path.

`engine.go:449-456` (the Preview path) and `engine.go:374-388`
(the CSV path) both contain this conversion:

```go
if b, ok := v.([]byte); ok {
    values[i] = string(b)
}
```

The intent was to render `VARCHAR` columns (which `database/sql`
returns as `[]byte`) as readable text instead of base64. But the
predicate `v.([]byte)` matches **every** column whose go-duckdb
representation is bytes — not just VARCHAR. DuckDB's `read_json_auto`
infers a UUID type for any column whose values match the canonical
8-4-4-4-12 pattern, and go-duckdb v1.8.5 returns UUID columns as
**16 raw bytes** (the binary form, not the canonical string form).
`string(b)` then wraps those 16 bytes into a Go string, producing
the garbage the user saw.

The same blind cast is the root cause of a wider class of
mis-rendered types. The discovery sweep
(`engine_typesweep_test.go`) loads one synthesized row containing
every plausible DuckDB type and dumps the per-column rendering of
all three paths plus `rows.ColumnTypes()` metadata. The matrix
below summarises the result.

### 1.1 Discovery sweep result matrix

| DuckDB type     | DatabaseTypeName    | Preview UI                              | QuerySQL → JSON (LLM)                   | CSV export                  | Verdict     |
|-----------------|---------------------|-----------------------------------------|------------------------------------------|------------------------------|-------------|
| VARCHAR / INTEGER / BIGINT / UBIGINT / HUGEINT / DOUBLE / BOOLEAN | (each native) | OK                                      | OK                                       | OK                           | OK          |
| DATE            | DATE                | Go `time.Time` literal                  | RFC3339                                  | RFC3339                      | OK          |
| TIME            | TIME                | `0001-01-01T12:34:56Z` (epoch year)     | `0001-01-01T12:34:56Z`                   | same                         | **Bug**     |
| TIMESTAMP       | TIMESTAMP           | Go `time.Time` literal                  | RFC3339                                  | RFC3339                      | OK (Phase 2)|
| TIMESTAMPTZ     | TIMESTAMPTZ         | UTC-normalised, original TZ lost        | UTC-normalised                           | UTC-normalised               | Phase 2     |
| DECIMAL         | `DECIMAL(p,s)`      | `{10 3 123456}` (struct fields)         | `{"Width":10,"Scale":3,"Value":123456}` | `{10 3 123456}`              | **Bug**     |
| INTERVAL        | INTERVAL            | `{0 14 0}` (struct fields)              | `{"days":0,"months":14,"micros":0}`     | `{0 14 0}`                   | **Bug**     |
| UUID            | UUID                | 16 raw bytes wrapped as string          | base64 of binary form                    | 16 raw bytes                 | **Bug**     |
| BLOB            | BLOB                | raw bytes wrapped                       | base64 (JSON spec)                       | raw bytes                    | **Bug**     |
| MAP(K,V)        | `MAP(...)`          | `map[k1:v1 ...]` (Go fmt)               | **empty string** (whole-row marshal fails) | `map[...]`                | **Bug**     |
| LIST `T[]`      | `T[]`               | `[1 2 3]` (Go fmt)                      | `[1,2,3]`                                | `[1 2 3]`                    | Phase 2     |
| STRUCT          | `STRUCT(...)`       | `map[a:1 b:x]` (Go fmt)                 | `{"a":1,"b":"x"}`                        | `map[a:1 b:x]`               | Phase 2     |
| JSON (DuckDB)   | VARCHAR (sic)       | `map[x:1]` (Go fmt — already parsed)    | `{"x":1}`                                | `map[x:1]`                   | Phase 2     |

Six types in the **Bug** rows produce data-incorrect output that
either misleads the LLM (UUID/BLOB/DECIMAL/INTERVAL/MAP), shows
table-meaningless values to the user (TIME's epoch year), or
silently drops whole rows from the Wails event payload (MAP via
the row-level `json.Marshal` failure). These are Phase 1.

The rows tagged **Phase 2** are display-quality issues (Go's
default formatter producing `[1 2 3]` instead of `[1,2,3]` etc.),
or carry an inherent DuckDB constraint (TIMESTAMPTZ is internally
UTC-normalised). Fixing them is desirable but not blocking and is
the subject of a separate ADR.

### 1.2 Why the bug stayed hidden

- Existing test coverage exercises VARCHAR / INTEGER / DOUBLE
  exclusively (e.g. `TestPreviewTable` loads a CSV of names + ages).
  None of the bug-class types ever appeared in a test fixture.
- The Preview path's blind cast made every binary type *look* like
  a string. Until a user happened to load JSON with a UUID-shaped
  column, no one saw the corruption.
- DuckDB CLI auto-formats UUID and DECIMAL on output. The CLI is
  silent about the binary form because users never see it. Only
  consumers going through `database/sql` are exposed.

---

## 2. Goals

1. **Correct rendering** of UUID, BLOB, DECIMAL, INTERVAL, MAP,
   and TIME across all three result paths.
2. **One helper, three call sites** — Preview, QuerySQL, and
   QuerySQLToCSV must dispatch through the same conversion so
   future fixes don't need to be applied three times.
3. **Type-dispatched, not value-sniffed** — the dispatch key is
   `rows.ColumnTypes()[i].DatabaseTypeName()`, not `len(b) == 16`
   guesses or `utf8.Valid(b)` heuristics. The latter would
   misclassify any 16-byte VARCHAR or any binary that happens to
   be valid UTF-8.
4. **Whole-row JSON marshal must succeed** for any combination of
   typed columns. The current MAP-induced silent failure breaks
   the Wails event that carries Preview rows to React.
5. **Discovery sweep stays as a permanent regression guard**.
   Future DuckDB upgrades, new types, or driver swaps trigger
   visible diff in the sweep output.
6. **No persistence-format change**. Sessions, `.shellagent`
   exports, and stored memory records remain byte-compatible.

Non-goals (deferred to Phase 2):

- LIST / STRUCT / JSON-as-VARCHAR display polish.
- TIMESTAMPTZ original-TZ preservation (DuckDB normalises
  internally; this is not a Go-side fix).
- Migrating to `github.com/marcboeker/go-duckdb/v2` (separate
  evaluation; the type-dispatch fix is needed regardless).

---

## 3. Design

### 3.1 Helper signature

```go
// renderScalar converts a single value scanned through database/sql
// into a Go value that is safe for both downstream JSON marshaling
// and human display. It dispatches on the upstream DuckDB type
// name (from sql.ColumnType.DatabaseTypeName) rather than sniffing
// the runtime Go type, which would miscategorise text-shaped
// binary blobs.
func renderScalar(v any, dbTypeName string) any
```

Returned values per type:

| dbTypeName            | Returned Go value                                              |
|-----------------------|----------------------------------------------------------------|
| `UUID`                | canonical lowercase string `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxx` (formatted from 16 bytes) |
| `BLOB`                | `[]byte` (unchanged) — JSON marshals to base64 (spec) and CSV gets base64 explicitly |
| `DECIMAL(p,s)`        | canonical decimal string `"123.456"` (formatted from `duckdb.Decimal` Width/Scale/Value) |
| `INTERVAL`            | ISO-8601 string `"P1Y2M3DT4H5M6S"` (formatted from `duckdb.Interval` Months/Days/Micros) |
| `MAP(K,V)`            | `map[string]any` constructed from `duckdb.Map` so JSON marshal works |
| `TIME`                | string `"12:34:56"` (clock-only, no `0001-01-01` prefix; formatted from `time.Time`) |
| `VARCHAR` / `*INTEGER` / `*BIGINT` / `DOUBLE` / `BOOLEAN` / `DATE` / `TIMESTAMP*` | unchanged |
| anything else (LIST, STRUCT, ENUM, BIT, future types) | unchanged (Phase 2 territory) |

### 3.2 Dispatch table

The mapping above is a small table (`map[string]func(any) any`)
inside `internal/analysis`. Pattern-match `DECIMAL(*)` and
`MAP(*)` via prefix because the parameter syntax varies.

Reasoning: a switch statement on bare type names plus two prefix
checks keeps the code readable and lets a new entry land as one
table row when the next type-class bug shows up.

### 3.3 Plumbing — three call sites

All three sites currently iterate `rows.Next()` and call
`rows.Scan` into an `[]any`. The fix is identical in shape:

1. After `rows.Columns()`, also call `rows.ColumnTypes()`.
   Cache the `DatabaseTypeName()` per column once.
2. Inside the `rows.Next()` loop, replace the existing
   conditional cast with `values[i] = renderScalar(values[i],
   dbTypeNames[i])`.
3. CSV path: after `renderScalar`, the existing `csvFormat`
   continues to handle `string`, `time.Time`, and everything
   else uniformly. BLOB is the only exception — `csvFormat`
   gains an explicit `[]byte → base64` branch (or `hex` —
   chosen at implementation time) so a CSV cell never contains
   raw binary.

### 3.4 BLOB encoding choice for CSV

The Preview/JSON paths are unambiguous: JSON's only standard
binary encoding is base64, and JavaScript clients can decode it
straightforwardly. CSV has no standard binary encoding. Two
candidates:

- **base64**: shorter, ubiquitous, but contains `+` and `/`
  which some downstream CSV parsers interpret oddly inside
  unquoted fields. Always works inside quoted fields (which is
  what `encoding/csv` produces by default).
- **hex**: longest output, no quoting concerns, easy to grep.

Choosing **base64** for CSV. encoding/csv quotes any field
containing `,` `"` `\n` and trims nothing in unquoted fields,
so base64's `+/=` characters round-trip safely. Documented in
the row-level comment on `csvFormat`.

### 3.5 What about HUGEINT and UBIGINT?

The sweep showed both render correctly in all three paths today:
`*big.Int` carries its own `MarshalJSON`, and uint64 does too via
the standard library. They're left alone.

### 3.6 What about whole-row JSON marshal?

Once `MAP(...)` columns return `map[string]any` instead of
`duckdb.Map`, `json.Marshal(prev.Rows[0])` succeeds for any
combination. No additional fix needed.

---

## 4. Testing

### 4.1 Promote the discovery sweep to a regression test

The current `engine_typesweep_test.go` is a chatty `t.Logf`
diagnostic. After the fix:

- Add an explicit `want` table (per column: expected go-type
  + expected JSON form + expected CSV form for the six fixed
  types). Assert on it.
- Keep the unfixed types as `t.Logf`-only entries so the
  sweep still prints visible drift if a future DuckDB version
  changes the rendering of LIST / STRUCT etc.
- Document at the top of the file that this test is the
  permanent guard for type-dispatch behaviour and any new
  DuckDB type added must extend it.

### 4.2 Targeted regression test for the user's symptom

A small `TestUUIDLoadedFromJSON_RendersCanonically` that loads
a synthesized JSONL file with one row containing a UUID-shaped
string, confirms `DESCRIBE` reports `UUID`, and asserts the
canonical form appears in Preview / QuerySQL / CSV. This pins
the originally reported bug independently of the broader sweep.

### 4.3 Whole-row marshal test

`TestPreviewRowMarshalsToJSON_AllSweepTypes` calls
`json.Marshal(prev.Rows[0])` against the sweep table and
asserts it returns non-empty bytes with a valid JSON object
shape. Pins the MAP-induced silent failure.

---

## 5. Risks & open questions

- **Future DuckDB type additions** (e.g. UNION, BIT-vector
  enhancements, ARRAY of fixed length) will land as `[]byte` or
  custom structs and may need a new dispatch entry. The sweep
  test will surface them as visible diff but won't auto-fix.
  Acceptable: each such bug becomes a one-row addition to the
  table.
- **go-duckdb v1.8.5 → v2.4.3 migration** could change some of
  the binary representations (e.g. UUID may start returning as a
  `string` on v2). The dispatch table degrades gracefully — a
  type that's already a `string` flows through unchanged. We
  evaluate the v2 migration in its own ADR; this fix is
  prerequisite-free.
- **DECIMAL precision edge cases**: `duckdb.Decimal` exposes
  Width/Scale/Value (big.Int). Naive `strconv` won't handle
  scale > 0 correctly. The implementation must do
  `value.String()` and insert the decimal point at position
  `len - scale` (with leading zero if `scale >= len`). Tested
  against Width=10,Scale=3,Value=123456 → `"123.456"`, and
  Width=18,Scale=2,Value=1 → `"0.01"`.
- **INTERVAL ISO-8601 vs `duckdb.Interval` fields**: ISO-8601
  is the most portable cross-system representation, but
  `duckdb.Interval` is a tuple of months / days / micros that
  DuckDB does not normalise. `P1M30D` and `P2M0D` are different
  values. The format must preserve all three components even
  when zero on output, e.g. `P0Y14M0DT0S` for "14 months".
- **TIME format** with sub-second precision: DuckDB TIME has
  microsecond precision. The format `15:04:05.999999` (Go
  layout) suffices; trailing zeros are dropped by the `.9`
  spec so plain seconds render as `12:34:56`.
- **CSV encoding of base64-with-padding**: `=` is allowed
  unquoted in CSV bodies but some downstream parsers
  (older Excel imports especially) treat trailing `=` as a
  formula trigger. Mitigated by always emitting through
  `encoding/csv` which quotes the field; no separate
  defensive escape needed.

---

## 6. Rejected alternatives

### 6.1 Force `SELECT col::VARCHAR` server-side

Rewrite every read query to cast all columns to VARCHAR before
fetching. **Rejected**: collapses all type information to text
(loses typed numbers in tool results, loses `time.Time` in the
Preview UI which the React side relies on), and can't be applied
to `SELECT *` from arbitrary user-table queries.

### 6.2 `utf8.Valid(b)` only

Treat any `[]byte` as a string only if it's valid UTF-8;
otherwise pass through as bytes. **Rejected**: a 16-byte UUID
binary is *frequently* valid UTF-8 by coincidence (the byte
ranges overlap with ASCII printable + Latin-1). The user's
reproducer happened to contain garbage characters because the
specific UUIDs decoded that way; with different UUIDs the
heuristic can silently produce plausible-but-wrong text. Type
dispatch is the only deterministic option.

### 6.3 Disable DuckDB UUID inference

Make `LoadJSON` / `LoadJSONL` pass a parameter to suppress UUID
auto-detection (i.e. force UUID-shaped strings to remain
VARCHAR). **Rejected**: DuckDB does not expose a knob for this
on `read_json` (only `auto_detect=false` which suppresses *all*
inference, requiring callers to provide a full schema). The
type-dispatch fix solves the problem at the rendering layer
where the data already lives correctly in DuckDB.

### 6.4 Convert all `[]byte` to hex / base64 unconditionally

Always render bytes as hex or base64. **Rejected**: VARCHAR
columns (which legitimately come back as `[]byte` from
go-duckdb) would render as hex strings — useless in the UI,
unreadable in CSV, and burns LLM tokens for no reason.

### 6.5 Upgrade go-duckdb to v2.4.3 first

Bump the driver and hope the new version doesn't have the
binary-UUID quirk. **Rejected as prerequisite** (still on the
table as a follow-up): the v1 → v2 migration is its own
risk-bearing change (full API audit, all DuckDB-touching
callers, sandbox image bumps, regression sweep across every
analysis test). The type-dispatch fix is small, self-contained,
and works regardless of which driver version we're on. Do the
fix first, evaluate the upgrade after.

### 6.6 Phase 1 + Phase 2 in the same release

Bundle the LIST / STRUCT / JSON display polish and the
TIMESTAMPTZ work into v0.6.4. **Rejected**: enlarges the
review surface and the regression risk. Phase 1 is data-correctness
(the user is being shown wrong data right now); Phase 2 is
display polish (the user is being shown awkward formatting).
Ship Phase 1 fast, Phase 2 when designed.

---

## 7. Compatibility & rollout

- **Persistence format**: unchanged. No ColumnType info or
  rendering choice lives on disk; the conversion is purely at
  result-fetch time.
- **LLM-observable**: tool results that previously contained
  base64-of-UUID-bytes now contain canonical UUID strings.
  Same for DECIMAL, INTERVAL, MAP, TIME. This is a strict
  improvement (the LLM can recognise UUIDs), but model behaviour
  on existing pinned-context sessions may shift as the LLM sees
  the new, correct strings instead of the corrupted ones.
  Documented in CHANGELOG.
- **CSV-observable**: existing pipelines that consumed CSV
  output of UUID/BLOB/DECIMAL columns received garbage bytes
  before. Behaviour change is from broken to correct, but if
  any external script was somehow parsing the garbage that
  script breaks. Documented in CHANGELOG with explicit before
  / after examples.
- **UI-observable**: Data panel summaries that previously
  showed mojibake for UUID columns now show the canonical form.
  No frontend code change required.
- **Rollout**: Phase 1 ships as v0.6.4 with the test sweep
  promoted to assertions. Phase 2 is its own ADR (number TBD)
  and its own point release.

---

## 8. References

- Reported by user: 2026-05-14, "load-data で大きなサイズの
  JSON をロードする際に、一部のデータが壊れている" (load-data
  showing corrupted data on a 17 MB JSON file with a UUID column).
- DuckDB type system reference:
  https://duckdb.org/docs/sql/data_types/overview
- go-duckdb v1.8.5 driver:
  https://github.com/marcboeker/go-duckdb (v1 line)
- Discovery sweep test:
  `app/internal/analysis/engine_typesweep_test.go`
- Existing affected sites:
  - `app/internal/analysis/engine.go:449-456` (Preview)
  - `app/internal/analysis/engine.go:374-388` (csvFormat)
  - `app/internal/analysis/engine.go:539-547` (QuerySQL)
