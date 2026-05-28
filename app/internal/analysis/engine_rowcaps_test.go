package analysis

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// TestSetRowCaps_ZeroKeepsDefault: 0 for either cap resolves to the
// package default; explicit values pass through.
func TestSetRowCaps_ZeroKeepsDefault(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()

	if got := e.MaxQueryRows(); got != config.DefaultMaxQueryRows {
		t.Errorf("default MaxQueryRows() = %d, want %d", got, config.DefaultMaxQueryRows)
	}
	if got := e.MaxExportRows(); got != config.DefaultMaxExportRows {
		t.Errorf("default MaxExportRows() = %d, want %d", got, config.DefaultMaxExportRows)
	}

	e.SetRowCaps(0, 0)
	if got := e.MaxQueryRows(); got != config.DefaultMaxQueryRows {
		t.Errorf("SetRowCaps(0,0) MaxQueryRows() = %d, want %d", got, config.DefaultMaxQueryRows)
	}
	if got := e.MaxExportRows(); got != config.DefaultMaxExportRows {
		t.Errorf("SetRowCaps(0,0) MaxExportRows() = %d, want %d", got, config.DefaultMaxExportRows)
	}

	e.SetRowCaps(5, 7)
	if got := e.MaxQueryRows(); got != 5 {
		t.Errorf("MaxQueryRows() = %d, want 5", got)
	}
	if got := e.MaxExportRows(); got != 7 {
		t.Errorf("MaxExportRows() = %d, want 7", got)
	}
}

// TestQuerySQLToCSV_AllowsBeyondQueryCap pins the issue #14 / ADR-0029
// regression: the CSV-export path must not inherit the chat-output cap.
// With default caps, exporting >DefaultMaxQueryRows rows succeeds in
// full because the export cap (DefaultMaxExportRows) is far higher.
func TestQuerySQLToCSV_AllowsBeyondQueryCap(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	n := config.DefaultMaxQueryRows + 50
	if err := e.LoadCSV("rows", writeCSV(t, n)); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	var buf bytes.Buffer
	_, rowCount, err := e.QuerySQLToCSV(`SELECT id, value FROM "rows"`, &buf)
	if err != nil {
		t.Fatalf("QuerySQLToCSV (%d rows): %v", n, err)
	}
	if rowCount != n {
		t.Fatalf("rowCount = %d, want %d (export path must not use the chat cap)", rowCount, n)
	}
}

// TestQuerySQLToCSV_RespectsExportCap: the export path errors at its
// own cap, not the chat cap. A small export cap is injected via
// SetRowCaps so the test doesn't materialise a huge table.
func TestQuerySQLToCSV_RespectsExportCap(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	e.SetRowCaps(0, 500) // chat cap default, export cap 500
	if err := e.LoadCSV("rows", writeCSV(t, 600)); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	var buf bytes.Buffer
	_, _, err := e.QuerySQLToCSV(`SELECT id FROM "rows"`, &buf)
	if err == nil {
		t.Fatal("expected QuerySQLToCSV to error past the export cap")
	}
	if !strings.Contains(err.Error(), "exceeds 500 rows") {
		t.Errorf("expected 'exceeds 500 rows' in error, got: %v", err)
	}
}
