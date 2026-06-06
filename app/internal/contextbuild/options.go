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
	//
	// Returning an error aborts the whole Build — see
	// security-hardening-2.md L1 for why we fail closed rather than
	// silently feeding unwrapped untrusted content into the LLM
	// context.
	WrapUserToolContent func(string) (string, error)

	// ObjectLookup resolves a Record.DocumentIDs entry to the metadata
	// the document-anchor helper needs (name + tokens). nil → no
	// anchor prepending (legacy / test paths). The agent supplies a
	// closure over objstore.Store at message-build time so the lookup
	// always sees the freshest metadata (Load() backfill may have
	// updated tokens since the record was written).
	ObjectLookup llm.ObjectMetaLookup

	// UserRecordTemporalPrefix, if set, is called per user-role
	// record during message rendering and the result is prepended to
	// that record's content (after guard wrapping). nil disables the
	// feature for test / legacy paths.
	//
	// The renderer must be deterministic in record.Timestamp so that
	// identical records produce byte-identical bytes across
	// successive Build calls. That byte-stability is what lets the
	// LLM server's KV-cache prefix reuse fire across turns
	// (ADR-0017).
	//
	// Records whose Timestamp is the zero time are rendered without
	// a prefix — defensive against very old session bundles where
	// the field may be missing.
	UserRecordTemporalPrefix func(ts time.Time) string

	// --- ADR-0032 inputs --------------------------------------

	// AnchorSources lists the Fact strings of every decision /
	// preference Global Memory entry. Build uses these (via
	// memory.AnchorRecord) to lift records whose content lexically
	// matches an anchor out of the summary input and into a
	// verbatim block between the summaries and the raw records.
	// Empty / nil disables anchor extraction.
	AnchorSources []string

	// DeadTopicSources lists the Fact strings of every dormant /
	// archived Session Memory entry. Build uses these (via
	// memory.DeadTopicRecord, with LiveTopicSources as the safety
	// net) to drop records that reference a topic that is no longer
	// relevant to the current conversation.
	DeadTopicSources []string

	// LiveTopicSources lists the Fact strings of every fresh /
	// active Session Memory entry. Used as the live-clause safety
	// net in dead-topic detection — a record that also references a
	// live topic stays, even if it overlaps a dead one.
	LiveTopicSources []string

	// FarSummaryShare / NearSummaryShare are the fractions of the
	// available budget (MaxContextTokens - SystemPrompt -
	// OutputReserve) allocated to each summary tier's output. The
	// remainder is shared between anchored records and raw records.
	// Zero falls back to ADR-0032 §3.4 defaults (0.05 / 0.15).
	FarSummaryShare  float64
	NearSummaryShare float64

	// AnchorJaccardThreshold / DeadTopicJaccardThreshold are the
	// lexical thresholds for lift / drop decisions. Zero falls back
	// to ADR-0032 §3.4 defaults (0.4 each).
	AnchorJaccardThreshold    float64
	DeadTopicJaccardThreshold float64
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
	Messages    []llm.Message
	TotalTokens int
	IncludedRaw int // count of raw records included

	// SummarizedSpan and UsedCache are backward-compat aggregates
	// over the two ADR-0032 tiers: SummarizedSpan ==
	// NearSummarizedSpan + FarSummarizedSpan, UsedCache ==
	// NearCacheHit && FarCacheHit.
	SummarizedSpan int
	UsedCache      bool

	// --- ADR-0032 per-tier breakdown ---------------------------

	NearSummarizedSpan int
	FarSummarizedSpan  int
	NearCacheHit       bool
	FarCacheHit        bool
	AnchoredRecords    int // count of records lifted out as anchors
	DroppedDeadTopics  int // count of records suppressed by dead-topic drop
}

// EstimateTokens is re-exported from memory so callers can compute budgets.
func EstimateTokens(s string) int { return memory.EstimateTokens(s) }
