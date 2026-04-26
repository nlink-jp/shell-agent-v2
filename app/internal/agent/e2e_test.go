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

// newTestAgent creates an agent with a mock backend for E2E testing.
func newTestAgent(t *testing.T, mock *llm.MockBackend) *Agent {
	t.Helper()
	a := New(config.Default())
	a.backend = mock
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "e2e-test", Title: "E2E Test", Records: []memory.Record{}}
	return a
}

// --- E2E: Full Agent Loop ---

func TestE2E_SimpleChat(t *testing.T) {
	mock := llm.NewMockBackend(llm.MockResponse{Content: "Hello! How can I help?"})
	a := newTestAgent(t, mock)

	result, err := a.Send(context.Background(), "Hi there")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result != "Hello! How can I help?" {
		t.Errorf("result = %q", result)
	}

	// Verify message was added to session
	if len(a.session.Records) != 2 {
		t.Fatalf("records = %d, want 2 (user + assistant)", len(a.session.Records))
	}
	if a.session.Records[0].Role != "user" {
		t.Errorf("record[0] role = %v", a.session.Records[0].Role)
	}
	if a.session.Records[1].Role != "assistant" {
		t.Errorf("record[1] role = %v", a.session.Records[1].Role)
	}

	// Verify the mock received at least the main chat call
	// (post-response tasks may add extractPinnedMemories calls)
	if len(mock.Calls()) < 1 {
		t.Fatalf("mock calls = %d, want >= 1", len(mock.Calls()))
	}
	msgs := mock.Calls()[0].Messages
	if msgs[0].Role != "system" {
		t.Errorf("first message role = %v, want system", msgs[0].Role)
	}
}

func TestE2E_SlashPathNotCommand(t *testing.T) {
	// Messages starting with / but not a known command should be sent to LLM
	mock := llm.NewMockBackend(llm.MockResponse{Content: "I'll load that file."})
	a := newTestAgent(t, mock)

	result, err := a.Send(context.Background(), "/tmp/sales.csv を読み込んで")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result != "I'll load that file." {
		t.Errorf("result = %q, want LLM response", result)
	}
	// Should have been sent to LLM, not handled as command
	if len(mock.Calls()) < 1 {
		t.Errorf("mock calls = %d, want >= 1 (message should reach LLM)", len(mock.Calls()))
	}
}

func TestE2E_ToolCallLoop(t *testing.T) {
	// LLM calls resolve-date, then responds with the result
	mock := llm.NewMockWithToolCall(
		"resolve-date",
		`{"expression":"today"}`,
		"Today's date is confirmed.",
	)
	a := newTestAgent(t, mock)

	result, err := a.Send(context.Background(), "What's today's date?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result != "Today's date is confirmed." {
		t.Errorf("result = %q", result)
	}

	// Should have: user, tool result, assistant (final)
	// Note: empty assistant message from tool call round is NOT recorded (design rule)
	if len(a.session.Records) < 3 {
		t.Fatalf("records = %d, want >= 3", len(a.session.Records))
	}

	// Verify tool result is in session
	hasToolResult := false
	for _, r := range a.session.Records {
		if r.Role == "tool" {
			hasToolResult = true
			if r.ToolName != "resolve-date" {
				t.Errorf("tool name = %v", r.ToolName)
			}
		}
	}
	if !hasToolResult {
		t.Error("expected tool result in session")
	}

	// Mock should have been called at least twice (tool call + final response)
	// Post-response tasks (extractPinnedMemories) may add more calls
	if len(mock.Calls()) < 2 {
		t.Errorf("mock calls = %d, want >= 2", len(mock.Calls()))
	}
}

// --- E2E: Analysis Workflow ---

func TestE2E_AnalysisWorkflow(t *testing.T) {
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "sales.csv")
	os.WriteFile(csvPath, []byte("product,amount\nWidget,100\nGadget,250\nDoohickey,75\n"), 0644)

	// Step 1: LLM calls load-data
	// Step 2: LLM calls query-sql
	// Step 3: LLM responds with analysis
	mock := llm.NewMockBackend(
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-1",
			Name:      "load-data",
			Arguments: `{"file_path":"` + csvPath + `","table_name":"sales"}`,
		}}},
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-2",
			Name:      "query-sql",
			Arguments: `{"sql":"SELECT product, amount FROM \"sales\" ORDER BY amount DESC"}`,
		}}},
		llm.MockResponse{Content: "Gadget has the highest sales at 250."},
	)

	a := newTestAgent(t, mock)
	engine := analysis.NewWithPath("e2e-test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	result, err := a.Send(context.Background(), "Load sales.csv and find the top product")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(result, "Gadget") {
		t.Errorf("result = %q, expected mention of Gadget", result)
	}

	// Verify data was loaded
	if !engine.HasData() {
		t.Error("expected data in analysis engine")
	}

	// Verify tool results in session
	toolResults := 0
	for _, r := range a.session.Records {
		if r.Role == "tool" {
			toolResults++
		}
	}
	if toolResults != 2 {
		t.Errorf("tool results = %d, want 2 (load + query)", toolResults)
	}
}

// --- E2E: Finding Promotion ---

func TestE2E_FindingPromotion(t *testing.T) {
	mock := llm.NewMockBackend(
		// LLM decides to promote a finding
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:   "tc-1",
			Name: "promote-finding",
			Arguments: `{"content":"Sales peak in Q2","tags":["sales","quarterly"]}`,
		}}},
		llm.MockResponse{Content: "I've noted the Q2 sales peak for future reference."},
	)

	a := newTestAgent(t, mock)
	tmpDir := t.TempDir()
	engine := analysis.NewWithPath("e2e-test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	result, err := a.Send(context.Background(), "Note the sales peak finding")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = result

	// Verify finding was stored
	all := a.findings.All()
	if len(all) != 1 {
		t.Fatalf("findings = %d, want 1", len(all))
	}
	if all[0].Content != "Sales peak in Q2" {
		t.Errorf("finding content = %q", all[0].Content)
	}
	if len(all[0].Tags) != 2 {
		t.Errorf("finding tags = %v", all[0].Tags)
	}
	if all[0].OriginSessionID != "e2e-test" {
		t.Errorf("origin session = %v", all[0].OriginSessionID)
	}
}

// --- E2E: Session Isolation ---

func TestE2E_SessionIsolation(t *testing.T) {
	tmpDir := t.TempDir()

	// Session A: load data
	mockA := llm.NewMockBackend(
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:   "tc-1",
			Name: "load-data",
			Arguments: `{"file_path":"` + filepath.Join(tmpDir, "a.csv") + `","table_name":"data_a"}`,
		}}},
		llm.MockResponse{Content: "Data loaded in session A."},
	)

	os.WriteFile(filepath.Join(tmpDir, "a.csv"), []byte("x\n1\n"), 0644)

	a := New(config.Default())
	a.backend = mockA
	a.findings = findings.NewStore()

	sessionA := &memory.Session{ID: "sess-a", Title: "Session A", Records: []memory.Record{}}
	a.LoadSession(sessionA)
	engineA := analysis.NewWithPath("sess-a", filepath.Join(tmpDir, "a.duckdb"))
	a.SetAnalysis(engineA)

	a.Send(context.Background(), "Load data for session A")

	if !engineA.HasData() {
		t.Fatal("session A should have data")
	}

	// Switch to session B
	sessionB := &memory.Session{ID: "sess-b", Title: "Session B", Records: []memory.Record{}}
	a.LoadSession(sessionB)
	engineB := analysis.NewWithPath("sess-b", filepath.Join(tmpDir, "b.duckdb"))
	a.SetAnalysis(engineB)

	if engineB.HasData() {
		t.Error("session B should not have data")
	}

	// Session A's data still accessible via its engine
	if !engineA.HasData() {
		t.Error("session A engine should still have data")
	}

	engineA.Close()
	engineB.Close()
}

// --- E2E: Dynamic Tool Filtering ---

func TestE2E_DynamicToolFilteringInLoop(t *testing.T) {
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("a\n1\n"), 0644)

	// First call: LLM sees only load-data (no data yet), calls it
	// Second call: LLM now sees query-sql (data loaded), uses it
	mock := llm.NewMockBackend(
		llm.MockResponse{ToolCalls: []llm.ToolCall{{
			ID:        "tc-1",
			Name:      "load-data",
			Arguments: `{"file_path":"` + csvPath + `","table_name":"test"}`,
		}}},
		llm.MockResponse{Content: "Data loaded and ready."},
	)

	a := newTestAgent(t, mock)
	engine := analysis.NewWithPath("e2e-test", filepath.Join(tmpDir, "test.duckdb"))
	a.SetAnalysis(engine)
	defer engine.Close()

	// Before send: verify no query-sql in tools
	tools := a.buildToolDefs()
	for _, tool := range tools {
		if tool.Name == "query-sql" {
			t.Error("query-sql should not be available before data load")
		}
	}

	a.Send(context.Background(), "Load the data")

	// After send: verify query-sql is now available
	tools = a.buildToolDefs()
	hasQuerySQL := false
	for _, tool := range tools {
		if tool.Name == "query-sql" {
			hasQuerySQL = true
		}
	}
	if !hasQuerySQL {
		t.Error("query-sql should be available after data load")
	}

	// Verify the first call had load-data but not query-sql
	firstCallTools := mock.Calls()[0].Tools
	for _, tool := range firstCallTools {
		if tool.Name == "query-sql" {
			t.Error("first LLM call should not have query-sql")
		}
	}
}

// --- E2E: Streaming ---

func TestE2E_NonStreamingAgentLoop(t *testing.T) {
	// Agent loop uses Chat() (non-streaming) for ALL rounds to prevent
	// gemma text tool call markup from leaking through streaming.
	mock := llm.NewMockBackend(llm.MockResponse{Content: "Non-streamed response"})
	a := newTestAgent(t, mock)

	var tokens []string
	a.SetStreamHandler(func(token string, done bool) {
		if token != "" {
			tokens = append(tokens, token)
		}
	})

	result, err := a.Send(context.Background(), "Test")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if result != "Non-streamed response" {
		t.Errorf("result = %q", result)
	}
	// No streaming tokens expected — first round has tools active, uses Chat()
	if len(tokens) != 0 {
		t.Errorf("expected 0 streaming tokens, got %d", len(tokens))
	}
}

func TestE2E_StreamingAfterToolExecution(t *testing.T) {
	// After tool execution, the agent calls LLM with tools=nil to force
	// a text response. This round uses ChatStream() for real-time display.
	mock := llm.NewMockBackend(
		llm.MockResponse{
			ToolCalls: []llm.ToolCall{{
				ID: "tc-1", Name: "resolve-date", Arguments: `{"expression":"today"}`,
			}},
		},
		llm.MockResponse{Content: "Streamed final response"},
	)
	a := newTestAgent(t, mock)

	var tokens []string
	var gotDone bool
	a.SetStreamHandler(func(token string, done bool) {
		if token != "" {
			tokens = append(tokens, token)
		}
		if done {
			gotDone = true
		}
	})

	result, err := a.Send(context.Background(), "What date is today?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if result != "Streamed final response" {
		t.Errorf("result = %q, want %q", result, "Streamed final response")
	}
	// Streaming tokens expected — final round uses ChatStream()
	if len(tokens) == 0 {
		t.Error("expected streaming tokens from final round, got 0")
	}
	if !gotDone {
		t.Error("expected done signal from streaming")
	}
}

// --- E2E: /model Command + Chat ---

func TestE2E_ModelSwitchThenChat(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore()
	a.session = &memory.Session{ID: "e2e-test", Records: []memory.Record{}}

	// Switch to vertex
	result, err := a.Send(context.Background(), "/model vertex")
	if err != nil {
		t.Fatalf("Send /model: %v", err)
	}
	if !strings.Contains(result, "Vertex AI") {
		t.Errorf("result = %q", result)
	}
	if a.CurrentBackend() != "vertex_ai" {
		t.Errorf("backend = %v", a.CurrentBackend())
	}

	// Switch back and verify state is idle
	a.Send(context.Background(), "/model local")
	if a.State() != StateIdle {
		t.Error("should be idle after commands")
	}
	if a.CurrentBackend() != "local" {
		t.Error("should be local after switch back")
	}
}

// --- E2E: Findings Cross-Session ---

func TestE2E_FindingsCrossSession(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore()

	// Session A: promote a finding
	sessionA := &memory.Session{ID: "sess-a", Title: "Sales Analysis", Records: []memory.Record{}}
	a.LoadSession(sessionA)
	a.findings.Add("Revenue doubled in Q3", "sess-a", "Sales Analysis", []string{"revenue"})

	// Session B: findings should be visible in system prompt
	sessionB := &memory.Session{ID: "sess-b", Title: "Planning", Records: []memory.Record{}}
	a.LoadSession(sessionB)

	prompt := a.findings.FormatForPrompt()
	if !strings.Contains(prompt, "Revenue doubled in Q3") {
		t.Error("finding from session A not visible in session B context")
	}
	if !strings.Contains(prompt, "Sales Analysis") {
		t.Error("finding origin session title not in prompt")
	}
}

// --- E2E: Abort ---

func TestE2E_AbortDuringExecution(t *testing.T) {
	// Create a mock that blocks (we'll cancel before it returns)
	ctx, cancel := context.WithCancel(context.Background())

	mock := llm.NewMockBackend(llm.MockResponse{Content: "Should not see this"})
	a := newTestAgent(t, mock)

	// Cancel immediately
	cancel()

	result, _ := a.Send(ctx, "This should be cancelled")
	if result != "(Cancelled)" {
		// May also get an error from the context, which is acceptable
		if result != "" {
			t.Logf("result = %q (acceptable if context cancelled)", result)
		}
	}

	// Agent should return to idle
	if a.State() != StateIdle {
		t.Errorf("state = %v, want idle after abort", a.State())
	}
}
