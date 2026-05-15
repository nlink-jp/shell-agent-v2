package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestToolSaveQuery_HappyPath(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	if _, err := a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`); err != nil {
		t.Fatalf("toolLoadData: %v", err)
	}

	args := `{"sql":"SELECT * FROM \"people\" WHERE age >= 30","name":"adults"}`
	result, err := a.toolSaveQuery(args)
	if err != nil {
		t.Fatalf("toolSaveQuery: %v", err)
	}
	if !strings.Contains(result, "adults") {
		t.Errorf("result missing derived table name: %s", result)
	}
	if !strings.Contains(result, "Rows: 1") {
		t.Errorf("result missing row count (expected 1 row for age>=30): %s", result)
	}

	// The derived table must surface in list-tables alongside the loaded one.
	listed, err := a.toolListTables()
	if err != nil {
		t.Fatalf("toolListTables: %v", err)
	}
	if !strings.Contains(listed, "adults") {
		t.Errorf("list-tables missing the derived table: %s", listed)
	}
	if !strings.Contains(listed, "people") {
		t.Errorf("list-tables missing the original table: %s", listed)
	}
}

func TestToolSaveQuery_NameCollisionSuggestions(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	// Collide with the loaded table name.
	args := `{"sql":"SELECT * FROM \"people\"","name":"people"}`
	_, err := a.toolSaveQuery(args)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	msg := err.Error()
	for _, suffix := range []string{"_v2", "_filtered", "_derived"} {
		if !strings.Contains(msg, suffix) {
			t.Errorf("collision error missing %q suggestion; got: %s", suffix, msg)
		}
	}
}

func TestToolSaveQuery_NonSelectRejected(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	args := `{"sql":"DROP TABLE \"people\"","name":"derived"}`
	_, err := a.toolSaveQuery(args)
	if err == nil {
		t.Fatal("expected error for non-SELECT body, got nil")
	}
	if !strings.Contains(err.Error(), "SELECT") {
		t.Errorf("error should mention SELECT requirement; got: %v", err)
	}
}

func TestToolSaveQuery_InvalidIdentifier(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	args := `{"sql":"SELECT * FROM \"people\"","name":"bad name with spaces"}`
	_, err := a.toolSaveQuery(args)
	if err == nil {
		t.Fatal("expected identifier error, got nil")
	}
	if !strings.Contains(err.Error(), "alphanumeric") {
		t.Errorf("error should mention identifier requirement; got: %v", err)
	}
}

func TestToolSaveQuery_DuckDBErrorPassThrough(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	args := `{"sql":"SELECT no_such_column FROM \"people\"","name":"bad_filter"}`
	_, err := a.toolSaveQuery(args)
	if err == nil {
		t.Fatal("expected DuckDB error, got nil")
	}
	if !strings.Contains(err.Error(), "save query:") {
		t.Errorf("error should be wrapped with 'save query:' prefix; got: %v", err)
	}
}

func TestToolSaveQuery_DescriptionPlumbing(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	args := `{"sql":"SELECT * FROM \"people\" WHERE age >= 30","name":"adults","description":"30 and over"}`
	if _, err := a.toolSaveQuery(args); err != nil {
		t.Fatalf("toolSaveQuery: %v", err)
	}

	// describe-data on the derived table should surface the description.
	describe, err := a.toolDescribeData(`{"table_name":"adults"}`)
	if err != nil {
		t.Fatalf("toolDescribeData: %v", err)
	}
	if !strings.Contains(describe, "30 and over") {
		t.Errorf("describe-data missing description: %s", describe)
	}
}

func TestToolSaveQuery_MissingRequiredFields(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)

	// Missing sql
	_, err := a.toolSaveQuery(`{"name":"x"}`)
	if err == nil {
		t.Error("expected error for missing sql, got nil")
	}
	// Missing name
	_, err = a.toolSaveQuery(`{"sql":"SELECT 1"}`)
	if err == nil {
		t.Error("expected error for missing name, got nil")
	}
}
