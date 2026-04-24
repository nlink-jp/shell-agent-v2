package analysis

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuerySQLOnEmptyDB(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "empty.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	e.Open()
	defer e.Close()

	results, err := e.QuerySQL("SELECT 1 AS n")
	if err != nil {
		t.Fatalf("error on empty DB: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
}

func TestLoadCSVOverwrite(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "overwrite.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()

	csv1 := filepath.Join(t.TempDir(), "v1.csv")
	os.WriteFile(csv1, []byte("a\n1\n2\n"), 0644)
	e.LoadCSV("data", csv1)

	csv2 := filepath.Join(t.TempDir(), "v2.csv")
	os.WriteFile(csv2, []byte("a\n10\n20\n30\n"), 0644)
	e.LoadCSV("data", csv2)

	tables := e.Tables()
	if len(tables) != 1 {
		t.Fatalf("tables = %d, want 1", len(tables))
	}
	if tables[0].RowCount != 3 {
		t.Errorf("row count = %d, want 3 (overwritten)", tables[0].RowCount)
	}
}

func TestResetOnEmptyDB(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "reset.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	e.Open()
	defer e.Close()

	// Reset on empty should not error
	if err := e.Reset(); err != nil {
		t.Errorf("reset on empty: %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "close.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	e.Open()

	if err := e.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestSetDescriptionOnClosedDB(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "closed.duckdb"),
		tables:    make(map[string]*TableMeta),
	}

	err := e.SetTableDescription("test", "desc")
	if err == nil {
		t.Error("expected error on closed DB")
	}
}

func TestIsReadOnlySQL(t *testing.T) {
	readOnly := []string{
		"SELECT * FROM t",
		"  select count(*) from t",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
	}
	for _, q := range readOnly {
		if !isReadOnlySQL(q) {
			t.Errorf("expected read-only: %q", q)
		}
	}

	notReadOnly := []string{
		"INSERT INTO t VALUES (1)",
		"  UPDATE t SET x = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"CREATE TABLE t (id INT)",
		"ALTER TABLE t ADD COLUMN x INT",
		"LOAD 'ext'",
		"INSTALL 'ext'",
	}
	for _, q := range notReadOnly {
		if isReadOnlySQL(q) {
			t.Errorf("expected not read-only: %q", q)
		}
	}
}
