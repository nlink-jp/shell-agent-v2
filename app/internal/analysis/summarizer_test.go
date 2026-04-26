package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// mockLLM returns predefined responses for testing.
type mockLLM struct {
	calls    int
	response func(int) string
}

func (m *mockLLM) Chat(_ context.Context, _, _ string) (string, error) {
	m.calls++
	return m.response(m.calls), nil
}

func TestSummarizerSingleWindow(t *testing.T) {
	mock := &mockLLM{
		response: func(_ int) string {
			return `{"summary":"Found 3 products","new_findings":[{"description":"Widget A is top seller","severity":"medium","evidence":"25 units in Jan"}]}`
		},
	}

	s := NewSummarizer(mock, "Table: sales (5 rows)\n  Columns: product, amount", DefaultSummarizerConfig())
	rows := []string{
		`{"product":"Widget A","amount":100}`,
		`{"product":"Widget B","amount":200}`,
		`{"product":"Gadget X","amount":50}`,
	}

	result, err := s.Analyze(context.Background(), "Find top sellers", rows, nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if result.Summary != "Found 3 products" {
		t.Errorf("summary = %q", result.Summary)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(result.Findings))
	}
	if result.Findings[0].Severity != "medium" {
		t.Errorf("severity = %q", result.Findings[0].Severity)
	}
	if result.Windows != 1 {
		t.Errorf("windows = %d, want 1", result.Windows)
	}
	if mock.calls != 1 {
		t.Errorf("LLM calls = %d, want 1", mock.calls)
	}
}

func TestSummarizerMultipleWindows(t *testing.T) {
	cfg := SummarizerConfig{
		MaxRecordsPerWindow: 3,
		OverlapRatio:        0.0,
		MaxFindings:         50,
	}

	mock := &mockLLM{
		response: func(call int) string {
			return fmt.Sprintf(`{"summary":"Window %d analyzed","new_findings":[{"description":"Finding from window %d","severity":"info","evidence":"data"}]}`, call, call)
		},
	}

	s := NewSummarizer(mock, "Table: test", cfg)
	rows := make([]string, 9) // 3 windows of 3
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":%d}`, i)
	}

	var progressCalls []int
	result, err := s.Analyze(context.Background(), "test", rows, func(idx, total int) {
		progressCalls = append(progressCalls, idx)
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if result.Windows != 3 {
		t.Errorf("windows = %d, want 3", result.Windows)
	}
	if mock.calls != 3 {
		t.Errorf("LLM calls = %d, want 3", mock.calls)
	}
	if len(result.Findings) != 3 {
		t.Errorf("findings = %d, want 3", len(result.Findings))
	}
	if len(progressCalls) != 3 {
		t.Errorf("progress calls = %d, want 3", len(progressCalls))
	}
}

func TestEvictFindings(t *testing.T) {
	findings := []Finding{
		{Description: "critical1", Severity: "critical"},
		{Description: "info1", Severity: "info"},
		{Description: "info2", Severity: "info"},
		{Description: "high1", Severity: "high"},
		{Description: "info3", Severity: "info"},
	}

	result := evictFindings(findings, 3)
	if len(result) != 3 {
		t.Fatalf("evicted findings = %d, want 3", len(result))
	}
	// Should keep critical and high, plus newest info
	has := map[string]bool{}
	for _, f := range result {
		has[f.Description] = true
	}
	if !has["critical1"] || !has["high1"] {
		t.Error("high-priority findings should be kept")
	}
}

func TestGenerateReport(t *testing.T) {
	result := &AnalyzeResult{
		Summary: "Test summary",
		Findings: []Finding{
			{Description: "Critical issue", Severity: "critical", Evidence: "data shows X"},
			{Description: "Minor note", Severity: "info", Evidence: "general observation"},
		},
		Windows: 2,
	}

	report := GenerateReport("Test perspective", result)
	if report == "" {
		t.Fatal("empty report")
	}
	if !contains(report, "Critical issue") {
		t.Error("report missing critical finding")
	}
	if !contains(report, "Test summary") {
		t.Error("report missing summary")
	}
	if !contains(report, "critical") && !contains(report, "Critical") {
		t.Error("report missing severity section")
	}
}

func TestRowsToJSON(t *testing.T) {
	results := []map[string]any{
		{"name": "Alice", "age": 30},
		{"name": "Bob", "age": 25},
	}
	rows := RowsToJSON(results)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(rows[0]), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestParseWindowResponseCodeFence(t *testing.T) {
	s := &Summarizer{}
	resp := "Here's my analysis:\n```json\n{\"summary\":\"test\",\"new_findings\":[]}\n```\n"
	wr := s.parseWindowResponse(resp)
	if wr.Summary != "test" {
		t.Errorf("summary = %q, want 'test'", wr.Summary)
	}
}

func TestParseWindowResponseFallback(t *testing.T) {
	s := &Summarizer{}
	resp := "I couldn't parse the data properly but here's what I found."
	wr := s.parseWindowResponse(resp)
	if wr.Summary != resp {
		t.Errorf("fallback summary should be raw text")
	}
}

// contains is defined in load_test.go
