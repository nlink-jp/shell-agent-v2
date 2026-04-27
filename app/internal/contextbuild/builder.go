package contextbuild

import (
	"context"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Build assembles the LLM-bound message list from a session per the
// algorithm described in memory-architecture-v2.md §6.
//
// Caller responsibilities:
//   - Provide the fully-rendered system prompt (pinned/findings already
//     formatted into it).
//   - Supply a SummarizerID and Summarize callback if summarization is
//     desired.
//   - Persist the cache via SummaryCache.Save after Build returns when
//     UsedCache is false (a new entry was added).
func Build(ctx context.Context, session *memory.Session, cache *SummaryCache, opts BuildOptions) BuildResult {
	res := BuildResult{}

	msgs := []llm.Message{}
	if opts.SystemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: opts.SystemPrompt})
	}
	sysTokens := EstimateTokens(opts.SystemPrompt)

	if session == nil || len(session.Records) == 0 {
		res.Messages = msgs
		res.TotalTokens = sysTokens
		return res
	}

	// Filter the records:
	//   - "summary" records are legacy compaction output; they participate
	//     only as opaque older-tail content (handled below).
	//   - "[Calling: ...]" assistant messages are placeholder markers used
	//     when the LLM emitted a tool call without text. Including them in
	//     the LLM context teaches gemma-style models to mimic the pattern
	//     as text instead of using the real tool API.
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
		raw = append(raw, r)
	}

	budget := opts.MaxContextTokens - sysTokens - opts.OutputReserve

	// Walk newest → oldest, accumulate raw rendered records until budget.
	type rendered struct {
		idx       int
		role      string
		content   string
		tokens    int
		toolName  string
		imageURLs []string
	}
	var acc []rendered
	used := 0
	splitIdx := len(raw) // first index NOT included; equals len(raw) means all included

	for i := len(raw) - 1; i >= 0; i-- {
		content := renderRecordContent(raw, i, opts)
		t := EstimateTokens(content)
		if opts.MaxContextTokens > 0 && used+t > budget && len(acc) > 0 {
			splitIdx = i + 1
			break
		}
		acc = append([]rendered{{
			idx: i, role: raw[i].Role, content: content, tokens: t,
			toolName:  raw[i].ToolName,
			imageURLs: raw[i].ImageURLs,
		}}, acc...)
		used += t
		splitIdx = i
	}

	older := raw[:splitIdx]

	// Build the summary block from older + legacy summaries that fall in
	// or before the older tail. Cache key is computed from `older` only;
	// legacy summaries are appended as their own headed sub-blocks.
	if shouldSummarize(older, legacy) {
		summaryBlock, fromCache := assembleSummary(ctx, older, legacy, cache, opts)
		if summaryBlock != "" {
			msgs = append(msgs, llm.Message{Role: llm.RoleSummary, Content: summaryBlock})
			used += EstimateTokens(summaryBlock)
		}
		res.UsedCache = fromCache
		res.SummarizedSpan = len(older)
	}

	for _, a := range acc {
		msgs = append(msgs, llm.Message{
			Role:      llm.Role(a.role),
			Content:   a.content,
			ToolName:  a.toolName,
			ImageURLs: a.imageURLs,
		})
	}

	res.Messages = msgs
	res.TotalTokens = used + sysTokens
	res.IncludedRaw = len(acc)
	return res
}

func shouldSummarize(older, legacy []memory.Record) bool {
	return len(older) > 0 || len(legacy) > 0
}

// assembleSummary produces the rendered summary content for the older
// tail. It checks the cache for a matching range first; on miss, calls
// the summarizer (if provided) and stores the result. Legacy summary
// records contained in or preceding the older tail are appended as
// their own sub-blocks with their own range headers.
func assembleSummary(ctx context.Context, older, legacy []memory.Record, cache *SummaryCache, opts BuildOptions) (string, bool) {
	var blocks []string
	usedCache := false

	for _, lg := range legacy {
		from, to := lg.Timestamp, lg.Timestamp
		count := 1
		if lg.SummaryRange != nil {
			from, to = lg.SummaryRange.From, lg.SummaryRange.To
		}
		header := renderSummaryHeader(from, to, count, opts.loc())
		blocks = append(blocks, header+"\n"+lg.Content)
	}

	if len(older) > 0 {
		key := ComputeRangeKey(older, opts.SummarizerID)
		if entry := cache.Get(key); entry != nil {
			header := renderSummaryHeader(entry.FromTimestamp, entry.ToTimestamp, entry.RecordCount, opts.loc())
			blocks = append(blocks, header+"\n"+entry.Summary)
			usedCache = true
		} else if opts.Summarize != nil {
			summary, err := opts.Summarize(ctx, older)
			if err == nil && summary != "" {
				entry := SummaryEntry{
					RangeKey:      key,
					SummarizerID:  opts.SummarizerID,
					FromTimestamp: older[0].Timestamp,
					ToTimestamp:   older[len(older)-1].Timestamp,
					RecordCount:   len(older),
					Summary:       summary,
					CreatedAt:     opts.now(),
				}
				if cache != nil {
					cache.Put(entry)
				}
				header := renderSummaryHeader(entry.FromTimestamp, entry.ToTimestamp, entry.RecordCount, opts.loc())
				blocks = append(blocks, header+"\n"+summary)
			}
		}
		// If no summarizer and no cache, the older tail is silently dropped
		// (with the legacy blocks still rendered if any).
	}

	if len(blocks) == 0 {
		return "", false
	}
	out := blocks[0]
	for i := 1; i < len(blocks); i++ {
		out += "\n\n" + blocks[i]
	}
	return out, usedCache
}
