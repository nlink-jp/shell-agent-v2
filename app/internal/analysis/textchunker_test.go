package analysis

import (
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestTextChunker_TokenBudget verifies that no chunk grossly
// exceeds the target token count. We allow up to ~1.5× over the
// soft target (the chunker is line-atomic, so a single long line
// can push past the boundary by itself), but anything wildly
// larger means the budget logic is broken.
func TestTextChunker_TokenBudget(t *testing.T) {
	body := strings.Repeat("This is a fairly normal sentence about something.\n", 500)
	cfg := DefaultChunkerConfig()
	cfg.TargetTokens = 500 // smaller for test speed
	chunks, err := ChunkText(body, cfg)
	if err != nil {
		t.Fatalf("ChunkText: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected ≥1 chunk")
	}
	for i, c := range chunks {
		tokens := memory.EstimateTokens(c)
		if tokens > int(float64(cfg.TargetTokens)*1.5) {
			t.Errorf("chunk %d: tokens=%d exceeds 1.5× target=%d", i, tokens, cfg.TargetTokens)
		}
	}
}

// TestTextChunker_LineAtomic ensures no chunk boundary falls
// inside a line — boundaries always land between '\n'-separated
// lines. We check that each chunk, when re-split on '\n', has
// matching pieces in the original content (each piece appears
// somewhere as a whole line in the input).
func TestTextChunker_LineAtomic(t *testing.T) {
	// Lines are distinguishable so we can detect partial cuts.
	var sb strings.Builder
	for range 50 {
		sb.WriteString("Line ")
		sb.WriteString(strings.Repeat("x", 80))
		sb.WriteString("\n")
	}
	body := sb.String()
	cfg := DefaultChunkerConfig()
	cfg.TargetTokens = 300
	chunks, err := ChunkText(body, cfg)
	if err != nil {
		t.Fatalf("ChunkText: %v", err)
	}
	for ci, c := range chunks {
		for piece := range strings.SplitSeq(c, "\n") {
			if piece == "" {
				continue
			}
			// Each piece must appear as a complete line in the
			// original. We assert that the piece is followed
			// either by '\n' (somewhere in body) or is the
			// last line (body ends with the piece + optional '\n').
			if !strings.Contains(body, piece+"\n") && !strings.HasSuffix(body, piece) {
				t.Errorf("chunk %d contains non-atomic line: %q", ci, piece)
			}
		}
	}
}

// TestTextChunker_HeadingAware verifies that, when a heading
// sits within the lookback window before a natural chunk
// boundary, the boundary snaps so that the heading starts the
// next chunk. With headings every ~300 tokens of body, we
// expect most chunks (excluding the first) to start with `#`.
func TestTextChunker_HeadingAware(t *testing.T) {
	var sb strings.Builder
	for range 6 {
		sb.WriteString("# Section X\n\n")
		// Roughly 300 tokens of filler.
		sb.WriteString(strings.Repeat("Filler line that adds tokens to push the chunker boundary.\n", 80))
	}
	body := sb.String()
	cfg := DefaultChunkerConfig()
	cfg.TargetTokens = 400
	chunks, err := ChunkText(body, cfg)
	if err != nil {
		t.Fatalf("ChunkText: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(chunks))
	}
	startsWithHeading := 0
	for i, c := range chunks {
		firstNonOverlap := firstNonEmptyLine(c)
		if i > 0 && strings.HasPrefix(firstNonOverlap, "# ") {
			startsWithHeading++
		}
	}
	// At minimum we expect most non-first chunks to begin
	// with a heading (the snap fires within 10% of target).
	if startsWithHeading == 0 {
		t.Errorf("heading-aware snap did not fire for any chunk; chunks: %d", len(chunks))
		for i, c := range chunks {
			t.Logf("chunk %d first non-empty line: %q", i, firstNonEmptyLine(c))
		}
	}
}

func firstNonEmptyLine(c string) string {
	for l := range strings.SplitSeq(c, "\n") {
		if l != "" {
			return l
		}
	}
	return ""
}

// TestTextChunker_LongLineBackstop verifies a pathological
// single line gets force-broken at MaxLineWidth so it can't
// dominate a single chunk.
func TestTextChunker_LongLineBackstop(t *testing.T) {
	// One ~50 KB line, no newlines.
	body := strings.Repeat("abcdefghij", 5000) // 50,000 chars
	cfg := DefaultChunkerConfig()
	cfg.MaxLineWidth = 10000
	cfg.TargetTokens = 4000 // generous so the test focuses on width breaking
	chunks, err := ChunkText(body, cfg)
	if err != nil {
		t.Fatalf("ChunkText: %v", err)
	}
	// The 50k char line should be broken into ~5 pieces.
	// Multiple chunks may also be needed if pieces exceed
	// TargetTokens; we just assert ≥1 and that no chunk
	// contains the entire 50k-char string.
	if len(chunks) < 1 {
		t.Fatal("expected ≥1 chunk")
	}
	for i, c := range chunks {
		// Each "line" inside a chunk (split by '\n') must be
		// at most MaxLineWidth chars.
		for piece := range strings.SplitSeq(c, "\n") {
			if len(piece) > cfg.MaxLineWidth {
				t.Errorf("chunk %d has line of %d chars > MaxLineWidth=%d", i, len(piece), cfg.MaxLineWidth)
			}
		}
	}
}

// TestTextChunker_RespectsTotalChunkCap pins the safety
// backstop: a document that would produce more than MaxChunks
// chunks returns an error with the design's hint string.
func TestTextChunker_RespectsTotalChunkCap(t *testing.T) {
	// Tight cap to keep the test fast.
	cfg := ChunkerConfig{
		TargetTokens: 50, // small target → many chunks
		OverlapRatio: 0.1,
		MaxLineWidth: 1000,
		MaxChunks:    5,
	}
	var sb strings.Builder
	for range 1000 {
		sb.WriteString("This sentence has enough tokens to fill a small chunk by itself.\n")
	}
	_, err := ChunkText(sb.String(), cfg)
	if err == nil {
		t.Fatal("expected MaxChunks error, got nil")
	}
	if !strings.Contains(err.Error(), "chunk cap") {
		t.Errorf("error wording = %q, want contains 'chunk cap'", err.Error())
	}
}

// TestTextChunker_EmptyInput — degenerate boundary: empty
// content yields nil, no error.
func TestTextChunker_EmptyInput(t *testing.T) {
	chunks, err := ChunkText("", DefaultChunkerConfig())
	if err != nil {
		t.Fatalf("empty input must not error: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("empty input chunks = %d, want 0", len(chunks))
	}
}

// TestTextChunker_SmallDocSingleChunk — a small document that
// fits in TargetTokens should return exactly one chunk.
func TestTextChunker_SmallDocSingleChunk(t *testing.T) {
	body := "# Hello\n\nThis is a short document.\n"
	chunks, err := ChunkText(body, DefaultChunkerConfig())
	if err != nil {
		t.Fatalf("ChunkText: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0] != strings.TrimSuffix(body, "\n") && chunks[0] != body {
		// Allow trailing-newline normalization variance.
		// Just make sure all content is present.
		for line := range strings.SplitSeq(body, "\n") {
			if line == "" {
				continue
			}
			if !strings.Contains(chunks[0], line) {
				t.Errorf("small-doc chunk missing line: %q", line)
			}
		}
	}
}
