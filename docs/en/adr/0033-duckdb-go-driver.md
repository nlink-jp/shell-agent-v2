# ADR-0033: Move the DuckDB driver to duckdb-go (DuckDB 2.0)

- Status: Accepted
- Deciders: magi
- Related: ADR-0010 (DuckDB result rendering — §6.5 deferred this move), ADR-0011 (TIMESTAMPTZ rendering)

## 1. Context

The analysis engine linked `github.com/marcboeker/go-duckdb` v1.8.5,
which bundles DuckDB v1.1.3. That module path no longer receives
releases: the driver moved to `github.com/duckdb/duckdb-go/v2`, and
the `marcboeker` v1 and v2 lines are frozen. A DuckDB fix can
therefore never reach shell-agent-v2 on the old path.

The trigger was CVE-2026-111497, an out-of-bounds write in the Brotli
decoder. DuckDB's parquet extension vendors Brotli 1.1.0, and the
analysis engine runs LLM-written SQL in-process, so `read_parquet` on a
crafted file reaches it. On 2026-10-25 every DuckDB release (v1.4.5
LTS, v1.5.6, v2.0.0 LTS) still vendors 1.1.0 and the upstream fix
(duckdb/duckdb#27791) is unmerged — no driver release fixes it yet.

Candidates on the maintained path, by bundled DuckDB:

| duckdb-go | DuckDB | Support |
|---|---|---|
| v2.5.6 | v1.4.5 LTS | until 2026-11-17 |
| v2.10506.0 | v1.5.6 | until 2027-03 |
| v2.20000.0 | v2.0.0 LTS | until at least 2027-10 |

## 2. Decision

Move to `github.com/duckdb/duckdb-go/v2` v2.20000.0 (DuckDB v2.0.0
LTS): the longest-supported line, and the one the upstream Brotli fix
targets (DuckDB `main`), so the CVE fix arrives as a patch bump.

Changes it takes:

- **MAP** now scans into `duckdb.OrderedMap` instead of `duckdb.Map`.
  `renderScalar` converts it with `mapToStringKeyed`, so the UI, the
  LLM tool result and CSV export show the same values as before. The
  type-sweep test caught this as the only drift; every other type
  scans into the same Go value as under v1.8.5.
- **Arrow is opt-in** (`-tags duckdb_arrow`) instead of opt-out, so the
  `no_duckdb_arrow` tag is dropped from the Makefile. Arrow stays out
  of the binary.
- **Database files.** Session databases created before this release
  open and stay readable by older versions after DuckDB 2.0 writes to
  them (measured: a v1.1.3 file written by v2.0.0 reopens in v1.1.3).
  A database *created* by DuckDB 2.0 cannot be opened by DuckDB 1.x,
  so a session made by this release — or a `.shellagent` bundle holding
  one — does not open in v0.17.x.

## 3. Consequences

- The CVE remains open until DuckDB ships the Brotli fix; the CHANGELOG
  says so. Taking it is a `go get` of the next v2.2000x release.
- DuckDB 2.0 parses with its new PEG parser by default. The test suite
  passes unchanged; LLM-written SQL that the old parser accepted and the
  new one rejects surfaces as a query error the agent can retry.
- Downgrading after creating new sessions is not supported (see §2).

## 4. Rejected alternatives

- **Stay on go-duckdb v1.8.5.** Frozen at DuckDB v1.1.3; no fix can land.
- **duckdb-go v2.5.6 (DuckDB 1.4 LTS).** Support ends in three weeks.
- **duckdb-go v2.10506.0 (DuckDB 1.5).** Supported until 2027-03, but
  it is unclear whether the Brotli fix is backported there; a second
  migration to 2.0 would follow within months anyway.
