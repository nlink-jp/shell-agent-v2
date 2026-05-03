package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

func setupAgentWithAnalysis(t *testing.T) (*Agent, string) {
	t.Helper()
	tmpDir := t.TempDir()

	a := New(config.Default())
	a.session = &memory.Session{ID: "test", Title: "Test Session"}
	// v0.2.0: per-session findings store. setupAgentWithAnalysis
	// is used by tests that don't go through LoadSession, so
	// wire it up directly.
	a.findings = findings.NewStore(a.session.ID)

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
	// Legacy mode, no data: load-data, reset-analysis, create-report,
	// list-objects, get-object, register-object = 6.
	tools := analysisTools(false, true)
	if len(tools) != 6 {
		t.Errorf("legacy no-data tools count = %d, want 6", len(tools))
	}

	// Legacy mode, with data: full set.
	tools = analysisTools(true, true)
	if len(tools) <= 6 {
		t.Errorf("legacy with-data tools count = %d, want > 6", len(tools))
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
// the pre-v0.1.21 split. The unconditional set grew from 5 to 6
// in v0.1.25 with the addition of register-object
// (docs/en/work-dir-shell-bridge.md).
func TestAnalysisTools_HideFlagRestoresLegacyBehaviour(t *testing.T) {
	short := analysisTools(false, true)
	full := analysisTools(true, true)
	if len(short) != 6 {
		t.Errorf("hide-flag, no data: %d tools, want 6", len(short))
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

// --- register-object (docs/en/work-dir-shell-bridge.md) ---

// setupAgentWithWorkDir builds an Agent with a fresh objstore and
// session whose work dir is a tempdir we control, so we can drop a
// file in and call toolRegisterObject against it.
func setupAgentWithWorkDir(t *testing.T) (*Agent, string) {
	t.Helper()

	// Redirect DataDir so memory.SessionDir resolves under the tempdir.
	dataDir := t.TempDir()
	t.Setenv("HOME", dataDir)

	a := New(config.Default())
	a.session = &memory.Session{ID: "regobj-test", Title: "regobj test"}
	a.objects = objstore.NewStoreAt(filepath.Join(dataDir, "objects"))

	workDir := a.sessionWorkDir()
	if workDir == "" {
		t.Fatal("sessionWorkDir empty")
	}
	if err := os.MkdirAll(workDir, 0700); err != nil {
		t.Fatal(err)
	}
	return a, workDir
}

func TestToolRegisterObject_HappyPath(t *testing.T) {
	a, workDir := setupAgentWithWorkDir(t)

	// Drop a tiny PNG-like file in /work.
	src := filepath.Join(workDir, "sunset.png")
	if err := os.WriteFile(src, []byte("\x89PNG\r\n\x1a\nfake-image-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	out, status := a.toolRegisterObject(`{"path":"sunset.png","name":"Sunset over the bay"}`)
	if status != ActivityStatusSuccess {
		t.Fatalf("status = %v, want success; out=%s", status, out)
	}
	if !strings.HasPrefix(out, "registered as object ") {
		t.Errorf("out = %q, want it to start with 'registered as object '", out)
	}
	// Object should now be in the store with the human-readable name.
	all := a.objects.All()
	if len(all) != 1 {
		t.Fatalf("objstore count = %d, want 1", len(all))
	}
	if all[0].OrigName != "Sunset over the bay" {
		t.Errorf("OrigName = %q, want 'Sunset over the bay'", all[0].OrigName)
	}
}

func TestToolRegisterObject_RejectsTraversal(t *testing.T) {
	a, _ := setupAgentWithWorkDir(t)

	for _, evil := range []string{"../etc/passwd", "../../something", "/etc/passwd"} {
		t.Run(evil, func(t *testing.T) {
			out, status := a.toolRegisterObject(`{"path":"` + evil + `","name":"x"}`)
			if status == ActivityStatusSuccess {
				t.Errorf("traversal path %q accepted; out=%s", evil, out)
			}
		})
	}
}

func TestToolRegisterObject_RejectsMissingPath(t *testing.T) {
	a, _ := setupAgentWithWorkDir(t)

	out, status := a.toolRegisterObject(`{"name":"x"}`)
	if status == ActivityStatusSuccess {
		t.Errorf("missing path accepted; out=%s", out)
	}
	if !strings.Contains(out, "path") {
		t.Errorf("error should mention 'path': %s", out)
	}
}

// TestToolRegisterObject_DefaultsName: when name is omitted, falls
// back to filepath.Base(path).
func TestToolRegisterObject_DefaultsName(t *testing.T) {
	a, workDir := setupAgentWithWorkDir(t)
	src := filepath.Join(workDir, "report.md")
	if err := os.WriteFile(src, []byte("# title"), 0644); err != nil {
		t.Fatal(err)
	}
	_, status := a.toolRegisterObject(`{"path":"report.md"}`)
	if status != ActivityStatusSuccess {
		t.Fatalf("status = %v", status)
	}
	all := a.objects.All()
	if len(all) != 1 {
		t.Fatalf("count = %d", len(all))
	}
	if all[0].OrigName != "report.md" {
		t.Errorf("OrigName = %q, want default 'report.md'", all[0].OrigName)
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
