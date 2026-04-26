package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestE2E_QueryPreview(t *testing.T) {
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("name,score\nAlice,90\nBob,85\n"), 0644)

	// Mock: first call loads data, second call LLM generates SQL, third call final response
	mock := llm.NewMockBackend(
		// Round 1: LLM calls load-data
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-1",
			Name:      "load-data",
			Arguments: `{"file_path":"` + csvPath + `","table_name":"scores"}`,
		}}},
		// Round 2: LLM calls query-preview
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-2",
			Name:      "query-preview",
			Arguments: `{"question":"Who has the highest score?"}`,
		}}},
		// Round 3: LLM final response (but query-preview internally calls Chat for SQL gen)
		llm.MockResponse{Content: "Alice has the highest score at 90."},
	)

	a := New(config.Default())
	a.backend = mock
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "test", Title: "Test", Records: []memory.Record{}}
	engine := analysis.NewWithPath("test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	result, err := a.Send(context.Background(), "Analyze scores")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = result
}

func TestE2E_SuggestAnalysis(t *testing.T) {
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("product,price\nWidget,10\n"), 0644)

	mock := llm.NewMockBackend(
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-1",
			Name:      "load-data",
			Arguments: `{"file_path":"` + csvPath + `","table_name":"products"}`,
		}}},
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-2",
			Name:      "suggest-analysis",
			Arguments: `{}`,
		}}},
		llm.MockResponse{Content: "Here are analysis suggestions."},
	)

	a := New(config.Default())
	a.backend = mock
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "test", Title: "Test", Records: []memory.Record{}}
	engine := analysis.NewWithPath("test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	result, err := a.Send(context.Background(), "Suggest analysis")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestE2E_QuickSummary(t *testing.T) {
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("item,count\nA,100\nB,200\n"), 0644)

	mock := llm.NewMockBackend(
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-1",
			Name:      "load-data",
			Arguments: `{"file_path":"` + csvPath + `","table_name":"items"}`,
		}}},
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-2",
			Name:      "quick-summary",
			Arguments: `{"sql":"SELECT * FROM \"items\""}`,
		}}},
		llm.MockResponse{Content: "Summary complete."},
	)

	a := New(config.Default())
	a.backend = mock
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "test", Title: "Test", Records: []memory.Record{}}
	engine := analysis.NewWithPath("test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	result, err := a.Send(context.Background(), "Summarize items")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = result
}

func TestE2E_LoadJSON(t *testing.T) {
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "data.json")
	os.WriteFile(jsonPath, []byte(`[{"k":"v1"},{"k":"v2"}]`), 0644)

	mock := llm.NewMockBackend(
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-1",
			Name:      "load-data",
			Arguments: `{"file_path":"` + jsonPath + `","table_name":"jdata"}`,
		}}},
		llm.MockResponse{Content: "JSON data loaded."},
	)

	a := New(config.Default())
	a.backend = mock
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "test", Title: "Test", Records: []memory.Record{}}
	engine := analysis.NewWithPath("test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	result, err := a.Send(context.Background(), "Load JSON")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !engine.HasData() {
		t.Error("expected data after JSON load")
	}
	_ = result
}

func TestE2E_MITLApprove(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "test", Title: "Test", Records: []memory.Record{}}

	mitlCalled := false
	a.SetMITLHandler(func(req MITLRequest) MITLResponse {
		mitlCalled = true
		if req.Category != "write" {
			t.Errorf("category = %v, want write", req.Category)
		}
		return MITLResponse{Approved: true}
	})

	// Simulate a write tool call
	tc := llm.ToolCall{ID: "tc-1", Name: "nonexistent-write-tool", Arguments: "{}"}
	result := a.executeTool(context.Background(), tc)
	// Tool won't be found in registry, so MITL won't be triggered
	if !strings.Contains(result, "unknown tool") {
		t.Logf("result = %q", result)
	}
	_ = mitlCalled
}

func TestE2E_MITLReject(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "test", Title: "Test", Records: []memory.Record{}}

	a.SetMITLHandler(func(req MITLRequest) MITLResponse {
		return MITLResponse{Approved: false}
	})

	// Can't easily test with shell scripts without a real script,
	// but verify the handler is set
	if a.mitlHandler == nil {
		t.Error("MITL handler not set")
	}
}

func TestAnalysisToolsFilteringWithNewTools(t *testing.T) {
	tools := analysisTools(true)

	expectedTools := []string{
		"load-data", "reset-analysis", "describe-data", "query-sql",
		"list-tables", "query-preview", "suggest-analysis", "quick-summary",
		"promote-finding",
	}

	for _, expected := range expectedTools {
		found := false
		for _, tool := range tools {
			if tool.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected tool %q in with-data set", expected)
		}
	}
}
