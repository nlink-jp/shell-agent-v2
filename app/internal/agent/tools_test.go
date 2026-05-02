package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
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

// TestAnalysisToolsFiltering covers the legacy hide-until-data-loaded
// flag (default OFF in v0.1.21+; ON restores the pre-v0.1.21 split).
// See docs/en/agent-tool-visibility.md.
func TestAnalysisToolsFiltering(t *testing.T) {
	// Legacy mode, no data: only the load-data half (5 tools).
	tools := analysisTools(false, true)
	if len(tools) != 5 {
		t.Errorf("legacy no-data tools count = %d, want 5", len(tools))
	}

	// Legacy mode, with data: full set.
	tools = analysisTools(true, true)
	if len(tools) < 5 {
		t.Errorf("legacy with-data tools count = %d, want >= 5", len(tools))
	}
	if !containsTool(tools, "promote-finding") {
		t.Error("promote-finding not in legacy with-data tools")
	}
}

// TestAnalysisTools_FullSetByDefault_AllowsPlanning pins the new
// v0.1.21 default: data-dependent tools (query-sql, analyze-data,
// etc.) are exposed every round so the LLM can plan a load-then-
// query workflow up front.
func TestAnalysisTools_FullSetByDefault_AllowsPlanning(t *testing.T) {
	// Default mode (hideUntilDataLoaded=false), no data: still full set.
	tools := analysisTools(false, false)
	for _, want := range []string{"query-sql", "describe-data", "analyze-data", "promote-finding", "list-tables"} {
		if !containsTool(tools, want) {
			t.Errorf("default-mode no-data tools missing %q (full set should be exposed)", want)
		}
	}
}

// TestAnalysisTools_HideFlagRestoresLegacyBehaviour: the opt-in
// flag (cfg.Tools.HideAnalysisToolsUntilDataLoaded=true) restores
// the pre-v0.1.21 5/13 split.
func TestAnalysisTools_HideFlagRestoresLegacyBehaviour(t *testing.T) {
	short := analysisTools(false, true)
	full := analysisTools(true, true)
	if len(short) != 5 {
		t.Errorf("hide-flag, no data: %d tools, want 5", len(short))
	}
	if len(full) <= len(short) {
		t.Errorf("hide-flag, with-data tools (%d) should be more than no-data (%d)", len(full), len(short))
	}
	if containsTool(short, "query-sql") {
		t.Error("hide-flag no-data should NOT contain query-sql")
	}
	if !containsTool(full, "query-sql") {
		t.Error("hide-flag with-data should contain query-sql")
	}
}

func containsTool(tools []llm.ToolDef, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
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
