// Package chat handles message building, system prompt construction,
// and temporal context injection.
package chat

import (
	"fmt"
	"strings"
	"time"

	"github.com/nlink-jp/nlk/guard"
	"github.com/nlink-jp/nlk/strip"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Engine builds LLM messages from session state.
type Engine struct {
	systemPrompt string
	location     string
	guardTag     guard.Tag
}

// New creates a new chat Engine.
func New(systemPrompt string) *Engine {
	return &Engine{
		systemPrompt: systemPrompt,
		guardTag:     guard.NewTag(),
	}
}

// SetLocation sets the user's location for temporal context.
// Input is sanitized to prevent prompt injection: newlines stripped,
// length capped at 200 chars.
func (e *Engine) SetLocation(location string) {
	e.location = sanitizeSystemContext(location, 200)
}

// sanitizeSystemContext strips characters that could be used for
// prompt injection when content is concatenated into the system prompt.
// Removes control chars and newlines, caps length.
func sanitizeSystemContext(s string, maxLen int) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteRune(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue // strip other control chars
		}
		b.WriteRune(r)
		if b.Len() >= maxLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// BuildMessages constructs the message array for the API call,
// injecting temporal context, pinned memory, and findings.
// User and tool content is wrapped with guard tags for prompt injection defense.
func (e *Engine) BuildMessages(session *memory.Session, pinnedContext, findingsContext string) []llm.Message {
	// Rotate guard nonce each call
	e.guardTag = guard.NewTag()

	timeContext := buildTemporalContext()
	if e.location != "" {
		timeContext += "\nLocation: " + e.location
	}
	fullSystem := fmt.Sprintf("%s\n\n%s", e.systemPrompt, timeContext)
	if pinnedContext != "" {
		fullSystem += "\n\nImportant facts you remember about the user:\n" + pinnedContext
	}
	if findingsContext != "" {
		fullSystem += "\n\nAnalysis findings from other sessions:\n" + findingsContext
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: fullSystem},
	}

	// Pass application-level roles as-is.
	// Each backend handles role mapping internally.
	// Design: docs/en/llm-abstraction.md Section 4
	for _, r := range session.Records {
		content := r.Content
		// Guard user and tool content against prompt injection
		if r.Role == "user" || r.Role == "tool" {
			if wrapped, err := e.guardTag.Wrap(content); err == nil {
				content = wrapped
			}
		}
		messages = append(messages, llm.Message{
			Role:      llm.Role(r.Role),
			Content:   content,
			ImageURLs: r.ImageURLs,
			ToolName:  r.ToolName,
		})
	}

	return messages
}

// BuildOptions controls context budget for BuildMessagesWithBudget.
type BuildOptions struct {
	MaxConversationTokens int // total budget for conversation messages (0 = unlimited)
	MaxWarmTokens         int // budget for warm summaries (0 = unlimited)
	MaxToolResultTokens   int // per-tool-result truncation (0 = unlimited)
}

// BuildResult contains the built messages and diagnostics.
type BuildResult struct {
	Messages     []llm.Message
	TotalTokens  int
	DroppedCount int // number of hot records that didn't fit
}

// BuildMessagesWithBudget constructs messages within a token budget.
// Newest messages are preserved; oldest are dropped first.
// [Calling: ...] messages are excluded from LLM context.
// Tool results are truncated to MaxToolResultTokens.
// Design: docs/en/agent-data-flow.md Section 3.3
func (e *Engine) BuildMessagesWithBudget(session *memory.Session, pinnedContext, findingsContext string, opts BuildOptions) BuildResult {
	e.guardTag = guard.NewTag()

	// 1. Build system prompt (same as BuildMessages)
	timeContext := buildTemporalContext()
	if e.location != "" {
		timeContext += "\nLocation: " + e.location
	}
	fullSystem := fmt.Sprintf("%s\n\n%s", e.systemPrompt, timeContext)
	if pinnedContext != "" {
		fullSystem += "\n\nImportant facts you remember about the user:\n" + pinnedContext
	}
	if findingsContext != "" {
		fullSystem += "\n\nAnalysis findings from other sessions:\n" + findingsContext
	}

	systemTokens := memory.EstimateTokens(fullSystem)

	// 2. Collect warm and hot records separately
	var warmRecords, hotRecords []memory.Record
	for _, r := range session.Records {
		switch r.Tier {
		case memory.TierWarm, memory.TierCold:
			warmRecords = append(warmRecords, r)
		default:
			hotRecords = append(hotRecords, r)
		}
	}

	// 3. Build warm summary messages (truncated to MaxWarmTokens)
	var warmMessages []llm.Message
	warmTokens := 0
	for _, r := range warmRecords {
		content := r.Content
		if opts.MaxWarmTokens > 0 {
			content = truncateToTokens(content, opts.MaxWarmTokens-warmTokens)
		}
		tokens := memory.EstimateTokens(content)
		if opts.MaxWarmTokens > 0 && warmTokens+tokens > opts.MaxWarmTokens {
			break
		}
		warmMessages = append(warmMessages, llm.Message{
			Role:    llm.Role(r.Role),
			Content: content,
		})
		warmTokens += tokens
	}

	// 4. Build hot messages newest-first, within budget
	remainingTokens := 0
	if opts.MaxConversationTokens > 0 {
		remainingTokens = opts.MaxConversationTokens - systemTokens - warmTokens
		if remainingTokens < 0 {
			remainingTokens = 0
		}
	}

	var selectedHot []llm.Message
	hotTokens := 0
	dropped := 0

	for i := len(hotRecords) - 1; i >= 0; i-- {
		r := hotRecords[i]

		// Skip [Calling: ...] messages — not needed for LLM context
		if r.Role == "assistant" && strings.HasPrefix(r.Content, "[Calling:") {
			continue
		}

		content := r.Content

		// Truncate tool results
		if r.Role == "tool" && opts.MaxToolResultTokens > 0 {
			content = truncateToTokens(content, opts.MaxToolResultTokens)
		}

		// Guard user and tool content
		if r.Role == "user" || r.Role == "tool" {
			if wrapped, err := e.guardTag.Wrap(content); err == nil {
				content = wrapped
			}
		}

		tokens := memory.EstimateTokens(content)

		// Check budget
		if opts.MaxConversationTokens > 0 && hotTokens+tokens > remainingTokens {
			dropped++
			continue
		}

		selectedHot = append(selectedHot, llm.Message{
			Role:      llm.Role(r.Role),
			Content:   content,
			ImageURLs: r.ImageURLs,
			ToolName:  r.ToolName,
		})
		hotTokens += tokens
	}

	// 5. Reverse to chronological order
	for i, j := 0, len(selectedHot)-1; i < j; i, j = i+1, j-1 {
		selectedHot[i], selectedHot[j] = selectedHot[j], selectedHot[i]
	}

	// 6. Assemble: system + warm + hot
	messages := make([]llm.Message, 0, 1+len(warmMessages)+len(selectedHot))
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: fullSystem})
	messages = append(messages, warmMessages...)
	messages = append(messages, selectedHot...)

	return BuildResult{
		Messages:     messages,
		TotalTokens:  systemTokens + warmTokens + hotTokens,
		DroppedCount: dropped,
	}
}

// truncateToTokens truncates text to approximately maxTokens.
func truncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	tokens := memory.EstimateTokens(text)
	if tokens <= maxTokens {
		return text
	}
	// Approximate: cut by ratio
	ratio := float64(maxTokens) / float64(tokens)
	cutAt := int(float64(len(text)) * ratio)
	if cutAt >= len(text) {
		return text
	}
	return text[:cutAt] + "\n...(truncated)"
}

// CleanResponse removes thinking tags from LLM output.
func CleanResponse(content string) string {
	return strip.ThinkTags(content)
}

// buildTemporalContext creates enriched date/time context for the LLM.
func buildTemporalContext() string {
	now := time.Now()
	_, offset := now.Zone()
	offsetHours := offset / 3600
	offsetMins := (offset % 3600) / 60

	yesterday := now.AddDate(0, 0, -1)

	return fmt.Sprintf(
		"Current date and time: %s (%s) %s (UTC%+03d:%02d)\nYesterday: %s (%s)",
		now.Format("2006-01-02"),
		now.Format("Monday"),
		now.Format("15:04:05"),
		offsetHours, offsetMins,
		yesterday.Format("2006-01-02"),
		yesterday.Format("Monday"),
	)
}
