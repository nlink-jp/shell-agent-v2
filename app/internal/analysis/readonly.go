// Package analysis — read-only enforcement for agent-written SQL.
//
// query-sql, quick-summary, analyze-data and export-sql-to-csv only
// read, and save-query only adds a table. The SQL they run is written
// by the LLM, so the engine enforces that, in three layers (ADR-0034):
//
//  1. Statement type. DuckDB's parser must classify the query as exactly
//     one SELECT statement. This refuses a second statement after a
//     semicolon, INSERT/UPDATE/DELETE, COPY, ATTACH, SET, INSTALL, LOAD,
//     CALL and EXPLAIN before anything runs, with a message the agent
//     can act on.
//  2. A read-only database. The query runs on a throwaway in-memory
//     instance that attaches the session file READ_ONLY. DuckDB refuses
//     every write to it — a data-modifying CTE, dbgen, and SQL run by
//     functions on internal connections (json_execute_serialized_sql),
//     which a READ ONLY transaction does not contain.
//  3. A locked configuration with extension autoinstall/autoload off,
//     on every instance. A plain SELECT of enable_profiling /
//     enable_logging writes files, and reading a URL installs an
//     extension; DuckDB refuses both.
//
// save-query runs its body on such a reader into a separate staging
// file, then copies the one result table into the session database.
package analysis

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// lockStatements switch off extension autoinstall/autoload and lock
// the configuration for the life of a database instance. Nothing in
// the engine changes a setting afterwards, and every format load-data
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
// requireSelect is DuckDB's parse or bind error for the query itself, which
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

// reader is a throwaway in-memory DuckDB instance with the session
// database attached READ_ONLY (and, for save-query, a writable staging
// database), reserved to one connection on which it is the default.
type reader struct {
	db   *sql.DB
	conn *sql.Conn
}

// Close releases the reader's connection and instance.
func (r *reader) Close() error {
	return errors.Join(r.conn.Close(), r.db.Close())
}

// readerAlias names the attached session database the way DuckDB names
// it when the file is opened directly (its file stem), so qualified
// names in agent SQL resolve the same on both instances.
func readerAlias(dbPath string) string {
	stem := strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))
	switch strings.ToLower(stem) {
	case "", "memory", "system", "temp", "stage":
		return "session"
	}
	return stem
}

// openReaderLocked closes the read-write instance — so only one
// instance has the file open — and returns a reader on the session
// database. stage, if not empty, is attached writable as "stage". The
// caller holds e.mu, has opened the engine, and closes the reader.
func (e *Engine) openReaderLocked(ctx context.Context, stage string) (*reader, error) {
	if e.db != nil {
		err := e.db.Close()
		e.db = nil
		if err != nil {
			return nil, fmt.Errorf("close read-write instance: %w", err)
		}
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open reader: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	r := &reader{db: db, conn: conn}
	alias := sanitizeIdentifier(readerAlias(e.dbPath))
	stmts := []string{"ATTACH '" + escapeSQLString(e.dbPath) + "' AS " + alias + " (READ_ONLY)"}
	if stage != "" {
		stmts = append(stmts, "ATTACH '"+escapeSQLString(stage)+"' AS stage")
	}
	// Spill next to the session file, as the read-write instance does,
	// rather than into the process's working directory.
	stmts = append(stmts, "USE "+alias, "SET temp_directory = '"+escapeSQLString(e.dbPath+".tmp")+"'")
	stmts = append(stmts, lockStatements...)
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			r.Close()
			return nil, fmt.Errorf("open reader (%s): %w", stmt, err)
		}
	}
	return r, nil
}

// wrapQueryErr prefixes DuckDB's own error the way the caller reports
// execution errors, and passes the gate's refusal through unchanged.
func wrapQueryErr(prefix string, err error) error {
	if errors.Is(err, errNotSelect) {
		return err
	}
	return fmt.Errorf("%s: %w", prefix, err)
}
