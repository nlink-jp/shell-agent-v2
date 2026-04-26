//go:build lmstudio
// +build lmstudio

// Heavy data analysis scenario test.
// Simulates realistic tool result accumulation (analyze-data + query-sql).
// Run with: go test ./internal/agent/ -tags "lmstudio no_duckdb_arrow" -v -timeout 600s -run "TestLMStudio_Heavy"
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

// TestLMStudio_Heavy_AnalysisWorkflow simulates a realistic data analysis
// session: load → query → analyze → query → analyze on second table.
// This is the scenario most likely to produce large contexts.
func TestLMStudio_Heavy_AnalysisWorkflow(t *testing.T) {
	cfg := config.Default()
	// Budget is unlimited by default; [Calling:] exclusion is active
	a := New(cfg)
	a.findings = findings.NewStore()
	a.session = &memory.Session{
		ID:      fmt.Sprintf("heavy-%d", time.Now().UnixMilli()),
		Records: []memory.Record{},
	}

	tmpDir := t.TempDir()

	// Create larger test CSV (30 rows)
	var csvBuf strings.Builder
	csvBuf.WriteString("date,product,category,quantity,price,region\n")
	products := []struct{ name, cat string; price int }{
		{"Widget A", "Electronics", 2980},
		{"Widget B", "Electronics", 4500},
		{"Widget C", "Electronics", 6800},
		{"Gadget X", "Home", 1200},
		{"Gadget Y", "Home", 3500},
	}
	regions := []string{"Tokyo", "Osaka", "Nagoya", "Fukuoka", "Sapporo"}
	for i := 0; i < 30; i++ {
		p := products[i%len(products)]
		r := regions[i%len(regions)]
		csvBuf.WriteString(fmt.Sprintf("2026-01-%02d,%s,%s,%d,%d,%s\n",
			(i%28)+1, p.name, p.cat, (i*3)+5, p.price, r))
	}
	csvPath := filepath.Join(tmpDir, "sales.csv")
	os.WriteFile(csvPath, []byte(csvBuf.String()), 0644)

	// Create log data
	var logBuf strings.Builder
	for i := 0; i < 20; i++ {
		level := "INFO"
		if i%7 == 0 { level = "ERROR" }
		if i%5 == 0 { level = "WARN" }
		logBuf.WriteString(fmt.Sprintf(`{"ts":"2026-01-10T%02d:%02d:00Z","level":"%s","svc":"api","msg":"req %d","ms":%d}`+"\n",
			9+i/60, i%60, level, i, 40+i*10))
	}
	logPath := filepath.Join(tmpDir, "logs.jsonl")
	os.WriteFile(logPath, []byte(logBuf.String()), 0644)

	eng := analysis.NewWithPath(a.session.ID, filepath.Join(tmpDir, "test.duckdb"))
	a.analysis = eng
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	steps := []struct {
		msg  string
		desc string
	}{
		{fmt.Sprintf("%s を sales テーブルとして読み込んで", csvPath), "Load CSV"},
		{"カテゴリ別の売上合計を教えて", "Query: category totals"},
		{"地域別のトップ商品を教えて", "Query: regional top products"},
		{"売上データの異常値やパターンを分析して", "Analyze: sales patterns"},
		{fmt.Sprintf("%s を logs テーブルとして読み込んで", logPath), "Load JSONL"},
		{"エラーの発生パターンを教えて", "Query: error patterns"},
		{"logsテーブルの応答時間の異常を分析して", "Analyze: log anomalies"},
		{"salesテーブルで最も利益率の高い商品は？", "Query: back to sales"},
	}

	for i, step := range steps {
		turn := i + 1
		t.Logf("--- Step %d: %s ---", turn, step.desc)

		r, err := a.Send(ctx, step.msg)
		if err != nil {
			t.Logf("Step %d FAILED: %v", turn, err)
			break
		}

		records := len(a.session.Records)
		tokens := countSessionTokensH(a.session)
		warmN := countWarmH(a.session)
		toolUsed := wasToolCalledInLastTurnH(a.session)

		status := "NO_TOOL"
		if toolUsed { status = "TOOL_OK" }
		if strings.Contains(r, "[Calling:") { status = "FAKE_TOOL" }

		t.Logf("Step %02d | rec=%02d | warm=%d | ~tok=%05d | %s | %s",
			turn, records, warmN, tokens, status, truncate(r, 120))
	}

	t.Logf("\n=== Session Summary ===")
	t.Logf("Total records: %d", len(a.session.Records))
	t.Logf("Estimated tokens: %d", countSessionTokensH(a.session))
	t.Logf("Warm summaries: %d", countWarmH(a.session))
	t.Logf("Promoted findings: %d", len(a.findings.All()))
	for _, f := range a.findings.All() {
		t.Logf("  Finding: [%v] %s", f.Tags, truncate(f.Content, 60))
	}
}
