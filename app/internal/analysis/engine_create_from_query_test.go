package analysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupEngineWithCSV is a per-test helper: writes a small CSV
// to tmpDir, loads it into a fresh engine under the given table
// name, returns the engine and a cleanup func.
func setupEngineWithCSV(t *testing.T, tableName string) (*Engine, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	body := "id,status,amount\n" +
		"1,ok,10\n" +
		"2,failed,20\n" +
		"3,ok,30\n" +
		"4,failed,40\n" +
		"5,ok,50\n"
	if err := os.WriteFile(csvPath, []byte(body), 0644); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	e := &Engine{
		sessionID: "test-session",
		dbPath:    filepath.Join(tmpDir, "test.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	if err := e.LoadCSV(tableName, csvPath); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}
	return e, func() { _ = e.Close() }
}

func TestCreateFromQuery_HappyPath(t *testing.T) {
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	meta, err := e.CreateFromQuery("failed_events",
		`SELECT * FROM "events" WHERE status = 'failed'`, "")
	if err != nil {
		t.Fatalf("CreateFromQuery: %v", err)
	}
	if meta == nil {
		t.Fatal("meta is nil")
	}
	if meta.Name != "failed_events" {
		t.Errorf("Name = %q, want failed_events", meta.Name)
	}
	if meta.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", meta.RowCount)
	}
	if len(meta.Columns) != 3 {
		t.Errorf("Columns = %v, want 3 columns", meta.Columns)
	}

	tables := e.Tables()
	found := false
	for _, t := range tables {
		if t.Name == "failed_events" {
			found = true
			break
		}
	}
	if !found {
		t.Error("failed_events not in Tables() after CreateFromQuery")
	}
}

func TestCreateFromQuery_InvalidIdentifier(t *testing.T) {
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	cases := []string{
		"",            // empty
		"123",         // starts with digit
		"foo bar",     // contains space
		"drop-table",  // contains hyphen
		"select",      // SQL keyword — still matches the regex (lowercase letters), so this one IS allowed; replaced below
		"a.b",         // dotted
		"a;drop",      // injection attempt
	}
	// "select" technically matches the regex; DuckDB will fail at DDL time
	// for SQL-reserved bare identifiers. The regex's job is just to prevent
	// embedded whitespace, punctuation, and identifier-injection vectors.
	// Replace it with a case the regex actually catches.
	cases[4] = ""

	for _, badName := range cases {
		_, err := e.CreateFromQuery(badName, `SELECT * FROM "events"`, "")
		if err == nil {
			t.Errorf("expected error for name %q, got nil", badName)
			continue
		}
		if !strings.Contains(err.Error(), "alphanumeric") {
			t.Errorf("error for %q should mention alphanumeric requirement; got: %v", badName, err)
		}
	}
}

func TestCreateFromQuery_NonSelectRejected(t *testing.T) {
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	cases := []string{
		`INSERT INTO "events" VALUES (99,'x',0)`,
		`UPDATE "events" SET amount = 0`,
		`DELETE FROM "events"`,
		`DROP TABLE "events"`,
		`CREATE TABLE x AS SELECT 1`,
		`ALTER TABLE "events" ADD COLUMN x INT`,
	}
	for _, bad := range cases {
		_, err := e.CreateFromQuery("derived", bad, "")
		if err == nil {
			t.Errorf("expected error for non-SELECT body %q, got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), "SELECT") {
			t.Errorf("error for %q should mention SELECT requirement; got: %v", bad, err)
		}
	}
}

func TestCreateFromQuery_NameCollision(t *testing.T) {
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	// "events" is already a loaded table.
	_, err := e.CreateFromQuery("events",
		`SELECT * FROM "events" WHERE 1=1`, "")
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	msg := err.Error()
	// Verify the suggestion suffixes are all present in the error.
	for _, suffix := range []string{"_v2", "_filtered", "_derived"} {
		if !strings.Contains(msg, suffix) {
			t.Errorf("collision error missing %q suggestion; got: %s", suffix, msg)
		}
	}
}

func TestCreateFromQuery_DuckDBErrorPassThrough(t *testing.T) {
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	// Reference a column that doesn't exist.
	_, err := e.CreateFromQuery("bad_filter",
		`SELECT no_such_column FROM "events"`, "")
	if err == nil {
		t.Fatal("expected DuckDB error for unknown column, got nil")
	}
	if !strings.Contains(err.Error(), "save query:") {
		t.Errorf("error should be wrapped with 'save query:' prefix; got: %v", err)
	}
}

func TestCreateFromQuery_DescriptionPlumbing(t *testing.T) {
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	meta, err := e.CreateFromQuery("annotated",
		`SELECT * FROM "events" WHERE status = 'ok'`,
		"Successful events only")
	if err != nil {
		t.Fatalf("CreateFromQuery: %v", err)
	}
	if meta.Description != "Successful events only" {
		t.Errorf("Description = %q, want 'Successful events only'", meta.Description)
	}

	// Re-fetch via Tables() to confirm the description is also
	// reflected in the engine's persisted meta map.
	tables := e.Tables()
	for _, t2 := range tables {
		if t2.Name == "annotated" {
			if t2.Description != "Successful events only" {
				t.Errorf("Tables() Description = %q, want 'Successful events only'", t2.Description)
			}
			return
		}
	}
	t.Error("annotated not found in Tables()")
}

func TestCreateFromQuery_QuotedIdentifiersInBody(t *testing.T) {
	// Pin the contract that quoted-identifier SELECT bodies pass
	// through unchanged — the regex only constrains the new
	// table name, not what appears inside the SELECT.
	e, cleanup := setupEngineWithCSV(t, "events")
	defer cleanup()

	_, err := e.CreateFromQuery("by_status",
		`SELECT status, COUNT(*) AS n FROM "events" GROUP BY status`, "")
	if err != nil {
		t.Fatalf("CreateFromQuery: %v", err)
	}
}
