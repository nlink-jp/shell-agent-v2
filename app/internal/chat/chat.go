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
	systemPrompt    string
	location        string
	sandboxEnabled  bool
	guardTag        guard.Tag
}

// New creates a new chat Engine.
// toLLMToolCalls converts persisted Record.ToolCalls into the
// llm.ToolCall shape that backends emit on the wire (Vertex
// FunctionCall part / OpenAI tool_calls). Returns nil for an
// empty slice so the JSON `omitempty` on Message.ToolCalls
// drops the key for non-tool-calling assistant turns.
func toLLMToolCalls(rec []memory.ToolCallRecord) []llm.ToolCall {
	if len(rec) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, len(rec))
	for i, r := range rec {
		out[i] = llm.ToolCall{ID: r.ID, Name: r.Name, Arguments: r.Arguments}
	}
	return out
}

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

// SetSandboxEnabled toggles the sandbox-tool guidance section in
// BuildSystemPrompt. The agent calls this from maybeStartSandbox and
// RestartSandbox so the guidance shows up only when the sandbox-*
// tools are actually available — otherwise the LLM would hallucinate
// calls to tools that aren't there.
func (e *Engine) SetSandboxEnabled(enabled bool) {
	e.sandboxEnabled = enabled
}

// sandboxGuidance is the system-prompt section that appears when the
// sandbox is enabled. It tells the model how to chain the six
// sandbox-* tools so they aren't a black box at the start of a
// conversation.
const sandboxGuidance = `

A per-session container sandbox is available. Use it whenever the user asks you to run code, generate files, or do anything that has side effects you don't want on the host:
- sandbox-run-shell — run a shell command in the container; files in /work persist within this session
- sandbox-run-python — run Python code in the container
- sandbox-write-file — write text content to /work/<path> directly (avoids heredoc escaping)
- sandbox-copy-object — copy an object from the central store into /work so you can analyze it
- sandbox-register-object — register a /work file (chart, generated CSV, etc.) back into the central object store; the returned ID can be referenced from reports as ![alt](object:ID)
- sandbox-load-into-analysis — load a CSV/JSON/JSONL file from /work into the analysis database (DuckDB) as a queryable table. Use this after generating data with sandbox-run-python to make it available to query-sql / describe-data / suggest-analysis.
- sandbox-export-sql — run a SELECT query and write the result as CSV to /work/<file_path>. Use this whenever you want sandbox-run-python to operate on a query result; do NOT paste query-sql output text into Python code (lossy, wasteful, and the LLM will mistype large numbers).
- sandbox-info — describe the runtime (engine, image, Python version, installed pip packages, /work listing). Call this once early when you need to know what is preinstalled.

Decision rule for ingesting files into the analysis database: if the file lives under /work (i.e. you produced it via any sandbox-* tool), use sandbox-load-into-analysis. The host-side load-data tool CANNOT see /work and will fail with "no such file or directory" — do not retry it with different filename variants, switch tools.

Workflow tips: when a tool produces a file under /work, immediately call sandbox-register-object on it in the same response so it's available for reports and downstream tools. Don't only describe what you would do — emit the actual function call.`

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

// BuildSystemPrompt assembles the full system block (base prompt + temporal
// context + pinned + findings). Callers using contextbuild.Build directly
// pass this as BuildOptions.SystemPrompt instead of going through
// BuildMessages. The guard tag is rotated as a side effect, matching
// BuildMessages' behavior.
func (e *Engine) BuildSystemPrompt(pinnedContext, findingsContext string) string {
	e.guardTag = guard.NewTag()
	timeContext := buildTemporalContext()
	if e.location != "" {
		timeContext += "\nLocation: " + e.location
	}
	full := fmt.Sprintf("%s\n\n%s", e.systemPrompt, timeContext)
	if e.sandboxEnabled {
		full += sandboxGuidance
	}
	if pinnedContext != "" {
		full += "\n\nImportant facts you remember about the user:\n" + pinnedContext
	}
	if findingsContext != "" {
		full += "\n\nAnalysis findings from other sessions:\n" + findingsContext
	}
	return full
}

// WrapUserToolContent exposes the current guard tag's wrap function for
// callers that render records outside BuildMessages (e.g. contextbuild).
//
// Fail-closed: when guard.Wrap returns an error (essentially a
// crypto/rand-source catastrophe — the only realistic failure mode)
// we return the error rather than silently passing the unwrapped
// untrusted content through with elevated trust into the LLM
// system-prompt context (security-hardening-2.md L1).
func (e *Engine) WrapUserToolContent(s string) (string, error) {
	wrapped, err := e.guardTag.Wrap(s)
	if err != nil {
		return "", fmt.Errorf("guard wrap: %w", err)
	}
	return wrapped, nil
}

// BuildMessages constructs the message array for the API call,
// injecting temporal context, pinned memory, and findings.
// User and tool content is wrapped with guard tags for prompt
// injection defense.
//
// Fail-closed: a guard.Wrap failure returns an error rather than
// silently feeding unwrapped untrusted content into the LLM context
// (security-hardening-2.md L1).
func (e *Engine) BuildMessages(session *memory.Session, pinnedContext, findingsContext string) ([]llm.Message, error) {
	// Rotate guard nonce each call
	e.guardTag = guard.NewTag()

	timeContext := buildTemporalContext()
	if e.location != "" {
		timeContext += "\nLocation: " + e.location
	}
	fullSystem := fmt.Sprintf("%s\n\n%s", e.systemPrompt, timeContext)
	if e.sandboxEnabled {
		fullSystem += sandboxGuidance
	}
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
			wrapped, err := e.guardTag.Wrap(content)
			if err != nil {
				return nil, fmt.Errorf("guard wrap: %w", err)
			}
			content = wrapped
		}
		messages = append(messages, llm.Message{
			Role:       llm.Role(r.Role),
			Content:    content,
			ImageURLs:  r.ImageURLs,
			ObjectIDs:  r.ObjectIDs,
			ToolName:   r.ToolName,
			ToolCallID: r.ToolCallID,
			ToolCalls:  toLLMToolCalls(r.ToolCalls),
		})
	}

	return messages, nil
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
//
// Fail-closed: a guard.Wrap failure (essentially crypto/rand
// catastrophe) returns an error rather than feeding unwrapped
// untrusted content into the LLM context
// (security-hardening-2.md L1).
//
// Design: docs/en/agent-data-flow.md Section 3.3
func (e *Engine) BuildMessagesWithBudget(session *memory.Session, pinnedContext, findingsContext string, opts BuildOptions) (BuildResult, error) {
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

		// Guard user and tool content (fail-closed; security-hardening-2.md L1)
		if r.Role == "user" || r.Role == "tool" {
			wrapped, err := e.guardTag.Wrap(content)
			if err != nil {
				return BuildResult{}, fmt.Errorf("guard wrap: %w", err)
			}
			content = wrapped
		}

		tokens := memory.EstimateTokens(content)

		// Check budget
		if opts.MaxConversationTokens > 0 && hotTokens+tokens > remainingTokens {
			dropped++
			continue
		}

		selectedHot = append(selectedHot, llm.Message{
			Role:       llm.Role(r.Role),
			Content:    content,
			ImageURLs:  r.ImageURLs,
			ObjectIDs:  r.ObjectIDs,
			ToolName:   r.ToolName,
			ToolCallID: r.ToolCallID,
			ToolCalls:  toLLMToolCalls(r.ToolCalls),
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
	}, nil
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
//
// We deliberately do NOT auto-rewrite the input-anchor shape
// `Image (object ID: <hex>):` to markdown image references here —
// it would mangle legitimate prose where the model is talking
// ABOUT an ID rather than asking to render it (e.g. "Image
// (object ID: abc) is missing", explanations of the anchor
// format itself, code-block quoting, IDs followed by filename
// suffixes). Defense relies on the system-prompt rule alone,
// which explicitly forbids emitting the anchor shape in output.
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
