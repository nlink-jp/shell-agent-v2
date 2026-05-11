package analysis

import (
	"fmt"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// ChunkerConfig controls how ChunkText splits markdown / text
// content into windows for the sliding-window summarizer.
//
// Defaults (DefaultChunkerConfig) target ~2000 tokens per chunk
// with 10% overlap. A single line exceeding MaxLineWidth chars
// is force-broken at the width limit so it can't blow a single
// chunk's budget by itself. Total chunk count is capped at
// MaxChunks; documents that would exceed it return an error
// suggesting the LLM scope down via the `lines` parameter.
type ChunkerConfig struct {
	TargetTokens int     // soft target token count per chunk (default 2000)
	OverlapRatio float64 // fraction of TargetTokens carried as overlap (default 0.1)
	MaxLineWidth int     // force-break threshold for single lines (default 10000 chars)
	MaxChunks    int     // hard cap on chunk count (default 1000)
}

// DefaultChunkerConfig returns the production defaults. Callers
// who want to tune (e.g., a future Settings UI) start from this
// and override individual fields.
func DefaultChunkerConfig() ChunkerConfig {
	return ChunkerConfig{
		TargetTokens: 2000,
		OverlapRatio: 0.1,
		MaxLineWidth: 10000,
		MaxChunks:    1000,
	}
}

// ChunkText splits content into a slice of chunks suitable for
// feeding into Summarizer.Analyze. Each chunk is at most about
// TargetTokens tokens (with up to ~20% overshoot to keep lines
// atomic), and successive chunks overlap by ~OverlapRatio so the
// running-summary path observes some continuity at boundaries.
//
// Markdown heading awareness: when a chunk would close mid-section,
// the closer looks back within the last OverlapRatio fraction of
// the chunk and, if it finds a heading line (`^#{1,3} `), snaps the
// chunk boundary so the heading starts the next chunk. Degenerate
// inputs without headings simply fall back to line-atomic boundary.
//
// Errors:
//   - empty content → returns (nil, nil); the caller may treat as
//     "nothing to analyze".
//   - chunk count > MaxChunks → returns an error suggesting the
//     LLM narrow the input range; the design rejects implicit
//     dropping of content.
func ChunkText(content string, cfg ChunkerConfig) ([]string, error) {
	if cfg.TargetTokens <= 0 {
		cfg.TargetTokens = 2000
	}
	if cfg.OverlapRatio < 0 || cfg.OverlapRatio >= 1 {
		cfg.OverlapRatio = 0.1
	}
	if cfg.MaxLineWidth <= 0 {
		cfg.MaxLineWidth = 10000
	}
	if cfg.MaxChunks <= 0 {
		cfg.MaxChunks = 1000
	}
	if content == "" {
		return nil, nil
	}

	// Step 1: split into lines, force-breaking any line that
	// exceeds MaxLineWidth. We keep newlines implicit (the chunk
	// builder rejoins with "\n"); a force-broken segment is just
	// inserted as additional adjacent lines.
	lines := splitAndBreakLines(content, cfg.MaxLineWidth)

	// Step 2: walk lines, accumulating into the current chunk
	// until adding one more line would exceed TargetTokens. At
	// boundary, optionally snap back to a heading line within
	// the last OverlapRatio fraction of the chunk.
	overlapTokens := max(int(float64(cfg.TargetTokens)*cfg.OverlapRatio), 1)

	var chunks []string
	var curr []string
	currTokens := 0

	for _, line := range lines {
		lt := memory.EstimateTokens(line)
		// Close the current chunk before adding a line that
		// would push us past the target — but only if the
		// current chunk is non-empty. A single line bigger than
		// TargetTokens still goes into its own chunk (already
		// width-limited by Step 1).
		if currTokens+lt > cfg.TargetTokens && len(curr) > 0 {
			closeIdx := chooseCloseIndex(curr, cfg.OverlapRatio)
			chunkLines := curr[:closeIdx]
			chunks = append(chunks, strings.Join(chunkLines, "\n"))
			if len(chunks) > cfg.MaxChunks {
				return nil, fmt.Errorf("text exceeds chunk cap (%d > %d). Narrow the input via the lines argument or shorten the document", len(chunks), cfg.MaxChunks)
			}
			// Carry overlap into the next chunk: the lines
			// AFTER closeIdx, plus we additionally keep a tail
			// of the closed chunk if the heading snap left
			// nothing for overlap.
			curr = append([]string{}, curr[closeIdx:]...)
			curr = append(curr, takeOverlapTail(chunkLines, overlapTokens)...)
			currTokens = sumTokens(curr)
		}
		curr = append(curr, line)
		currTokens += lt
	}

	if len(curr) > 0 {
		chunks = append(chunks, strings.Join(curr, "\n"))
		if len(chunks) > cfg.MaxChunks {
			return nil, fmt.Errorf("text exceeds chunk cap (%d > %d). Narrow the input via the lines argument or shorten the document", len(chunks), cfg.MaxChunks)
		}
	}

	return chunks, nil
}

// splitAndBreakLines splits content on '\n' and force-breaks any
// resulting line that exceeds maxWidth into successive segments
// of at most maxWidth chars (byte-counted; safe for UTF-8 because
// we only break on byte boundaries that don't fall mid-rune — we
// approximate by breaking on the byte boundary closest to maxWidth
// that lands on a rune start. For pathological inputs that's still
// valid bytes, just possibly slightly past the exact threshold).
func splitAndBreakLines(content string, maxWidth int) []string {
	raw := strings.Split(content, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if len(line) <= maxWidth {
			out = append(out, line)
			continue
		}
		// Break the long line into segments.
		for len(line) > maxWidth {
			// Find rune-safe cut at or before maxWidth.
			cut := maxWidth
			for cut > 0 && !validUTF8BoundaryAt(line, cut) {
				cut--
			}
			if cut == 0 {
				// Pathological: no rune boundary in the first
				// maxWidth bytes. Just cut at maxWidth and accept
				// the (unlikely) mojibake — the LLM still sees
				// well-formed UTF-8 because the next segment
				// starts at the same byte we cut at, completing
				// the multi-byte sequence as the rest of its first
				// rune.
				cut = maxWidth
			}
			out = append(out, line[:cut])
			line = line[cut:]
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// validUTF8BoundaryAt reports whether byte position i is the
// start of a UTF-8 rune (i.e., not a continuation byte). Used by
// splitAndBreakLines to avoid splitting in the middle of a
// multi-byte sequence.
func validUTF8BoundaryAt(s string, i int) bool {
	if i == 0 || i == len(s) {
		return true
	}
	b := s[i]
	// Continuation bytes have the high bits 10xxxxxx.
	return b&0xC0 != 0x80
}

// chooseCloseIndex returns the index in curr at which to close
// the current chunk. Default is len(curr) (close at the end).
// If the chunk has a markdown heading within the last
// overlapRatio fraction, close just before that heading so the
// heading starts the next chunk's overlap region — keeping
// section boundaries aligned with chunk boundaries when feasible.
func chooseCloseIndex(curr []string, overlapRatio float64) int {
	if len(curr) == 0 {
		return 0
	}
	lookback := max(int(float64(len(curr))*overlapRatio), 1)
	for i := len(curr) - 1; i >= len(curr)-lookback && i >= 1; i-- {
		if isMarkdownHeading(curr[i]) {
			return i
		}
	}
	return len(curr)
}

// isMarkdownHeading reports whether a line is a level 1-3
// markdown heading (`# `, `## `, `### `). Lower levels (4-6)
// are not used for snap targets because they're usually too
// fine-grained to make useful chunk boundaries.
func isMarkdownHeading(line string) bool {
	trimmed := strings.TrimLeft(line, "#")
	hashes := len(line) - len(trimmed)
	if hashes < 1 || hashes > 3 {
		return false
	}
	return strings.HasPrefix(trimmed, " ")
}

// takeOverlapTail returns the tail of `lines` whose cumulative
// token estimate is at most overlapTokens. Used to carry context
// from a closed chunk into the next chunk so the running-summary
// path observes some continuity at boundaries.
func takeOverlapTail(lines []string, overlapTokens int) []string {
	if overlapTokens <= 0 {
		return nil
	}
	used := 0
	startIdx := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		t := memory.EstimateTokens(lines[i])
		if used+t > overlapTokens {
			break
		}
		used += t
		startIdx = i
	}
	if startIdx >= len(lines) {
		return nil
	}
	out := make([]string, len(lines)-startIdx)
	copy(out, lines[startIdx:])
	return out
}

// sumTokens returns the sum of EstimateTokens over each line.
func sumTokens(lines []string) int {
	total := 0
	for _, l := range lines {
		total += memory.EstimateTokens(l)
	}
	return total
}
