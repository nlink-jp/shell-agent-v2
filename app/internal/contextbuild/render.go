package contextbuild

import (
	"fmt"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// recentGap is the threshold below which consecutive records are
// considered tightly clustered and don't get a timestamp marker.
const recentGap = 30 * time.Minute

// rolesAlwaysAnnotated need a timestamp marker regardless of clustering,
// because the timing of the event has domain meaning to the LLM.
var rolesAlwaysAnnotated = map[string]bool{
	"tool":   true,
	"report": true,
}

// shouldAnnotate decides whether to prepend a timestamp marker to record i.
//
// We deliberately do NOT annotate the first record by default: the
// system block already injects "now" via temporal context, and adding
// a leading timestamp on the very first user turn caused gemini-2.5
// to read the message as a logged/historical event and stop dispatching
// tool calls. A marker is added only for records where time is itself
// information: tool/report results, and any record arriving after a
// >30-minute gap (so the model knows the session was resumed).
func shouldAnnotate(records []memory.Record, i int) bool {
	r := records[i]
	if rolesAlwaysAnnotated[r.Role] {
		return true
	}
	if i == 0 {
		return false
	}
	prev := records[i-1]
	return r.Timestamp.Sub(prev.Timestamp) > recentGap
}

// formatTimestamp renders a record's timestamp in the configured zone.
// Format: 2026-04-27 14:32 JST
func formatTimestamp(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

// renderRecordContent prepends a timestamp marker when warranted, applies
// tool-result truncation, and wraps user/tool content via the caller-
// supplied guard hook. The original record is unchanged.
func renderRecordContent(records []memory.Record, i int, opts BuildOptions) string {
	r := records[i]
	content := r.Content

	if r.Role == "tool" && opts.MaxToolResultTokens > 0 {
		content = truncateToTokens(content, opts.MaxToolResultTokens)
	}

	if opts.WrapUserToolContent != nil && (r.Role == "user" || r.Role == "tool") {
		content = opts.WrapUserToolContent(content)
	}

	if shouldAnnotate(records, i) {
		marker := "[" + formatTimestamp(r.Timestamp, opts.loc()) + "]"
		content = marker + "\n" + content
	}
	return content
}

// renderSummaryHeader produces the time-range header that wraps a summary
// block. See memory-architecture-v2.md §6.5/§6.6.
func renderSummaryHeader(from, to time.Time, count int, loc *time.Location) string {
	return fmt.Sprintf(
		"[Summary of %d earlier turn(s) — from %s to %s]",
		count,
		formatTimestamp(from, loc),
		formatTimestamp(to, loc),
	)
}

// truncateToTokens shortens text to approximately the given token budget.
// It uses the same character-based heuristic as memory.EstimateTokens,
// preferring early bytes (the start of tool output is usually most
// informative).
func truncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	if memory.EstimateTokens(text) <= maxTokens {
		return text
	}
	// 4 chars/token heuristic; leave a little slack.
	maxChars := maxTokens * 4
	if maxChars >= len(text) {
		return text
	}
	suffix := fmt.Sprintf("\n... [truncated, %d bytes total]", len(text))
	if maxChars <= len(suffix) {
		return text[:maxChars]
	}
	return text[:maxChars-len(suffix)] + suffix
}
