# ADR-0034: Decide read-only SQL by DuckDB's statement type

- Status: Accepted
- Deciders: magi
- Related: ADR-0013 (saved-query tables — its `isReadOnlySQL` gate is replaced here), ADR-0033 (duckdb-go driver)

## 1. Context

query-sql, quick-summary and export-sql-to-csv are read tools (no
MITL approval), and create-from-query only ever adds a table. The SQL
they run is written by the LLM. Until now `isReadOnlySQL` decided
whether it was read-only by checking that the text did not *start*
with one of nine keywords.

Measured on 2026-10-26 against v0.18.0:

- `SELECT 1; DROP TABLE t` passed the check and dropped the table: the
  driver executes every statement but the last while preparing a
  multi-statement query, so the leading `SELECT` was all the check saw.
- `WITH c AS (…) INSERT …`, `COPY t TO '<file>'`, `ATTACH`, `SET`,
  `CALL` and `EXPLAIN ANALYZE <write>` also passed — none starts with a
  listed keyword. DuckDB 2.0 adds more statements of this kind.

The check decided over an unbounded domain (query text) and kept
needing the next spelling.

## 2. Decision

Let DuckDB's parser decide. `requireSelect` prepares the query with
the driver's `Prepare` — which refuses a multi-statement query instead
of executing its leading statements — and reads the statement type, a
finite enum the parser assigns. The query must be exactly one statement
of type SELECT. The check and the execution run on the same reserved
connection, on the same string.

- Allowed (type SELECT): `SELECT`, `FROM t`, `WITH … SELECT`,
  `VALUES`, `DESCRIBE`, `SUMMARIZE`, `SHOW`, `PRAGMA table_info`.
- Refused before anything runs: a second statement, `INSERT` /
  `UPDATE` / `DELETE` (with or without `WITH`), `CREATE`, `ALTER`,
  `DROP`, `COPY`, `ATTACH`, `SET`, `INSTALL`, `LOAD`, `CALL`, `EXPLAIN`
  (incl. `ANALYZE`), and dynamic `PIVOT` (a multi-statement).
- `query('<sql>')` is refused by DuckDB itself unless its argument is a
  single SELECT.
- create-from-query checks its body the same way, then runs
  `CREATE TABLE <name> AS <body>` on that connection, so the only write
  is the table it creates.

A parse or bind error is DuckDB's own and is reported with the usual
`query:` / `save query:` prefix; only the gate's refusal uses its own
message.

## 3. Consequences

- The class "a write that does not start with a listed keyword" is
  closed: the decision no longer depends on how the query is spelled.
- `EXPLAIN` and dynamic `PIVOT` are refused where the old check let
  them through; `PIVOT … ON … IN (…)` with explicit values and plain
  SELECTs cover the same ground.
- The test drives every refused form through all three read paths and
  create-from-query and checks the database is unchanged afterwards,
  not only that an error came back.

## 4. Accepted residual: reading files

A SELECT can still read files and URLs (`read_csv('<path>')`,
`read_parquet(…)`). Closing that was measured and rejected:

- DuckDB's `enable_external_access = false` cannot be relaxed while the
  database runs (`allowed_paths` cannot be changed once it is off), so
  load-data — which reads the user's file through `read_csv_auto` —
  would stop working, and spilling to the temp directory failed with
  out-of-memory in the same probe.
- The agent already reads files through the read shell tools without
  approval. Taking the ability away from SQL alone would not change
  what the agent can do; it would only move it.

This keeps the parquet path to CVE-2026-111497 (vendored Brotli 1.1.0)
reachable until DuckDB ships the fix (see the v0.18.0 CHANGELOG).
