package contextbuild

import (
	"context"
	"fmt"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Build assembles the LLM-bound message list from a session per the
// algorithm described in ADR-0032 (replacing the original
// memory-architecture-v2.md §6 single-tier model).
//
// Caller responsibilities:
//   - Provide the fully-rendered system prompt (pinned/findings already
//     formatted into it).
//   - Supply AnchorSources / DeadTopicSources / LiveTopicSources derived
//     from the live memory stores; Build does not reach into stores.
//   - Supply a SummarizerID and Summarize callback if summarization is
//     desired.
//   - Persist the cache via SummaryCache.Save after Build returns when
//     any tier reports a cache miss.
//
// toLLMToolCalls mirrors chat.toLLMToolCalls — converts persisted
// Record.ToolCalls into the llm.ToolCall shape backends emit on the
// wire. Returns nil for an empty slice.
func toLLMToolCalls(rec []memory.ToolCallRecord) []llm.ToolCall {
	if len(rec) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, len(rec))
	for i, r := range rec {
		out[i] = llm.ToolCall{
			ID:               r.ID,
			Name:             r.Name,
			Arguments:        r.Arguments,
			ThoughtSignature: r.ThoughtSignature,
		}
	}
	return out
}

// ADR-0032 default knobs. Mirror config.ContextBudgetConfig's
// defaults; the same numbers also appear in the ADR §3.4.
const (
	defaultFarSummaryShare           = 0.05
	defaultNearSummaryShare          = 0.15
	defaultAnchorJaccardThreshold    = 0.4
	defaultDeadTopicJaccardThreshold = 0.4
)

// resolveDefaults fills zero-valued ADR-0032 knobs with defaults so a
// caller (e.g. test path) that constructs BuildOptions with only the
// pre-ADR-0032 fields still gets sensible compaction behaviour.
func resolveDefaults(opts BuildOptions) BuildOptions {
	if opts.FarSummaryShare <= 0 {
		opts.FarSummaryShare = defaultFarSummaryShare
	}
	if opts.NearSummaryShare <= 0 {
		opts.NearSummaryShare = defaultNearSummaryShare
	}
	if opts.AnchorJaccardThreshold <= 0 {
		opts.AnchorJaccardThreshold = defaultAnchorJaccardThreshold
	}
	if opts.DeadTopicJaccardThreshold <= 0 {
		opts.DeadTopicJaccardThreshold = defaultDeadTopicJaccardThreshold
	}
	return opts
}

func tokenizeAll(facts []string) []map[string]struct{} {
	if len(facts) == 0 {
		return nil
	}
	out := make([]map[string]struct{}, len(facts))
	for i, f := range facts {
		out[i] = memory.TokenSet(f)
	}
	return out
}

// rendered is the bookkeeping struct for a single record that has
// passed token estimation and content rendering. It carries the
// fields llm.Message needs at assembly time.
type rendered struct {
	idx             int
	role            string
	content         string
	tokens          int
	toolName        string
	toolCallID      string
	toolCalls       []llm.ToolCall
	imageURLs       []string
	objectIDs       []string
	thoughtPartSigs [][]byte
	textPartSig     []byte
}

func renderOne(records []memory.Record, i int, opts BuildOptions) (rendered, error) {
	content, err := renderRecordContent(records, i, opts)
	if err != nil {
		return rendered{}, err
	}
	r := records[i]
	return rendered{
		idx: i, role: r.Role, content: content, tokens: EstimateTokens(content),
		toolName:        r.ToolName,
		toolCallID:      r.ToolCallID,
		toolCalls:       toLLMToolCalls(r.ToolCalls),
		imageURLs:       r.ImageURLs,
		objectIDs:       r.ObjectIDs,
		thoughtPartSigs: r.ThoughtPartSigs,
		textPartSig:     r.TextPartSig,
	}, nil
}

func toMessage(r rendered) llm.Message {
	return llm.Message{
		Role:            llm.Role(r.role),
		Content:         r.content,
		ToolName:        r.toolName,
		ToolCallID:      r.toolCallID,
		ToolCalls:       r.toolCalls,
		ImageURLs:       r.imageURLs,
		ObjectIDs:       r.objectIDs,
		ThoughtPartSigs: r.thoughtPartSigs,
		TextPartSig:     r.textPartSig,
	}
}

// liftAnchors scans the candidates and partitions them into anchor
// records (lifted out for verbatim rendering) and the remaining
// records (eligible for summary or dead-topic drop).
//
// anchorIdx is the list of original candidate-relative positions of
// the anchor records. It is supplied to ComputeContentKey so that a
// change in the anchor set invalidates the corresponding tier cache.
func liftAnchors(candidates []memory.Record, anchorSets []map[string]struct{}, threshold float64) (anchorRecs []memory.Record, anchorIdx []int, remaining []memory.Record) {
	if len(anchorSets) == 0 || threshold <= 0 {
		return nil, nil, candidates
	}
	for i, r := range candidates {
		if memory.AnchorRecord(r.Content, anchorSets, threshold) {
			anchorRecs = append(anchorRecs, r)
			anchorIdx = append(anchorIdx, i)
		} else {
			remaining = append(remaining, r)
		}
	}
	return anchorRecs, anchorIdx, remaining
}

// dropDeadTopics removes records that match the dormant Session
// Memory fact set AND do not also match the live set. The matched
// dead fact texts (deadFingerprints) are returned so they can feed
// the cache key, ensuring an invalidation when the drop set
// changes.
func dropDeadTopics(candidates []memory.Record, deadSets, liveSets []map[string]struct{}, deadFacts []string, threshold float64) (filtered []memory.Record, droppedCount int, deadFingerprints []string) {
	if len(deadSets) == 0 || threshold <= 0 {
		return candidates, 0, nil
	}
	hit := map[string]struct{}{}
	for _, r := range candidates {
		if !memory.DeadTopicRecord(r.Content, deadSets, liveSets, threshold) {
			filtered = append(filtered, r)
			continue
		}
		droppedCount++
		// Track which dead fact(s) caused the drop so the cache
		// key changes when the set of drop-causing facts changes.
		// We record any dead fact whose Jaccard exceeds the
		// threshold for this record — usually just one, but
		// multiple matches are possible.
		contentSet := memory.TokenSet(r.Content)
		for j, d := range deadSets {
			if memory.JaccardScore(contentSet, d) >= threshold {
				if _, seen := hit[deadFacts[j]]; !seen {
					hit[deadFacts[j]] = struct{}{}
				}
			}
		}
	}
	if len(hit) > 0 {
		deadFingerprints = make([]string, 0, len(hit))
		for f := range hit {
			deadFingerprints = append(deadFingerprints, f)
		}
	}
	return filtered, droppedCount, deadFingerprints
}

// partitionForTiers splits the post-drop summary input into the
// near (newer half) and far (older half) tier inputs. The split is
// by record count: simple, predictable, and reasonable for the v1
// of ADR-0032. A future refinement may switch to token-weighted
// splits if measurement reveals imbalance.
func partitionForTiers(remaining []memory.Record) (nearInput, farInput []memory.Record) {
	n := len(remaining)
	if n == 0 {
		return nil, nil
	}
	mid := n / 2
	return remaining[mid:], remaining[:mid]
}

// assembleTier produces one tier's summary block: cache lookup,
// LLM call on miss, header rendering. Returns the rendered block
// text, whether the cache served it, and the number of records
// folded into the summary.
//
// The dropped count is appended to the block as
// "[N dead-topic turns suppressed]" so the LLM has a quantitative
// hint that conversation moved on past topics that aren't listed.
func assembleTier(ctx context.Context, tier string, input []memory.Record, droppedCount int, deadFingerprints []string, anchorIndices []int, cache *SummaryCache, opts BuildOptions) (string, bool, int) {
	if len(input) == 0 && droppedCount == 0 {
		return "", false, 0
	}

	key := ComputeContentKey(input, opts.SummarizerID, deadFingerprints, anchorIndices, tier)

	var summaryText string
	fromCache := false
	if entry := cache.Get(key); entry != nil {
		summaryText = entry.Summary
		fromCache = true
	} else if opts.Summarize != nil && len(input) > 0 {
		out, err := opts.Summarize(ctx, input)
		if err == nil && strings.TrimSpace(out) != "" {
			summaryText = out
			if cache != nil {
				cache.Put(SummaryEntry{
					RangeKey:      key,
					Kind:          SummaryEntryKindContentV2,
					Tier:          tier,
					SummarizerID:  opts.SummarizerID,
					FromTimestamp: input[0].Timestamp,
					ToTimestamp:   input[len(input)-1].Timestamp,
					RecordCount:   len(input),
					Summary:       summaryText,
					CreatedAt:     opts.now(),
				})
			}
		}
	}

	var header string
	if len(input) > 0 {
		header = renderSummaryHeader(input[0].Timestamp, input[len(input)-1].Timestamp, len(input), opts.loc())
	}

	var sb strings.Builder
	if header != "" {
		sb.WriteString(fmt.Sprintf("[tier=%s] %s\n", tier, header))
	} else {
		sb.WriteString(fmt.Sprintf("[tier=%s]\n", tier))
	}
	if summaryText != "" {
		sb.WriteString(summaryText)
	} else if len(input) > 0 {
		// No summarizer available: surface an elision marker so the
		// LLM is not silently fed nothing in place of records that
		// were excluded from the raw window. Failing closed is
		// safer than dropping content without notice.
		sb.WriteString(fmt.Sprintf("[summary unavailable — %d records elided]", len(input)))
	}
	if droppedCount > 0 {
		if sb.Len() > 0 && !strings.HasSuffix(sb.String(), "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("[%d dead-topic turns suppressed]", droppedCount))
	}

	return sb.String(), fromCache, len(input)
}

func Build(ctx context.Context, session *memory.Session, cache *SummaryCache, opts BuildOptions) (BuildResult, error) {
	opts = resolveDefaults(opts)

	res := BuildResult{}
	msgs := []llm.Message{}
	if opts.SystemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: opts.SystemPrompt})
	}
	sysTokens := EstimateTokens(opts.SystemPrompt)

	if session == nil || len(session.Records) == 0 {
		res.Messages = msgs
		res.TotalTokens = sysTokens
		return res, nil
	}

	// Filter the records (same rules as the pre-ADR-0032 path):
	//   - "summary" records are legacy compaction output; they
	//     participate only as opaque older-tail content rendered
	//     ahead of the far summary block (preserves v0.1.x bundles).
	//   - "[Calling: ...]" assistant placeholders are dropped: they
	//     teach gemma-style models to mimic the placeholder as text
	//     instead of using the real tool API.
	//   - "report" records are a user-facing side effect of the
	//     create-report tool and are already represented by the
	//     matching tool result. Including the report content again
	//     as an assistant turn confused LM Studio's chat template
	//     (broken token output on the next turn).
	var raw []memory.Record
	var legacy []memory.Record
	for _, r := range session.Records {
		if r.Role == "summary" {
			legacy = append(legacy, r)
			continue
		}
		if r.Role == "assistant" && strings.HasPrefix(r.Content, "[Calling:") {
			continue
		}
		if r.Role == "report" {
			continue
		}
		raw = append(raw, r)
	}

	// Compute budgets. Tier shares come out of the available
	// budget; anchor records consume from the raw share (they are
	// verbatim, so they share the raw rendering path).
	available := opts.MaxContextTokens - sysTokens - opts.OutputReserve
	if available < 0 {
		available = 0
	}
	rawBudget := available
	if opts.MaxContextTokens > 0 {
		farBudget := int(float64(available) * opts.FarSummaryShare)
		nearBudget := int(float64(available) * opts.NearSummaryShare)
		rawBudget = available - farBudget - nearBudget
		// rawBudget is best-effort: summary tiers may underuse their
		// share, but we don't redistribute since the LLM-summarizer's
		// output size is not known ahead of time.
		_ = farBudget
		_ = nearBudget
	}

	// Walk newest -> oldest, fill raw budget.
	var rawAcc []rendered
	used := 0
	splitIdx := len(raw)
	for i := len(raw) - 1; i >= 0; i-- {
		r, err := renderOne(raw, i, opts)
		if err != nil {
			return BuildResult{}, err
		}
		if opts.MaxContextTokens > 0 && used+r.tokens > rawBudget && len(rawAcc) > 0 {
			splitIdx = i + 1
			break
		}
		rawAcc = append([]rendered{r}, rawAcc...)
		used += r.tokens
		splitIdx = i
	}

	candidates := raw[:splitIdx]

	// ADR-0032 §4.1-§4.2: lift anchors, drop dead, partition.
	anchorSets := tokenizeAll(opts.AnchorSources)
	deadSets := tokenizeAll(opts.DeadTopicSources)
	liveSets := tokenizeAll(opts.LiveTopicSources)

	anchorRecs, anchorIdx, postAnchors := liftAnchors(candidates, anchorSets, opts.AnchorJaccardThreshold)
	postDrop, droppedDead, deadFps := dropDeadTopics(postAnchors, deadSets, liveSets, opts.DeadTopicSources, opts.DeadTopicJaccardThreshold)
	nearInput, farInput := partitionForTiers(postDrop)

	// Far tier carries the drop count (the older half is where the
	// "history moved on" framing fits best). Anchor indices are
	// passed to BOTH tiers' cache keys since an anchor shift may
	// have been pulled out of either half.
	farBlock, farHit, farSpan := assembleTier(ctx, "far", farInput, droppedDead, deadFps, anchorIdx, cache, opts)
	nearBlock, nearHit, nearSpan := assembleTier(ctx, "near", nearInput, 0, deadFps, anchorIdx, cache, opts)

	// Assembly order: system → legacy v0.1.x summaries → far →
	// near → anchor records → raw records.
	for _, lg := range legacy {
		from, to := lg.Timestamp, lg.Timestamp
		count := 1
		if lg.SummaryRange != nil {
			from, to = lg.SummaryRange.From, lg.SummaryRange.To
		}
		header := renderSummaryHeader(from, to, count, opts.loc())
		msgs = append(msgs, llm.Message{Role: llm.RoleSummary, Content: header + "\n" + lg.Content})
		used += EstimateTokens(header + "\n" + lg.Content)
	}

	if farBlock != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSummary, Content: farBlock})
		used += EstimateTokens(farBlock)
	}
	if nearBlock != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSummary, Content: nearBlock})
		used += EstimateTokens(nearBlock)
	}

	// Anchor records: render verbatim into messages just before the
	// raw window so the LLM sees them as historical, anchored
	// context.
	for i, ar := range anchorRecs {
		r, err := renderOne(anchorRecs, i, opts)
		if err != nil {
			return BuildResult{}, err
		}
		// renderOne above reads from anchorRecs but its lookback
		// (e.g. document anchors) may not match the original
		// candidates context. For v1 this is acceptable; document
		// anchor rendering is identity-equivalent for an isolated
		// record.
		_ = ar
		msgs = append(msgs, toMessage(r))
		used += r.tokens
	}

	for _, a := range rawAcc {
		msgs = append(msgs, toMessage(a))
	}

	res.Messages = msgs
	res.TotalTokens = used + sysTokens
	res.IncludedRaw = len(rawAcc)
	res.AnchoredRecords = len(anchorRecs)
	res.DroppedDeadTopics = droppedDead
	res.NearSummarizedSpan = nearSpan
	res.FarSummarizedSpan = farSpan
	res.SummarizedSpan = nearSpan + farSpan
	res.NearCacheHit = nearHit
	res.FarCacheHit = farHit
	res.UsedCache = (nearSpan == 0 || nearHit) && (farSpan == 0 || farHit)
	return res, nil
}
