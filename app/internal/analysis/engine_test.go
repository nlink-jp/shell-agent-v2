package analysis

import (
	"os"
	"path/filepath"
	"testing"
)

func setupTestEngine(t *testing.T) (*Engine, func()) {
	t.Helper()
	tmpDir := t.TempDir()

	// Override the dbPath directly
	e := &Engine{
		sessionID: "test-session",
		dbPath:    filepath.Join(tmpDir, "test.duckdb"),
		tables:    make(map[string]*TableMeta),
	}

	return e, func() {
		e.Close()
	}
}

func TestOpenClose(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()

	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !e.IsOpen() {
		t.Error("expected IsOpen = true")
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if e.IsOpen() {
		t.Error("expected IsOpen = false after close")
	}
}

func TestLoadCSVAndQuery(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()

	// Create a temporary CSV
	csvPath := filepath.Join(t.TempDir(), "test.csv")
	csvContent := "name,age,city\nAlice,30,Tokyo\nBob,25,Osaka\nCharlie,35,Kyoto\n"
	if err := os.WriteFile(csvPath, []byte(csvContent), 0644); err != nil {
		t.Fatalf("write CSV: %v", err)
	}

	if err := e.LoadCSV("people", csvPath); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	if !e.HasData() {
		t.Error("expected HasData = true after load")
	}

	tables := e.Tables()
	if len(tables) != 1 {
		t.Fatalf("tables count = %d, want 1", len(tables))
	}
	if tables[0].Name != "people" {
		t.Errorf("table name = %v, want people", tables[0].Name)
	}
	if tables[0].RowCount != 3 {
		t.Errorf("row count = %d, want 3", tables[0].RowCount)
	}
	if len(tables[0].Columns) != 3 {
		t.Errorf("column count = %d, want 3", len(tables[0].Columns))
	}

	// Query
	results, err := e.QuerySQL("SELECT name, age FROM \"people\" ORDER BY age")
	if err != nil {
		t.Fatalf("QuerySQL: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results count = %d, want 3", len(results))
	}
}

func TestQuerySQLRejectsWrite(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	e.Open()

	dangerous := []string{
		"DROP TABLE foo",
		"INSERT INTO foo VALUES (1)",
		"DELETE FROM foo",
		"CREATE TABLE foo (id INT)",
		"UPDATE foo SET id = 1",
	}

	for _, q := range dangerous {
		_, err := e.QuerySQL(q)
		if err == nil {
			t.Errorf("expected error for %q", q)
		}
	}
}

func TestSetTableDescription(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()

	csvPath := filepath.Join(t.TempDir(), "test.csv")
	os.WriteFile(csvPath, []byte("a,b\n1,2\n"), 0644)
	e.LoadCSV("test_table", csvPath)

	if err := e.SetTableDescription("test_table", "A test table"); err != nil {
		t.Fatalf("SetTableDescription: %v", err)
	}

	// Verify description persists after rebuild
	e.tables = make(map[string]*TableMeta)
	e.rebuildTableMeta()

	tables := e.Tables()
	if len(tables) != 1 {
		t.Fatalf("tables count = %d, want 1", len(tables))
	}
	if tables[0].Description != "A test table" {
		t.Errorf("description = %q, want 'A test table'", tables[0].Description)
	}
}

func TestReset(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()

	csvPath := filepath.Join(t.TempDir(), "test.csv")
	os.WriteFile(csvPath, []byte("a,b\n1,2\n"), 0644)
	e.LoadCSV("test_table", csvPath)

	if !e.HasData() {
		t.Fatal("expected data before reset")
	}

	if err := e.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if e.HasData() {
		t.Error("expected no data after reset")
	}
}

func TestDBPathPersistence(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()

	csvPath := filepath.Join(t.TempDir(), "test.csv")
	os.WriteFile(csvPath, []byte("x,y\n10,20\n"), 0644)
	e.LoadCSV("persist_test", csvPath)
	e.SetTableDescription("persist_test", "Persistence test")
	dbPath := e.DBPath()
	e.Close()

	// Reopen from same path
	e2 := &Engine{
		sessionID: "test-session",
		dbPath:    dbPath,
		tables:    make(map[string]*TableMeta),
	}
	defer e2.Close()

	if err := e2.Open(); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	tables := e2.Tables()
	if len(tables) != 1 {
		t.Fatalf("reopened tables count = %d, want 1", len(tables))
	}
	if tables[0].Description != "Persistence test" {
		t.Errorf("reopened description = %q, want 'Persistence test'", tables[0].Description)
	}
}
