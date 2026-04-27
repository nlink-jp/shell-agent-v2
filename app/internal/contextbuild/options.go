// Package contextbuild assembles the LLM-bound message list from a Session.
// It is sized to the active backend's budget, with older portions condensed
// via cached summaries that are content-keyed for safe reuse.
//
// Design: docs/en/memory-architecture-v2.md
//
// This package is dormant in Phase 1: it has no callers in the agent loop.
// Phase 2 wires it in behind a config flag.
package contextbuild

import (
	"context"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// SummarizeFunc condenses a span of records to a single summary string.
// The agent supplies this — usually a small LLM call against the active
// backend. Returning an error is non-fatal; the builder proceeds with
// the raw records that did fit and renders no summary block.
type SummarizeFunc func(ctx context.Context, records []memory.Record) (string, error)

// BuildOptions controls the assembly. Zero values are reasonable for tests
// (no budget enforcement, no summarization).
type BuildOptions struct {
	// SystemPrompt is the fully-rendered system block (already includes
	// temporal context, pinned, findings — those formatters live in their
	// own packages and produce text by the time we get here).
	SystemPrompt string

	// MaxContextTokens caps the total request size. 0 = unlimited.
	MaxContextTokens int

	// MaxToolResultTokens truncates a single tool record at render time.
	// 0 = no truncation.
	MaxToolResultTokens int

	// OutputReserve tokens are subtracted from MaxContextTokens to leave
	// room for the model's response. 0 disables.
	OutputReserve int

	// SummarizerID identifies the summarizer (backend + model). Used as
	// part of the cache key so summaries from different summarizers don't
	// pollute each other.
	SummarizerID string

	// Summarize is invoked for the older tail. nil disables summarization
	// (older records are simply dropped when over budget).
	Summarize SummarizeFunc

	// Now overrides time.Now() for deterministic tests. Zero = use time.Now().
	Now time.Time

	// Loc is the time zone for rendered timestamps. Nil = time.Local.
	Loc *time.Location

	// WrapUserToolContent, if set, is applied to the content of every
	// user and tool record before token estimation. The agent uses this
	// for prompt-injection guard wrapping. Identity-equivalent if nil.
	WrapUserToolContent func(string) string
}

func (o *BuildOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

func (o *BuildOptions) loc() *time.Location {
	if o.Loc == nil {
		return time.Local
	}
	return o.Loc
}

// BuildResult is what Build returns to the caller.
type BuildResult struct {
	Messages       []llm.Message
	TotalTokens    int
	IncludedRaw    int  // count of raw records included
	SummarizedSpan int  // count of records folded into the summary
	UsedCache      bool // true if the summary was served from cache
}

// EstimateTokens is re-exported from memory so callers can compute budgets.
func EstimateTokens(s string) int { return memory.EstimateTokens(s) }
