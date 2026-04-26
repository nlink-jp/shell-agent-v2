//go:build lmstudio
// +build lmstudio

// Limit tests: find the threshold where gemma-4 tool calling degrades.
// Run with: go test ./internal/agent/ -tags "lmstudio no_duckdb_arrow" -v -timeout 600s -run "TestLMStudio_Limit"
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
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestLMStudio_Limit_NoBudget sends increasingly many messages WITHOUT
// context budget control, testing each turn for tool calling ability.
// This identifies the threshold where gemma-4 loses tool calling.
func TestLMStudio_Limit_NoBudget(t *testing.T) {
	cfg := config.Default()
	cfg.ContextBudget.MaxContextTokens = 0 // disable budget control
	cfg.Memory.HotTokenLimit = 999999      // disable compaction

	a := New(cfg)
	a.session = &memory.Session{
		ID:      fmt.Sprintf("limit-nobud-%d", time.Now().UnixMilli()),
		Records: []memory.Record{},
	}

	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("name,score\nAlice,95\nBob,78\nCharlie,62\nDiana,88\nEve,45\n"), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	// Load data first
	r, err := a.Send(ctx, fmt.Sprintf("%s を scores テーブルとして読み込んで", csvPath))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("Load: %s", truncate(r, 100))

	// Progressively ask questions that require tool calls
	queries := []string{
		"Aliceのスコアは？",
		"Bobのスコアは？",
		"平均スコアは？",
		"最高スコアの学生は？",
		"最低スコアの学生は？",
		"スコアが80以上の学生は？",
		"スコアの合計は？",
		"学生数は？",
		"Charlieのスコアは？",
		"Dianaのスコアは？",
		"スコアの中央値は？",
		"スコアの標準偏差は？",
	}

	lastToolCallTurn := 0
	for i, q := range queries {
		turn := i + 2 // +1 for load, +1 for 1-indexed

		r, err := a.Send(ctx, q)
		if err != nil {
			t.Logf("Turn %d: ERROR %v", turn, err)
			break
		}

		// Count records to see context size
		records := len(a.session.Records)
		tokens := countSessionTokens(a.session)

		// Check if a tool was called in this turn
		toolUsed := wasToolCalledInLastTurn(a.session)

		status := "TEXT_ONLY"
		if toolUsed {
			status = "TOOL_CALL"
			lastToolCallTurn = turn
		}

		t.Logf("Turn %02d | records=%02d | ~tokens=%05d | %s | %s",
			turn, records, tokens, status, truncate(r, 80))

		// Detect [Calling: ...] text output (LLM faking tool call)
		if strings.Contains(r, "[Calling:") || strings.Contains(r, "<|tool_call>") {
			t.Logf("Turn %02d: *** FAKE TOOL CALL DETECTED ***", turn)
		}
	}

	t.Logf("\n=== RESULT: Last successful tool call at turn %d ===", lastToolCallTurn)
	t.Logf("Total session records: %d", len(a.session.Records))
	t.Logf("Estimated total tokens: %d", countSessionTokens(a.session))
}

// TestLMStudio_Limit_WithBudget runs the same test WITH context budget control.
func TestLMStudio_Limit_WithBudget(t *testing.T) {
	cfg := config.Default()
	// Use default budget: MaxContextTokens=8192

	a := New(cfg)
	a.session = &memory.Session{
		ID:      fmt.Sprintf("limit-budget-%d", time.Now().UnixMilli()),
		Records: []memory.Record{},
	}

	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("name,score\nAlice,95\nBob,78\nCharlie,62\nDiana,88\nEve,45\n"), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	// Load data
	r, err := a.Send(ctx, fmt.Sprintf("%s を scores テーブルとして読み込んで", csvPath))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("Load: %s", truncate(r, 100))

	queries := []string{
		"Aliceのスコアは？",
		"Bobのスコアは？",
		"平均スコアは？",
		"最高スコアの学生は？",
		"最低スコアの学生は？",
		"スコアが80以上の学生は？",
		"スコアの合計は？",
		"学生数は？",
		"Charlieのスコアは？",
		"Dianaのスコアは？",
		"スコアの中央値は？",
		"スコアの標準偏差は？",
	}

	lastToolCallTurn := 0
	for i, q := range queries {
		turn := i + 2

		r, err := a.Send(ctx, q)
		if err != nil {
			t.Logf("Turn %d: ERROR %v", turn, err)
			break
		}

		records := len(a.session.Records)
		tokens := countSessionTokens(a.session)
		warmCount := countWarm(a.session)
		toolUsed := wasToolCalledInLastTurn(a.session)

		// Count messages that BuildMessagesWithBudget would produce
		budgetResult := a.chat.BuildMessagesWithBudget(
			a.session,
			a.pinned.FormatForPrompt(),
			a.findings.FormatForPrompt(),
			chat.BuildOptions{
				MaxConversationTokens: cfg.ContextBudget.MaxContextTokens,
				MaxWarmTokens:         cfg.ContextBudget.MaxWarmTokens,
				MaxToolResultTokens:   cfg.ContextBudget.MaxToolResultTokens,
			},
		)

		status := "TEXT_ONLY"
		if toolUsed {
			status = "TOOL_CALL"
			lastToolCallTurn = turn
		}

		t.Logf("Turn %02d | records=%02d | warm=%d | ~tokens=%05d | sent=%02d msgs (~%d tok) | %s | %s",
			turn, records, warmCount, tokens,
			len(budgetResult.Messages), budgetResult.TotalTokens,
			status, truncate(r, 80))
	}

	t.Logf("\n=== RESULT: Last successful tool call at turn %d ===", lastToolCallTurn)
	t.Logf("Total session records: %d", len(a.session.Records))
}

// --- helpers ---

func countSessionTokens(s *memory.Session) int {
	total := 0
	for _, r := range s.Records {
		total += memory.EstimateTokens(r.Content)
	}
	return total
}

func countWarm(s *memory.Session) int {
	count := 0
	for _, r := range s.Records {
		if r.Tier == memory.TierWarm {
			count++
		}
	}
	return count
}

func wasToolCalledInLastTurn(s *memory.Session) bool {
	// Walk backwards from end to find the last user message,
	// then check if there are tool records after it
	lastUserIdx := -1
	for i := len(s.Records) - 1; i >= 0; i-- {
		if s.Records[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return false
	}
	for i := lastUserIdx + 1; i < len(s.Records); i++ {
		if s.Records[i].Role == "tool" {
			return true
		}
	}
	return false
}

// TestLMStudio_Limit_TokenThreshold tests specific token counts
// to find the exact threshold where tool calling breaks.
func TestLMStudio_Limit_TokenThreshold(t *testing.T) {
	client := llm.NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "google/gemma-4-26b-a4b",
	})

	tools := []llm.ToolDef{{
		Name:        "get_score",
		Description: "Get a student's score from the database",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Student name"},
			},
			"required": []string{"name"},
		},
	}}

	// Test at different message counts (each turn adds ~200 real tokens via long filler)
	turnCounts := []int{0, 4, 8, 12, 16, 20, 24, 28}

	for _, turns := range turnCounts {
		t.Run(fmt.Sprintf("turns_%d", turns), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			messages := []llm.Message{
				{Role: llm.RoleSystem, Content: "You are a helpful assistant. Use tools when asked about student scores."},
			}

			// Add realistic filler conversation
			for i := 0; i < turns; i++ {
				messages = append(messages, llm.Message{
					Role:    llm.RoleUser,
					Content: fmt.Sprintf("質問%d: データ分析において、カテゴリ別の集計や地域ごとの傾向を把握するためにはどのようなSQLクエリを書けばよいですか？具体的な例を教えてください。", i),
				})
				messages = append(messages, llm.Message{
					Role:    llm.RoleAssistant,
					Content: fmt.Sprintf("回答%d: カテゴリ別の集計にはGROUP BY句を使用します。例えば「SELECT category, SUM(amount) FROM sales GROUP BY category ORDER BY SUM(amount) DESC」のようなクエリで、各カテゴリの合計金額を降順で取得できます。地域ごとの傾向を見るには、WHERE句で地域を指定するか、ピボットテーブルを作成する方法があります。また、時系列データの場合はDATE_TRUNC関数を活用することで、月別や週別の集計が可能です。", i),
				})
			}

			// The actual tool-calling request
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: "Aliceのスコアを調べて",
			})

			totalTokens := 0
			for _, m := range messages {
				totalTokens += memory.EstimateTokens(m.Content)
			}

			resp, err := client.Chat(ctx, messages, tools)
			if err != nil {
				t.Logf("turns=%02d | msgs=%02d | ~tokens=%05d | ERROR: %v", turns, len(messages), totalTokens, err)
				return
			}

			status := "TEXT_ONLY"
			if len(resp.ToolCalls) > 0 {
				status = fmt.Sprintf("TOOL_CALL(%s)", resp.ToolCalls[0].Name)
			}
			if strings.Contains(resp.Content, "[Calling:") || strings.Contains(resp.Content, "tool_call") {
				status = "FAKE_TOOL"
			}

			t.Logf("turns=%02d | msgs=%03d | ~tokens=%05d | %s | %s",
				turns, len(messages), totalTokens, status, truncate(resp.Content, 80))
		})
	}
}
