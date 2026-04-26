//go:build lmstudio
// +build lmstudio

// Agent-level integration tests against a running LM Studio server.
// Run with: go test ./internal/agent/ -tags "lmstudio no_duckdb_arrow" -v -timeout 300s
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/chat"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func newLMStudioAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Memory.HotTokenLimit = 2048 // aggressive compaction for testing
	a := New(cfg)
	a.session = &memory.Session{
		ID:      fmt.Sprintf("lmtest-%d", time.Now().UnixMilli()),
		Records: []memory.Record{},
	}
	return a
}

// TestLMStudio_Agent_BasicToolCall verifies the agent loop correctly
// handles a single tool call round with a real LLM.
func TestLMStudio_Agent_BasicToolCall(t *testing.T) {
	a := newLMStudioAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := a.Send(ctx, "What day of the week was 2026-01-15?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	t.Logf("Response: %s", result)
	if result == "" {
		t.Error("empty response")
	}

	// Check that resolve-date tool was used
	hasToolResult := false
	for _, r := range a.session.Records {
		if r.Role == "tool" && r.ToolName == "resolve-date" {
			hasToolResult = true
			t.Logf("Tool result: %s", r.Content)
		}
	}
	if hasToolResult {
		t.Log("resolve-date tool was called correctly")
	} else {
		t.Log("resolve-date tool was NOT called (model answered directly)")
	}
}

// TestLMStudio_Agent_MultiTurnToolCalling verifies tool calling remains
// functional across multiple turns (context budget control test).
func TestLMStudio_Agent_MultiTurnToolCalling(t *testing.T) {
	a := newLMStudioAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Load CSV data
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("name,score,grade\nAlice,95,A\nBob,78,B\nCharlie,62,C\nDiana,88,A\nEve,45,F\n"), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	// Turn 1: Load data
	r1, err := a.Send(ctx, fmt.Sprintf("%s を scores テーブルとして読み込んで", csvPath))
	if err != nil {
		t.Fatalf("Turn 1: %v", err)
	}
	t.Logf("Turn 1: %s", truncate(r1, 200))

	if !eng.HasData() {
		t.Fatal("Turn 1: data not loaded")
	}

	// Turn 2: Query
	r2, err := a.Send(ctx, "平均スコアを教えて")
	if err != nil {
		t.Fatalf("Turn 2: %v", err)
	}
	t.Logf("Turn 2: %s", truncate(r2, 200))

	// Turn 3: Another query
	r3, err := a.Send(ctx, "A評価の学生は誰？")
	if err != nil {
		t.Fatalf("Turn 3: %v", err)
	}
	t.Logf("Turn 3: %s", truncate(r3, 200))

	// Turn 4: This is the critical turn — context is now ~6+ messages
	// If context budget control works, tool calling should still function
	r4, err := a.Send(ctx, "最低スコアの学生を教えて")
	if err != nil {
		t.Fatalf("Turn 4: %v", err)
	}
	t.Logf("Turn 4: %s", truncate(r4, 200))

	// Verify: response should mention Eve or 45 or F
	r4lower := strings.ToLower(r4)
	if !strings.Contains(r4lower, "eve") && !strings.Contains(r4, "45") && !strings.Contains(r4, "F") {
		t.Logf("WARNING: Turn 4 response may not contain the correct answer (Eve, 45, F)")
	}

	// Check that tool calls happened in some turns
	toolCallCount := 0
	for _, r := range a.session.Records {
		if r.Role == "tool" {
			toolCallCount++
		}
	}
	t.Logf("Total tool calls across all turns: %d", toolCallCount)
	t.Logf("Total session records: %d", len(a.session.Records))
}

// TestLMStudio_Agent_ContextBudgetPreventsOverflow verifies that
// BuildMessagesWithBudget keeps the context within limits even
// after many turns.
func TestLMStudio_Agent_ContextBudgetPreventsOverflow(t *testing.T) {
	a := newLMStudioAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Send 6 rapid-fire questions (no tool calls expected)
	questions := []string{
		"こんにちは",
		"あなたの名前は？",
		"日本の首都は？",
		"富士山の高さは？",
		"1+1は？",
		"今の季節は？",
	}

	for i, q := range questions {
		result, err := a.Send(ctx, q)
		if err != nil {
			t.Fatalf("Turn %d: %v", i+1, err)
		}
		t.Logf("Turn %d: %s", i+1, truncate(result, 100))
	}

	// Now ask something that requires a tool call
	result, err := a.Send(ctx, "来週の金曜日は何月何日？")
	if err != nil {
		t.Fatalf("Tool call turn: %v", err)
	}
	t.Logf("Tool call turn: %s", truncate(result, 200))

	// The key assertion: the LLM should still be able to function
	// after 7 turns, thanks to context budget control
	if result == "" {
		t.Error("empty response after many turns")
	}

	// Check context was managed
	hotCount := 0
	warmCount := 0
	for _, r := range a.session.Records {
		switch r.Tier {
		case memory.TierHot:
			hotCount++
		case memory.TierWarm:
			warmCount++
		}
	}
	t.Logf("Records: %d hot, %d warm (total %d)", hotCount, warmCount, len(a.session.Records))
	if warmCount > 0 {
		t.Log("Memory compaction occurred (warm summaries exist)")
	}
}

// TestLMStudio_Agent_AnalyzeData verifies the analyze-data tool
// works end-to-end with a real LLM.
func TestLMStudio_Agent_AnalyzeData(t *testing.T) {
	a := newLMStudioAgent(t)
	a.findings = findings.NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Set up data
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "sales.csv")
	os.WriteFile(csvPath, []byte("product,amount,region\nWidget,100,Tokyo\nGadget,250,Osaka\nWidget,150,Tokyo\nGadget,50,Nagoya\nWidget,300,Osaka\n"), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	// Load data
	r1, err := a.Send(ctx, fmt.Sprintf("%s を sales テーブルとして読み込んで", csvPath))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("Load: %s", truncate(r1, 200))

	// Run analysis
	r2, err := a.Send(ctx, "売上データの傾向を分析して")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	t.Logf("Analyze: %s", truncate(r2, 300))

	if r2 == "" {
		t.Error("empty analysis response")
	}

	// Check if analyze-data was called
	hasAnalyze := false
	for _, r := range a.session.Records {
		if r.Role == "tool" && r.ToolName == "analyze-data" {
			hasAnalyze = true
			t.Logf("analyze-data result length: %d chars", len(r.Content))
		}
	}
	if hasAnalyze {
		t.Log("analyze-data tool was called")
	} else {
		t.Log("analyze-data was NOT called (model used query-sql or direct answer)")
	}

	// Check findings were promoted
	allFindings := a.findings.All()
	t.Logf("Promoted findings: %d", len(allFindings))
	for _, f := range allFindings {
		t.Logf("  Finding: %s [%v]", truncate(f.Content, 80), f.Tags)
	}
}

// TestLMStudio_Agent_ModelSwitch verifies switching between backends.
func TestLMStudio_Agent_ModelSwitch(t *testing.T) {
	a := newLMStudioAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Verify we're on local
	if a.CurrentBackend() != "local" {
		t.Fatalf("expected local backend, got %s", a.CurrentBackend())
	}

	// Chat on local
	r1, err := a.Send(ctx, "こんにちは")
	if err != nil {
		t.Fatalf("Local chat: %v", err)
	}
	t.Logf("Local: %s", truncate(r1, 100))

	// Switch to vertex (will fail if no ADC, that's OK)
	r2, _ := a.Send(ctx, "/model vertex")
	t.Logf("Switch: %s", truncate(r2, 100))

	// Switch back
	r3, _ := a.Send(ctx, "/model local")
	t.Logf("Switch back: %s", truncate(r3, 100))

	if a.CurrentBackend() != "local" {
		t.Errorf("expected local after switch back, got %s", a.CurrentBackend())
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// TestLMStudio_Agent_BuildMessagesTokenCount verifies that
// BuildMessagesWithBudget actually reduces token count.
func TestLMStudio_Agent_BuildMessagesTokenCount(t *testing.T) {
	a := newLMStudioAgent(t)

	// Add many messages to session
	for i := 0; i < 20; i++ {
		a.session.AddUserMessage(fmt.Sprintf("Question %d: %s", i, strings.Repeat("word ", 50)))
		a.session.AddAssistantMessage(fmt.Sprintf("Answer %d: %s", i, strings.Repeat("reply ", 50)))
	}

	// Build with budget
	budget := a.cfg.ContextBudget
	result := a.chat.BuildMessagesWithBudget(
		a.session,
		a.pinned.FormatForPrompt(),
		a.findings.FormatForPrompt(),
		chat.BuildOptions{
			MaxConversationTokens: budget.MaxContextTokens,
			MaxWarmTokens:         budget.MaxWarmTokens,
			MaxToolResultTokens:   budget.MaxToolResultTokens,
		},
	)

	// Build without budget
	allMsgs := a.chat.BuildMessages(
		a.session,
		a.pinned.FormatForPrompt(),
		a.findings.FormatForPrompt(),
	)

	t.Logf("With budget: %d messages, ~%d tokens, %d dropped", len(result.Messages), result.TotalTokens, result.DroppedCount)
	t.Logf("Without budget: %d messages", len(allMsgs))

	if len(result.Messages) >= len(allMsgs) {
		t.Error("budgeted messages should be fewer than unlimited")
	}
	if result.DroppedCount == 0 {
		t.Error("expected some messages to be dropped")
	}
}
