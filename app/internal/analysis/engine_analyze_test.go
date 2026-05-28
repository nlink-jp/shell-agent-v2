package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// writeCSV writes a CSV with `id,value` columns and `n` data rows
// (id = 0..n-1, value = "v<id>") to a temp file under t.TempDir
// and returns the absolute path. Used by the analyze-cap tests
// which need >10k rows to clear the chat-output cap and verify
// the analyze-specific path takes over.
func writeCSV(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rows.csv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create csv: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("id,value\n"); err != nil {
		t.Fatalf("write header: %v", err)
	}
	for i := range n {
		if _, err := fmt.Fprintf(f, "%d,v%d\n", i, i); err != nil {
			t.Fatalf("write row %d: %v", i, err)
		}
	}
	return path
}

// TestQuerySQLForAnalyze_AllowsBeyond10k pins the v0.4.4 fix:
// analyze-data must be able to fetch tables larger than the
// interactive 10k cap, because the sliding-window summarizer is
// the whole point. Pre-fix, analyze-data's QuerySQL call would
// trip MaxQueryRows here.
func TestQuerySQLForAnalyze_AllowsBeyond10k(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 12_000
	if err := e.LoadCSV("rows", writeCSV(t, n)); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	results, err := e.QuerySQLForAnalyze(`SELECT id, value FROM "rows"`)
	if err != nil {
		t.Fatalf("QuerySQLForAnalyze (12k): %v", err)
	}
	if len(results) != n {
		t.Fatalf("rows = %d, want %d", len(results), n)
	}
}

// TestQuerySQL_StillCapsAt10k is the symmetric guard: the
// interactive chat-output path keeps its 10k cap. If somebody
// later "fixes" both paths together, this test stops it.
func TestQuerySQL_StillCapsAt10k(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := e.LoadCSV("rows", writeCSV(t, config.DefaultMaxQueryRows+50)); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	_, err := e.QuerySQL(`SELECT id FROM "rows"`)
	if err == nil {
		t.Fatalf("expected QuerySQL to error past DefaultMaxQueryRows=%d", config.DefaultMaxQueryRows)
	}
	if !strings.Contains(err.Error(), "exceeds 10000 rows") {
		t.Errorf("expected 'exceeds 10000 rows' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "LIMIT") {
		t.Errorf("interactive cap error should suggest LIMIT/WHERE; got: %v", err)
	}
}

// TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows verifies the
// memory-backstop semantics by shrinking the cap via the
// test seam (so we don't materialise a million rows in CI).
// The error message must NOT suggest LIMIT — adding LIMIT to
// analyze-data would defeat the sliding window's purpose.
func TestQuerySQLForAnalyze_RespectsMaxAnalyzeRows(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	setMaxAnalyzeRowsForTesting(t, 100)

	if err := e.LoadCSV("rows", writeCSV(t, 250)); err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	_, err := e.QuerySQLForAnalyze(`SELECT id FROM "rows"`)
	if err == nil {
		t.Fatalf("expected analyze cap error past %d rows", MaxAnalyzeRows)
	}
	msg := err.Error()
	if !strings.Contains(msg, "exceeds 100 rows") {
		t.Errorf("expected 'exceeds 100 rows' in error, got: %v", err)
	}
	if !strings.Contains(msg, "pre-aggregate") {
		t.Errorf("analyze cap error should suggest pre-aggregation; got: %v", err)
	}
	if strings.Contains(msg, "LIMIT") {
		t.Errorf("analyze cap error must NOT suggest LIMIT (defeats sliding window); got: %v", err)
	}
}

// TestQuerySQLForAnalyze_RejectsWrite ensures the read-only
// gate still applies on the analyze path — the new method
// must not be a write hole. Pairs with the existing
// TestQuerySQLRejectsWrite for the interactive path.
func TestQuerySQLForAnalyze_RejectsWrite(t *testing.T) {
	e, cleanup := setupTestEngine(t)
	defer cleanup()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	dangerous := []string{
		"DROP TABLE foo",
		"INSERT INTO foo VALUES (1)",
		"DELETE FROM foo",
		"CREATE TABLE foo (id INT)",
		"UPDATE foo SET id = 1",
	}
	for _, q := range dangerous {
		if _, err := e.QuerySQLForAnalyze(q); err == nil {
			t.Errorf("expected error for %q on analyze path", q)
		}
	}
}
