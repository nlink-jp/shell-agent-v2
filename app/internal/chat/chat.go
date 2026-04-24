// Package chat handles message building, system prompt construction,
// and temporal context injection.
package chat

import (
	"fmt"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Engine builds LLM messages from session state.
type Engine struct {
	systemPrompt string
}

// New creates a new chat Engine.
func New(systemPrompt string) *Engine {
	return &Engine{systemPrompt: systemPrompt}
}

// BuildMessages constructs the message array for the API call,
// injecting temporal context, pinned memory, and findings.
func (e *Engine) BuildMessages(session *memory.Session, pinnedContext, findingsContext string) []llm.Message {
	timeContext := buildTemporalContext()
	fullSystem := fmt.Sprintf("%s\n\n%s", e.systemPrompt, timeContext)
	if pinnedContext != "" {
		fullSystem += "\n\nImportant facts you remember about the user:\n" + pinnedContext
	}
	if findingsContext != "" {
		fullSystem += "\n\nAnalysis findings from other sessions:\n" + findingsContext
	}

	messages := []llm.Message{
		{Role: "system", Content: fullSystem},
	}

	for _, r := range session.Records {
		messages = append(messages, llm.Message{
			Role:    r.Role,
			Content: r.Content,
		})
	}

	return messages
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
