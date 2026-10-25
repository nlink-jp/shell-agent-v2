// Package analysis — read-only enforcement for agent-written SQL.
//
// query-sql, quick-summary and export-sql-to-csv are declared read
// tools, and create-from-query only ever adds a table. The SQL they run
// is written by the LLM, so whether it is read-only is decided by
// DuckDB's own parser, not by the query text: the query must be exactly
// one statement and that statement's type must be SELECT. The type is a
// finite enum the parser assigns, which covers every spelling of a read
// (SELECT, FROM-first, WITH ... SELECT, VALUES, DESCRIBE, SUMMARIZE,
// SHOW) and none of a write (a second statement after a semicolon,
// WITH ... INSERT, COPY, ATTACH, SET, INSTALL, LOAD, CALL, EXPLAIN
// ANALYZE, dynamic PIVOT). See docs/en/adr/0034-read-only-sql-by-statement-type.md.
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
// selectConn is DuckDB's parse or bind error for the query itself, which
// callers report the way they report an execution error.
var errNotSelect = errors.New("query must be a single SELECT statement; INSERT/UPDATE/DELETE/COPY/ATTACH/SET, other statements and a second statement after a semicolon are rejected")

// selectConn reserves a connection and checks that query is a single
// SELECT statement on it. The caller runs query on the returned
// connection and closes it.
func selectConn(ctx context.Context, db *sql.DB, query string) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireSelect(conn, query); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

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

// wrapQueryErr prefixes DuckDB's own error the way the caller reports
// execution errors, and passes the gate's refusal through unchanged.
func wrapQueryErr(prefix string, err error) error {
	if errors.Is(err, errNotSelect) {
		return err
	}
	return fmt.Errorf("%s: %w", prefix, err)
}
