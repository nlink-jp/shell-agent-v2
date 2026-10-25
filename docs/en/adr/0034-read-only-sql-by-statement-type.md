# ADR-0034: Enforce read-only agent SQL in the engine

- Status: Accepted
- Deciders: magi
- Related: ADR-0013 (saved-query tables — its `isReadOnlySQL` gate is replaced here), ADR-0033 (duckdb-go driver)

## 1. Context

query_sql, quick_summary, analyze_data and export-sql-to-csv only read;
save_query (create-from-query) only adds a table. The SQL is written by
the LLM. query_sql, analyze_data and save_query ask for MITL approval by
default; quick_summary does not, and any of them can have approval
turned off in Settings. Until now `isReadOnlySQL` decided whether the
SQL was read-only by checking that its text did not *start* with one
of nine keywords.

Measured on 2026-10-26 against v0.18.0:

- `SELECT 1; DROP TABLE t` passed and dropped the table: the driver
  executes every statement but the last while preparing a
  multi-statement query, so the leading `SELECT` was all the check saw.
- `WITH c AS (…) INSERT …`, `COPY t TO '<file>'`, `ATTACH`, `SET`,
  `CALL` and `EXPLAIN ANALYZE <write>` passed too.

A first replacement — DuckDB's statement type must be a single SELECT —
was itself measured past by the pre-release review:

- A data-modifying CTE at the top level,
  `WITH d AS (DELETE FROM t RETURNING *) SELECT count(*) FROM d`, is type
  SELECT, ran, and deleted rows; the same body in save_query inserted
  into another table while creating its own.
- Plain SELECTs of table functions write outside the database:
  `enable_profiling(save_location='<file>')` overwrote an existing file,
  `enable_logging(storage='file', …)` created a directory of logs, and
  `read_csv('https://…')` installed the httpfs extension.

Both checks decided "does not write" from a stand-in (the text, then
the statement type). Each stand-in let through the writes it did not
describe. A second review pass then broke a `READ ONLY` transaction
too: `json_execute_serialized_sql(json_serialize_sql('<DELETE …>'))`
runs its SQL on an internal connection outside the caller's
transaction, and deleted the rows; in save_query, `dbgen` / `dsdgen`
(TPC-H/DS, statically linked) created 8 and 24 tables next to the
saved one.

## 2. Decision

Enforce it in the engine, in three layers, each closing what the
others cannot:

1. **Statement type** (`requireSelect`). The query is prepared with the
   driver's `Prepare` — which refuses a multi-statement query without
   running any of it — and must be one statement of type SELECT. This
   refuses `INSERT` / `UPDATE` / `DELETE`, `CREATE`, `ALTER`, `DROP`,
   `COPY`, `ATTACH`, `SET`, `INSTALL`, `LOAD`, `CALL`, `EXPLAIN` and
   dynamic `PIVOT` before anything runs, with a message the agent can
   act on. `SELECT`, `FROM t`, `WITH … SELECT`, `VALUES`, `DESCRIBE`,
   `SUMMARIZE`, `SHOW` and `PIVOT … IN (…)` are type SELECT.
2. **Read-only database** (`openReaderLocked`). Agent SQL never runs on
   the read-write instance. A read closes it and runs the query on a
   throwaway in-memory instance that attaches the session file
   `READ_ONLY` under its usual name and makes it the default. The
   database itself is read-only there, so DuckDB refuses every write to
   it from any connection — the data-modifying CTE, `dbgen`, and SQL run
   by `json_execute_serialized_sql` alike. What such SQL can still
   write lands in the throwaway instance's own in-memory catalog and is
   discarded; spills go to `<session file>.tmp`, as the read-write
   instance's do. The next write reopens the read-write instance. Only
   one instance has the file open at a time: DuckDB does not enforce
   that within one process, so it rests on the engine's mutex and on
   there being one Engine per session file.
3. **Locked configuration** (`lockConfiguration`, on every instance).
   Extension autoinstall and autoload are switched off and
   `lock_configuration` is set. DuckDB then refuses `enable_profiling` /
   `enable_logging` from any query, and a URL read no longer installs an
   extension. Nothing in the engine changes a setting afterwards, and
   the formats load-data reads (CSV, JSON) are built in.

save_query must write, so it runs its body the same way: on a reader,
with a fresh staging file attached writable, as
`CREATE TABLE stage.result AS <body>`. The read-write instance then
attaches the staging file `READ_ONLY` and copies that one table in
(`CREATE TABLE <name> AS SELECT * FROM stage.result`), and the staging
file is deleted. Column types survive the copy; whatever else the body
writes stays in the throwaway instance or the deleted staging file.

A parse or bind error is DuckDB's own and is reported with the usual
`query:` / `save query:` prefix; only layer 1's refusal uses its own
message.

## 3. Consequences

- Whether agent SQL can write is decided by DuckDB's access mode and
  configuration lock, not by how the query is spelled or which function
  it calls.
- A read closes the read-write instance (a checkpoint) and the next
  write reopens it. A run of queries costs one switch; alternating
  reads and writes costs one each time.
- `EXPLAIN` and dynamic `PIVOT` are refused where the old check let
  them through; `PIVOT … ON … IN (…)` and plain SELECTs cover the same
  ground.
- The test drives each refused form — leading writes in a
  multi-statement query, the data-modifying CTEs, `dbgen` / `dsdgen`,
  `json_execute_serialized_sql`, and the file-writing table functions —
  through all three read paths and save_query, and checks the tables,
  the attached databases and the files afterwards. A control run with
  the session file attached writable, or with the lock removed, fails
  it. A second test pins that writes after a read reopen the read-write
  instance (Reset used to return early on a closed handle) and that
  save_query keeps column types and leaves no staging file.

## 4. Accepted residual: reading files

A SELECT can still read local files — `read_csv('<path>')`,
`read_parquet(…)`, `read_text(…)`, `read_duckdb(…)` (other sessions'
databases included) and `glob(…)` — anything the process can read.
Closing that was measured and rejected:

- DuckDB's `enable_external_access = false` cannot be relaxed while the
  database runs (`allowed_paths` cannot be changed once it is off), so
  load-data — which reads the user's file through `read_csv_auto` —
  would stop working, and spilling to the temp directory failed with
  out-of-memory in the same probe.
- The agent already reads files through the read shell tools without
  approval. Taking the ability away from SQL alone would not change
  what the agent can do; it would only move it.

URL reads now fail unless the extension is already installed, because
autoinstall is off. The parquet path to CVE-2026-111497 (vendored
Brotli 1.1.0) stays reachable for local files until DuckDB ships the
fix (see the v0.18.0 CHANGELOG).
