// Package analysis provides session-scoped DuckDB data analysis.
package analysis

import (
	"path/filepath"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Engine manages a session-scoped DuckDB instance.
type Engine struct {
	sessionID string
	dbPath    string
}

// New creates a new analysis engine for the given session.
// The database is created lazily on first data load.
func New(sessionID string) *Engine {
	return &Engine{
		sessionID: sessionID,
		dbPath:    filepath.Join(memory.SessionDir(sessionID), "analysis.duckdb"),
	}
}

// Close releases the DuckDB connection.
func (e *Engine) Close() error {
	// TODO: close DuckDB connection (Phase 1)
	return nil
}

// DBPath returns the path to the session's DuckDB file.
func (e *Engine) DBPath() string {
	return e.dbPath
}
