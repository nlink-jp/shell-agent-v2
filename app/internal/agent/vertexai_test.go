//go:build vertexai
// +build vertexai

// Agent-level integration tests against Vertex AI.
// Run with: VERTEX_PROJECT=xxx go test ./internal/agent/ -tags "vertexai no_duckdb_arrow" -v -timeout 600s -run "TestVertex_Agent"
//
// Prerequisites:
//   gcloud auth application-default login
//   export VERTEX_PROJECT=your-project-id
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
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func newVertexAgent(t *testing.T) *Agent {
	t.Helper()
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}

	cfg := config.Default()
	cfg.LLM.DefaultBackend = config.BackendVertexAI
	cfg.LLM.VertexAI.ProjectID = project
	cfg.LLM.VertexAI.Region = "us-central1"
	cfg.LLM.VertexAI.Model = "gemini-2.5-pro"

	a := New(cfg)
	a.findings = findings.NewStore()
	a.session = &memory.Session{
		ID:      fmt.Sprintf("vtx-%d", time.Now().UnixMilli()),
		Records: []memory.Record{},
	}
	return a
}

// TestVertex_Agent_BasicToolCall verifies tool calling with Vertex AI.
func TestVertex_Agent_BasicToolCall(t *testing.T) {
	a := newVertexAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := a.Send(ctx, "2026年1月15日は何曜日？")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	t.Logf("Response: %s", truncateV(result, 200))

	hasToolResult := false
	for _, r := range a.session.Records {
		if r.Role == "tool" {
			hasToolResult = true
			t.Logf("Tool: %s → %s", r.ToolName, truncateV(r.Content, 100))
		}
	}
	if hasToolResult {
		t.Log("Tool was called")
	} else {
		t.Log("No tool called (model answered directly)")
	}
}

// TestVertex_Agent_MultiTurnToolCalling verifies tool calling across
// multiple turns with Vertex AI gemini-2.5-pro.
func TestVertex_Agent_MultiTurnToolCalling(t *testing.T) {
	a := newVertexAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "data.csv")
	os.WriteFile(csvPath, []byte("name,score,grade\nAlice,95,A\nBob,78,B\nCharlie,62,C\nDiana,88,A\nEve,45,F\n"), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	steps := []struct{ msg, desc string }{
		{fmt.Sprintf("%s を scores テーブルとして読み込んで", csvPath), "Load CSV"},
		{"平均スコアを教えて", "Query: average"},
		{"最高スコアの学生は？", "Query: top student"},
		{"A評価の学生を一覧にして", "Query: grade A"},
		{"スコアの分布を分析して", "Analyze: distribution"},
	}

	for i, step := range steps {
		turn := i + 1
		r, err := a.Send(ctx, step.msg)
		if err != nil {
			t.Fatalf("Step %d (%s): %v", turn, step.desc, err)
		}

		records := len(a.session.Records)
		tokens := countSessionTokensH(a.session)
		toolUsed := wasToolCalledInLastTurnH(a.session)

		status := "NO_TOOL"
		if toolUsed {
			status = "TOOL_OK"
		}

		t.Logf("Step %02d | rec=%02d | ~tok=%05d | %s | %s | %s",
			turn, records, tokens, a.CurrentBackend(), status, truncateV(r, 100))
	}

	t.Logf("Total records: %d", len(a.session.Records))
	t.Logf("Promoted findings: %d", len(a.findings.All()))
}

// TestVertex_Agent_HeavyAnalysis tests the full analysis workflow
// with Vertex AI to compare behavior with local LLM.
func TestVertex_Agent_HeavyAnalysis(t *testing.T) {
	a := newVertexAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	tmpDir := t.TempDir()

	// Create test data
	var csvBuf strings.Builder
	csvBuf.WriteString("date,product,category,quantity,price,region\n")
	products := []struct{ name, cat string; price int }{
		{"Widget A", "Electronics", 2980},
		{"Widget B", "Electronics", 4500},
		{"Gadget X", "Home", 1200},
	}
	regions := []string{"Tokyo", "Osaka", "Nagoya"}
	for i := 0; i < 15; i++ {
		p := products[i%len(products)]
		r := regions[i%len(regions)]
		csvBuf.WriteString(fmt.Sprintf("2026-01-%02d,%s,%s,%d,%d,%s\n",
			(i%28)+1, p.name, p.cat, (i*3)+5, p.price, r))
	}
	csvPath := filepath.Join(tmpDir, "sales.csv")
	os.WriteFile(csvPath, []byte(csvBuf.String()), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	steps := []struct{ msg, desc string }{
		{fmt.Sprintf("%s を sales テーブルとして読み込んで", csvPath), "Load CSV"},
		{"カテゴリ別の売上合計を教えて", "Query: category totals"},
		{"売上データの傾向を分析して", "Analyze: trends"},
		{"最も売上が高い地域は？", "Query: top region"},
		{"地域別の商品構成比を教えて", "Query: regional mix"},
	}

	for i, step := range steps {
		turn := i + 1
		r, err := a.Send(ctx, step.msg)
		if err != nil {
			t.Logf("Step %d (%s) FAILED: %v", turn, step.desc, err)
			continue
		}

		records := len(a.session.Records)
		tokens := countSessionTokensH(a.session)
		warmN := countWarmH(a.session)
		toolUsed := wasToolCalledInLastTurnH(a.session)

		status := "NO_TOOL"
		if toolUsed {
			status = "TOOL_OK"
		}

		t.Logf("Step %02d | rec=%02d | warm=%d | ~tok=%05d | %s | %s",
			turn, records, warmN, tokens, status, truncateV(r, 120))
	}

	t.Logf("\n=== Vertex AI Session Summary ===")
	t.Logf("Backend: %s", a.CurrentBackend())
	t.Logf("Total records: %d", len(a.session.Records))
	t.Logf("Estimated tokens: %d", countSessionTokensH(a.session))
	t.Logf("Warm summaries: %d", countWarmH(a.session))
	t.Logf("Promoted findings: %d", len(a.findings.All()))
}

func truncateV(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
