package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func setupAgentWithAnalysis(t *testing.T) (*Agent, string) {
	t.Helper()
	tmpDir := t.TempDir()

	a := New(config.Default())
	a.session = &memory.Session{ID: "test", Title: "Test Session"}

	engine := &analysis.Engine{}
	// Use reflection-free approach: create via New then override path
	engine = analysis.New("test")
	// We need to set the db path to temp dir — use the engine directly
	// Actually, analysis.New uses memory.SessionDir which depends on config.DataDir
	// For testing, let's create a standalone engine
	engine = newTestEngine(t, tmpDir)
	a.analysis = engine

	return a, tmpDir
}

func newTestEngine(t *testing.T, dir string) *analysis.Engine {
	t.Helper()
	// Create a CSV for testing
	csvPath := filepath.Join(dir, "test.csv")
	os.WriteFile(csvPath, []byte("name,age,city\nAlice,30,Tokyo\nBob,25,Osaka\n"), 0644)
	return analysis.NewWithPath("test-session", filepath.Join(dir, "test.duckdb"))
}

func TestAnalysisToolsFiltering(t *testing.T) {
	// No data: load-data, reset-analysis, create-report
	tools := analysisTools(false)
	if len(tools) != 3 {
		t.Errorf("no-data tools count = %d, want 3", len(tools))
	}

	// With data: full set
	tools = analysisTools(true)
	if len(tools) < 5 {
		t.Errorf("with-data tools count = %d, want >= 5", len(tools))
	}

	// Verify promote-finding is in the with-data set
	found := false
	for _, tool := range tools {
		if tool.Name == "promote-finding" {
			found = true
			break
		}
	}
	if !found {
		t.Error("promote-finding not in with-data tools")
	}
}

func TestToolLoadData(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	args := `{"file_path":"` + csvPath + `","table_name":"people"}`
	result, err := a.toolLoadData(args)
	if err != nil {
		t.Fatalf("toolLoadData: %v", err)
	}
	if !strings.Contains(result, "people") {
		t.Errorf("result missing table name: %s", result)
	}
	if !strings.Contains(result, "2 rows") {
		t.Errorf("result missing row count: %s", result)
	}
}

func TestToolQuerySQL(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	result, err := a.toolQuerySQL(`{"sql":"SELECT name FROM \"people\" ORDER BY name"}`)
	if err != nil {
		t.Fatalf("toolQuerySQL: %v", err)
	}
	if !strings.Contains(result, "Alice") {
		t.Errorf("result missing Alice: %s", result)
	}
}

func TestToolDescribeData(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	// Describe
	result, err := a.toolDescribeData(`{"table_name":"people"}`)
	if err != nil {
		t.Fatalf("toolDescribeData: %v", err)
	}
	if !strings.Contains(result, "people") {
		t.Errorf("result missing table name: %s", result)
	}

	// Set description
	result, err = a.toolDescribeData(`{"table_name":"people","set_description":"User data"}`)
	if err != nil {
		t.Fatalf("toolDescribeData with set: %v", err)
	}
	if !strings.Contains(result, "User data") {
		t.Errorf("result missing description: %s", result)
	}
}

func TestToolListTables(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	result, err := a.toolListTables()
	if err != nil {
		t.Fatalf("toolListTables: %v", err)
	}
	if !strings.Contains(result, "people") {
		t.Errorf("result missing table: %s", result)
	}
}

func TestToolResetAnalysis(t *testing.T) {
	a, tmpDir := setupAgentWithAnalysis(t)

	csvPath := filepath.Join(tmpDir, "test.csv")
	a.toolLoadData(`{"file_path":"` + csvPath + `","table_name":"people"}`)

	result, err := a.toolResetAnalysis()
	if err != nil {
		t.Fatalf("toolResetAnalysis: %v", err)
	}
	if !strings.Contains(result, "cleared") {
		t.Errorf("unexpected result: %s", result)
	}
	if a.analysis.HasData() {
		t.Error("expected no data after reset")
	}
}

func TestToolPromoteFinding(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)

	result, err := a.toolPromoteFinding(`{"content":"Sales peak in April","tags":["sales"]}`)
	if err != nil {
		t.Fatalf("toolPromoteFinding: %v", err)
	}
	if !strings.Contains(result, "Sales peak in April") {
		t.Errorf("result missing content: %s", result)
	}

	all := a.findings.All()
	if len(all) == 0 {
		t.Error("expected finding in store")
	}
}
