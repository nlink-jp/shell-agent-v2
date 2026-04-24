// Package analysis provides session-scoped DuckDB data analysis.
package analysis

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/marcboeker/go-duckdb"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TableMeta holds metadata about a loaded table.
type TableMeta struct {
	Name        string   `json:"name"`
	Columns     []string `json:"columns"`
	RowCount    int64    `json:"row_count"`
	Description string   `json:"description"`
}

// Engine manages a session-scoped DuckDB instance.
type Engine struct {
	sessionID string
	dbPath    string
	db        *sql.DB
	tables    map[string]*TableMeta
	mu        sync.Mutex
}

// New creates a new analysis engine for the given session.
// The database is created lazily on first use.
func New(sessionID string) *Engine {
	return &Engine{
		sessionID: sessionID,
		dbPath:    filepath.Join(memory.SessionDir(sessionID), "analysis.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
}

// NewWithPath creates an engine with an explicit database path (for testing).
func NewWithPath(sessionID, dbPath string) *Engine {
	return &Engine{
		sessionID: sessionID,
		dbPath:    dbPath,
		tables:    make(map[string]*TableMeta),
	}
}

// Open opens or creates the DuckDB instance.
func (e *Engine) Open() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.db != nil {
		return nil
	}

	dir := filepath.Dir(e.dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	db, err := sql.Open("duckdb", e.dbPath)
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}
	e.db = db

	return e.rebuildTableMeta()
}

// Close releases the DuckDB connection.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.db != nil {
		err := e.db.Close()
		e.db = nil
		e.tables = make(map[string]*TableMeta)
		return err
	}
	return nil
}

// IsOpen reports whether the database connection is open.
func (e *Engine) IsOpen() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db != nil
}

// DBPath returns the path to the session's DuckDB file.
func (e *Engine) DBPath() string {
	return e.dbPath
}

// Tables returns metadata for all loaded tables.
func (e *Engine) Tables() []*TableMeta {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := make([]*TableMeta, 0, len(e.tables))
	for _, m := range e.tables {
		result = append(result, m)
	}
	return result
}

// HasData reports whether any tables exist.
func (e *Engine) HasData() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tables) > 0
}

// LoadFile loads a data file into a table, auto-detecting format by extension.
func (e *Engine) LoadFile(tableName, filePath string) error {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".csv", ".tsv":
		return e.LoadCSV(tableName, filePath)
	case ".json":
		return e.LoadJSON(tableName, filePath)
	case ".jsonl", ".ndjson":
		return e.LoadJSONL(tableName, filePath)
	default:
		return fmt.Errorf("unsupported file format: %s (supported: csv, tsv, json, jsonl)", ext)
	}
}

// LoadCSV loads a CSV file into a table.
func (e *Engine) LoadCSV(tableName, filePath string) error {
	if err := e.Open(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	query := fmt.Sprintf(
		"CREATE OR REPLACE TABLE %s AS SELECT * FROM read_csv_auto('%s')",
		sanitizeIdentifier(tableName), filePath,
	)
	if _, err := e.db.Exec(query); err != nil {
		return fmt.Errorf("load CSV: %w", err)
	}

	return e.refreshTableMeta(tableName)
}

// LoadJSON loads a JSON file (array of objects) into a table.
func (e *Engine) LoadJSON(tableName, filePath string) error {
	if err := e.Open(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	query := fmt.Sprintf(
		"CREATE OR REPLACE TABLE %s AS SELECT * FROM read_json_auto('%s')",
		sanitizeIdentifier(tableName), filePath,
	)
	if _, err := e.db.Exec(query); err != nil {
		return fmt.Errorf("load JSON: %w", err)
	}

	return e.refreshTableMeta(tableName)
}

// LoadJSONL loads a JSONL/NDJSON file into a table.
func (e *Engine) LoadJSONL(tableName, filePath string) error {
	if err := e.Open(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	query := fmt.Sprintf(
		"CREATE OR REPLACE TABLE %s AS SELECT * FROM read_json_auto('%s', format='newline_delimited')",
		sanitizeIdentifier(tableName), filePath,
	)
	if _, err := e.db.Exec(query); err != nil {
		return fmt.Errorf("load JSONL: %w", err)
	}

	return e.refreshTableMeta(tableName)
}

// Schema returns a text representation of all table schemas for LLM prompts.
func (e *Engine) Schema() string {
	e.mu.Lock()
	defer e.mu.Unlock()

	var sb strings.Builder
	for _, t := range e.tables {
		sb.WriteString(fmt.Sprintf("Table: %s (%d rows)\n", t.Name, t.RowCount))
		if t.Description != "" {
			sb.WriteString(fmt.Sprintf("  Description: %s\n", t.Description))
		}
		sb.WriteString(fmt.Sprintf("  Columns: %s\n\n", strings.Join(t.Columns, ", ")))
	}
	return sb.String()
}

// QuerySQL executes a read-only SQL query and returns results.
func (e *Engine) QuerySQL(query string) ([]map[string]any, error) {
	if err := e.Open(); err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if !isReadOnlySQL(query) {
		return nil, fmt.Errorf("only SELECT queries are allowed")
	}

	rows, err := e.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any)
		for i, col := range columns {
			row[col] = values[i]
		}
		results = append(results, row)
	}
	return results, nil
}

// SetTableDescription sets a persistent description on a table.
func (e *Engine) SetTableDescription(tableName, description string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.db == nil {
		return fmt.Errorf("database not open")
	}

	query := fmt.Sprintf("COMMENT ON TABLE %s IS '%s'",
		sanitizeIdentifier(tableName),
		strings.ReplaceAll(description, "'", "''"))
	if _, err := e.db.Exec(query); err != nil {
		return fmt.Errorf("set description: %w", err)
	}

	if meta, ok := e.tables[tableName]; ok {
		meta.Description = description
	}
	return nil
}

// Reset drops all tables and clears metadata.
func (e *Engine) Reset() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.db == nil {
		return nil
	}

	for name := range e.tables {
		if _, err := e.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", sanitizeIdentifier(name))); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
	}
	e.tables = make(map[string]*TableMeta)
	return nil
}

// --- internal ---

func (e *Engine) rebuildTableMeta() error {
	rows, err := e.db.Query("SHOW TABLES")
	if err != nil {
		return nil // empty database
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if err := e.refreshTableMeta(name); err != nil {
			continue
		}
	}
	return nil
}

func (e *Engine) refreshTableMeta(tableName string) error {
	meta := &TableMeta{Name: tableName}

	// Get columns via information_schema
	rows, err := e.db.Query(
		"SELECT column_name FROM information_schema.columns WHERE table_name = $1 ORDER BY ordinal_position",
		tableName)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				meta.Columns = append(meta.Columns, name)
			}
		}
	}

	// Get row count
	var count int64
	row := e.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", sanitizeIdentifier(tableName)))
	if err := row.Scan(&count); err == nil {
		meta.RowCount = count
	}

	// Get description from comment
	commentRow := e.db.QueryRow(fmt.Sprintf(
		"SELECT comment FROM duckdb_tables() WHERE table_name = '%s'", tableName))
	var comment sql.NullString
	if err := commentRow.Scan(&comment); err == nil && comment.Valid {
		meta.Description = comment.String
	}

	e.tables[tableName] = meta
	return nil
}

func isReadOnlySQL(query string) bool {
	q := strings.TrimSpace(strings.ToUpper(query))
	dangerous := []string{"INSERT", "UPDATE", "DELETE", "DROP", "CREATE", "ALTER", "LOAD", "INSTALL", "PRAGMA"}
	for _, kw := range dangerous {
		if strings.HasPrefix(q, kw) {
			return false
		}
	}
	return true
}

func sanitizeIdentifier(name string) string {
	return fmt.Sprintf("\"%s\"", strings.ReplaceAll(name, "\"", "\"\""))
}
