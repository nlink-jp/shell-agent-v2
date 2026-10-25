// Package analysis — read-only enforcement for agent-written SQL.
//
// query-sql, quick-summary and export-sql-to-csv only read, and
// create-from-query only adds a table. The SQL they run is written by
// the LLM, so the engine enforces that, in three layers that each close
// what the others cannot (ADR-0034):
//
//  1. Statement type. DuckDB's parser must classify the query as exactly
//     one SELECT statement. This refuses a second statement after a
//     semicolon, INSERT/UPDATE/DELETE, COPY, ATTACH, SET, INSTALL, LOAD,
//     CALL and EXPLAIN before anything runs.
//  2. A read-only transaction. A data-modifying CTE at the top level
//     (WITH d AS (DELETE … RETURNING *) SELECT …) is still type SELECT;
//     DuckDB refuses its write when the transaction is READ ONLY.
//  3. A locked configuration (lockConfiguration, at Open). Table
//     functions such as enable_profiling / enable_logging write files
//     from a plain SELECT, and reading a URL installs an extension;
//     with the configuration locked and extension autoload/autoinstall
//     off, DuckDB refuses both.
//
// create-from-query must write, so it runs the body nested as
// SELECT * FROM (<body>), where DuckDB's binder rejects a
// data-modifying CTE.
package analysis

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// lockStatements switch off extension autoinstall/autoload and lock
// the configuration for the life of the database instance. Nothing in
// the engine changes a setting after Open, and every format load-data
// reads (CSV, JSON) is built in.
var lockStatements = []string{
	"SET autoinstall_known_extensions = false",
	"SET autoload_known_extensions = false",
	"SET lock_configuration = true",
}

// lockConfiguration applies lockStatements to a freshly opened database.
func lockConfiguration(db *sql.DB) error {
	for _, stmt := range lockStatements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("lock configuration (%s): %w", stmt, err)
		}
	}
	return nil
}

// statementType asks DuckDB to parse query on conn and returns the type
// of its single statement. It uses the driver's Prepare, which refuses a
// multi-statement query instead of executing the leading statements the
// way PrepareContext (and therefore conn.QueryContext) does.
func statementType(conn *sql.Conn, query string) (duckdb.StmtType, error) {
	var typ duckdb.StmtType
	err := conn.Raw(func(dc any) error {
		st, err := dc.(driver.Conn).Prepare(query)
		if err != nil {
			return err
		}
		defer st.Close()
		typ, err = st.(*duckdb.Stmt).StatementType()
		return err
	})
	return typ, err
}

// errNotSelect is the gate's own refusal. Any other error from
// readConn is DuckDB's parse or bind error for the query itself, which
// callers report the way they report an execution error.
var errNotSelect = errors.New("query must be a single SELECT statement; INSERT/UPDATE/DELETE/COPY/ATTACH/SET, other statements and a second statement after a semicolon are rejected")

// requireSelect returns errNotSelect unless query parses as exactly one
// SELECT statement, or DuckDB's error when it does not parse or bind.
func requireSelect(conn *sql.Conn, query string) error {
	typ, err := statementType(conn, query)
	if err != nil {
		// The driver refuses a multi-statement query with a message
		// pointing at PrepareContext; say what the agent should change.
		if strings.Contains(err.Error(), "multi-statement") {
			return errNotSelect
		}
		return err
	}
	if typ != duckdb.STATEMENT_TYPE_SELECT {
		return errNotSelect
	}
	return nil
}

// readConn is a reserved connection inside a READ ONLY transaction, on
// which query has passed requireSelect. Close rolls the transaction
// back before returning the connection to the pool.
type readConn struct {
	*sql.Conn
}

// beginRead reserves a connection, checks query, and opens a READ ONLY
// transaction on it. The caller runs query on the returned connection
// and closes it after the rows.
func beginRead(ctx context.Context, db *sql.DB, query string) (*readConn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireSelect(conn, query); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION READ ONLY"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	return &readConn{conn}, nil
}

// Close ends the read-only transaction and releases the connection. A
// connection whose ROLLBACK failed may still be read-only, so it is
// discarded rather than handed to the next writer.
func (r *readConn) Close() error {
	if _, err := r.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		_ = r.Raw(func(any) error { return driver.ErrBadConn })
	}
	return r.Conn.Close()
}

// nestedBody wraps a create-from-query body so that DuckDB binds it as
// a subquery, where a data-modifying CTE is rejected. The body goes on
// its own lines so a trailing line comment cannot swallow the closing
// parenthesis, and a trailing semicolon is dropped.
func nestedBody(query string) string {
	body := strings.TrimRight(strings.TrimSpace(query), "; \t\r\n")
	return "SELECT * FROM (\n" + body + "\n)"
}

// wrapQueryErr prefixes DuckDB's own error the way the caller reports
// execution errors, and passes the gate's refusal through unchanged.
func wrapQueryErr(prefix string, err error) error {
	if errors.Is(err, errNotSelect) {
		return err
	}
	return fmt.Errorf("%s: %w", prefix, err)
}
