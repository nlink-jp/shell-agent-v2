package analysis

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJSON(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "test.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()

	jsonPath := filepath.Join(t.TempDir(), "data.json")
	os.WriteFile(jsonPath, []byte(`[{"name":"Alice","age":30},{"name":"Bob","age":25}]`), 0644)

	if err := e.LoadJSON("people", jsonPath); err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}

	tables := e.Tables()
	if len(tables) != 1 {
		t.Fatalf("tables = %d, want 1", len(tables))
	}
	if tables[0].RowCount != 2 {
		t.Errorf("rows = %d, want 2", tables[0].RowCount)
	}
}

func TestLoadJSONL(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "test.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()

	jsonlPath := filepath.Join(t.TempDir(), "data.jsonl")
	os.WriteFile(jsonlPath, []byte("{\"x\":1}\n{\"x\":2}\n{\"x\":3}\n"), 0644)

	if err := e.LoadJSONL("nums", jsonlPath); err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}

	tables := e.Tables()
	if len(tables) != 1 {
		t.Fatalf("tables = %d, want 1", len(tables))
	}
	if tables[0].RowCount != 3 {
		t.Errorf("rows = %d, want 3", tables[0].RowCount)
	}
}

func TestLoadFileDispatch(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "test.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()

	tmpDir := t.TempDir()

	// CSV
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("a\n1\n"), 0644)
	if err := e.LoadFile("csv_table", csvPath); err != nil {
		t.Errorf("LoadFile CSV: %v", err)
	}

	// JSON
	jsonPath := filepath.Join(tmpDir, "data.json")
	os.WriteFile(jsonPath, []byte(`[{"b":2}]`), 0644)
	if err := e.LoadFile("json_table", jsonPath); err != nil {
		t.Errorf("LoadFile JSON: %v", err)
	}

	// JSONL
	jsonlPath := filepath.Join(tmpDir, "data.jsonl")
	os.WriteFile(jsonlPath, []byte("{\"c\":3}\n"), 0644)
	if err := e.LoadFile("jsonl_table", jsonlPath); err != nil {
		t.Errorf("LoadFile JSONL: %v", err)
	}

	// Unsupported
	xmlPath := filepath.Join(tmpDir, "data.xml")
	os.WriteFile(xmlPath, []byte("<data/>"), 0644)
	if err := e.LoadFile("xml_table", xmlPath); err == nil {
		t.Error("LoadFile XML should fail")
	}

	if len(e.Tables()) != 3 {
		t.Errorf("tables = %d, want 3", len(e.Tables()))
	}
}

func TestSchema(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "test.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()

	csvPath := filepath.Join(t.TempDir(), "test.csv")
	os.WriteFile(csvPath, []byte("name,age\nAlice,30\n"), 0644)
	e.LoadFile("people", csvPath)
	e.SetTableDescription("people", "User data")

	schema := e.Schema()
	if schema == "" {
		t.Error("schema is empty")
	}
	if !contains(schema, "people") {
		t.Error("schema missing table name")
	}
	if !contains(schema, "User data") {
		t.Error("schema missing description")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
