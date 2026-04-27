package contextbuild

import (
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

var utc = time.UTC

func mkRec(t time.Time, role, content string) memory.Record {
	return memory.Record{Timestamp: t, Role: role, Content: content, Tier: memory.TierHot}
}

func TestShouldAnnotate_GapTriggers(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	records := []memory.Record{
		mkRec(now, "user", "hi"),
		mkRec(now.Add(2*time.Minute), "assistant", "hello"),       // tight cluster
		mkRec(now.Add(45*time.Minute), "user", "are you still there?"), // gap > 30min
	}
	if !shouldAnnotate(records, 0) {
		t.Error("first record always annotated")
	}
	if shouldAnnotate(records, 1) {
		t.Error("tightly clustered record should not annotate")
	}
	if !shouldAnnotate(records, 2) {
		t.Error("record after >30min gap should annotate")
	}
}

func TestShouldAnnotate_ToolAlways(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	records := []memory.Record{
		mkRec(now, "user", "run query"),
		mkRec(now.Add(time.Second), "tool", "result rows..."),
	}
	if !shouldAnnotate(records, 1) {
		t.Error("tool record always annotated regardless of clustering")
	}
}

func TestRenderRecordContent_PrependsMarker(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 30, 0, 0, utc)
	records := []memory.Record{mkRec(now, "user", "hello")}
	out := renderRecordContent(records, 0, BuildOptions{Loc: utc})
	if !strings.HasPrefix(out, "[2026-04-27 10:30 UTC]\n") {
		t.Errorf("marker missing; got %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("body missing; got %q", out)
	}
}

func TestRenderRecordContent_ToolTruncation(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, utc)
	huge := strings.Repeat("payload ", 500)
	records := []memory.Record{mkRec(now, "tool", huge)}
	out := renderRecordContent(records, 0, BuildOptions{MaxToolResultTokens: 50, Loc: utc})
	if !strings.Contains(out, "[truncated") {
		t.Error("expected truncation suffix")
	}
	if len(out) >= len(huge) {
		t.Errorf("output not shrunk: %d >= %d", len(out), len(huge))
	}
}

func TestRenderSummaryHeader(t *testing.T) {
	from := time.Date(2026, 4, 25, 14, 32, 0, 0, utc)
	to := time.Date(2026, 4, 27, 9, 18, 0, 0, utc)
	h := renderSummaryHeader(from, to, 17, utc)
	want := "[Summary of 17 earlier turn(s) — from 2026-04-25 14:32 UTC to 2026-04-27 09:18 UTC]"
	if h != want {
		t.Errorf("got  %q\nwant %q", h, want)
	}
}

func TestTruncateToTokens_Idempotent(t *testing.T) {
	short := "tiny payload"
	if got := truncateToTokens(short, 100); got != short {
		t.Errorf("short string should be unchanged; got %q", got)
	}
	if got := truncateToTokens("anything", 0); got != "anything" {
		t.Error("zero budget should disable truncation")
	}
}
