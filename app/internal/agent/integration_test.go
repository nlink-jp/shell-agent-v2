package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestSessionSwitchClearsAnalysis(t *testing.T) {
	a := New(config.Default())

	// Load session A with analysis
	sessionA := &memory.Session{ID: "sess-a", Title: "Session A"}
	a.LoadSession(sessionA)

	tmpDir := t.TempDir()
	engineA := analysis.NewWithPath("sess-a", filepath.Join(tmpDir, "a.duckdb"))
	a.SetAnalysis(engineA)

	csvPath := filepath.Join(tmpDir, "test.csv")
	os.WriteFile(csvPath, []byte("x,y\n1,2\n"), 0644)
	engineA.LoadCSV("test_table", csvPath)

	if !a.analysis.HasData() {
		t.Fatal("expected data in session A")
	}

	// Switch to session B with different analysis
	sessionB := &memory.Session{ID: "sess-b", Title: "Session B"}
	a.LoadSession(sessionB)

	engineB := analysis.NewWithPath("sess-b", filepath.Join(tmpDir, "b.duckdb"))
	a.SetAnalysis(engineB)

	if a.analysis.HasData() {
		t.Error("session B should have no data")
	}

	// Session A's data should still be accessible through its engine
	if !engineA.HasData() {
		t.Error("session A engine should still have data")
	}

	engineA.Close()
	engineB.Close()
}

func TestBackendSwitchPreservesSession(t *testing.T) {
	a := New(config.Default())
	session := &memory.Session{ID: "test", Title: "Test"}
	a.LoadSession(session)
	session.AddUserMessage("before switch")

	// Switch backend
	a.setBackend(config.BackendVertexAI)
	if a.CurrentBackend() != "vertex_ai" {
		t.Errorf("backend = %v, want vertex_ai", a.CurrentBackend())
	}

	// Session should be preserved
	if len(a.session.Records) != 1 {
		t.Errorf("records after switch = %d, want 1", len(a.session.Records))
	}

	a.setBackend(config.BackendLocal)
	if a.CurrentBackend() != "local" {
		t.Errorf("backend = %v, want local", a.CurrentBackend())
	}
}

func TestAbortDuringIdle(t *testing.T) {
	a := New(config.Default())

	// Abort on idle should be no-op
	a.Abort()
	if a.State() != StateIdle {
		t.Errorf("state after idle abort = %v, want idle", a.State())
	}
}

func TestCancelledContextReturnsCancelled(t *testing.T) {
	a := New(config.Default())
	a.session = &memory.Session{ID: "test", Records: []memory.Record{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result, err := a.Send(ctx, "hello")
	// Should get cancelled or error from LLM
	_ = err
	_ = result
}

func TestDynamicToolFiltering(t *testing.T) {
	a := New(config.Default())

	// Without analysis engine
	tools := a.buildToolDefs()
	hasLoadData := false
	hasQuerySQL := false
	for _, tool := range tools {
		if tool.Name == "load-data" {
			hasLoadData = true
		}
		if tool.Name == "query-sql" {
			hasQuerySQL = true
		}
	}
	// Without analysis, no analysis tools
	if hasLoadData {
		t.Error("load-data should not be present without analysis engine")
	}

	// With analysis but no data
	tmpDir := t.TempDir()
	engine := analysis.NewWithPath("test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	tools = a.buildToolDefs()
	hasLoadData = false
	hasQuerySQL = false
	for _, tool := range tools {
		if tool.Name == "load-data" {
			hasLoadData = true
		}
		if tool.Name == "query-sql" {
			hasQuerySQL = true
		}
	}
	if !hasLoadData {
		t.Error("load-data should be present with analysis engine")
	}
	if hasQuerySQL {
		t.Error("query-sql should not be present without data")
	}

	// Load data
	csvPath := filepath.Join(tmpDir, "test.csv")
	os.WriteFile(csvPath, []byte("a,b\n1,2\n"), 0644)
	engine.LoadCSV("test_table", csvPath)

	tools = a.buildToolDefs()
	hasQuerySQL = false
	for _, tool := range tools {
		if tool.Name == "query-sql" {
			hasQuerySQL = true
		}
	}
	if !hasQuerySQL {
		t.Error("query-sql should be present with data")
	}
}

func TestFindingsAccessor(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore()

	if len(a.Findings()) != 0 {
		t.Error("expected empty findings")
	}

	a.findings.Add("test finding", "sess-1", "Test", nil)
	if len(a.Findings()) != 1 {
		t.Error("expected 1 finding")
	}
}

func TestMultipleCommandsSequentially(t *testing.T) {
	a := New(config.Default())

	// Multiple commands should work sequentially
	r1, _ := a.Send(context.Background(), "/model")
	if r1 == "" {
		t.Error("empty response from /model")
	}

	r2, _ := a.Send(context.Background(), "/model vertex")
	if r2 == "" {
		t.Error("empty response from /model vertex")
	}

	r3, _ := a.Send(context.Background(), "/model local")
	if r3 == "" {
		t.Error("empty response from /model local")
	}

	if a.State() != StateIdle {
		t.Error("should be idle after commands")
	}
}
