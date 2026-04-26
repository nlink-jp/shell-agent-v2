// Package agent implements the core agent state machine and execution loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/chat"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/toolcall"
)

// State represents the agent's execution state.
type State string

const (
	StateIdle State = "idle"
	StateBusy State = "busy"
)

const maxToolRounds = 10

// ErrBusy is returned when a message is sent while the agent is busy.
var ErrBusy = errors.New("agent is busy")

// StreamHandler receives streaming tokens from the agent.
type StreamHandler func(token string, done bool)

// TitleHandler is called when the session title is auto-generated.
type TitleHandler func(sessionID, title string)

// MITLRequest represents a tool call awaiting MITL approval.
type MITLRequest struct {
	ToolName  string `json:"tool_name"`
	Arguments string `json:"arguments"`
	Category  string `json:"category"`
}

// MITLHandler is called when a tool requires Man-In-The-Loop approval.
// Returns true if approved, false if rejected.
type MITLHandler func(req MITLRequest) bool

// Agent orchestrates chat, analysis, tool execution, and memory.
type Agent struct {
	cfg     *config.Config
	state   State
	mu      sync.Mutex
	cancel  context.CancelFunc

	backend  llm.Backend
	chat     *chat.Engine
	session  *memory.Session
	findings *findings.Store
	analysis *analysis.Engine
	pinned   *memory.PinnedStore
	objects  *objstore.Store

	streamHandler   StreamHandler
	titleHandler    TitleHandler
	mitlHandler     MITLHandler
	reportHandler   func(title, content string)
	pinnedHandler   func()
	progressHandler func(toolName string)
	toolRegistry    *toolcall.Registry

	// Token usage tracking (session-scoped, reset on session switch)
	promptTokens int
	outputTokens int
}

// New creates a new Agent with the given configuration.
func New(cfg *config.Config) *Agent {
	registry := toolcall.NewRegistry()
	_ = registry.ScanDir(cfg.Tools.ScriptDir)

	chatEngine := chat.New(defaultSystemPrompt)
	if cfg.Location != "" {
		chatEngine.SetLocation(cfg.Location)
	}

	a := &Agent{
		cfg:          cfg,
		state:        StateIdle,
		findings:     findings.NewStore(),
		pinned:       memory.NewPinnedStore(),
		chat:         chatEngine,
		toolRegistry: registry,
	}
	a.setBackend(cfg.LLM.DefaultBackend)
	_ = a.findings.Load()
	_ = a.pinned.Load()
	return a
}

// State returns the current agent state.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// SetStreamHandler sets the callback for streaming tokens.
func (a *Agent) SetStreamHandler(h StreamHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streamHandler = h
}

// SetTitleHandler sets the callback for auto-generated session titles.
func (a *Agent) SetTitleHandler(h TitleHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.titleHandler = h
}

// SetMITLHandler sets the callback for tool approval requests.
func (a *Agent) SetMITLHandler(h MITLHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mitlHandler = h
}

// SetReportHandler sets the callback for report creation.
func (a *Agent) SetReportHandler(h func(title, content string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reportHandler = h
}

// SetPinnedHandler sets the callback for pinned memory updates.
func (a *Agent) SetPinnedHandler(h func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pinnedHandler = h
}

// SetProgressHandler sets the callback for tool execution progress.
func (a *Agent) SetProgressHandler(h func(toolName string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.progressHandler = h
}

// CurrentBackend returns the name of the active LLM backend.
func (a *Agent) CurrentBackend() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.backend == nil {
		return ""
	}
	return a.backend.Name()
}

// CurrentSession returns the current session (for session ID access).
func (a *Agent) CurrentSession() *memory.Session {
	return a.session
}

// Send processes a user message. Returns ErrBusy if the agent is not idle.
func (a *Agent) Send(ctx context.Context, message string) (string, error) {
	return a.SendWithImages(ctx, message, nil, nil)
}

// SendWithImages processes a user message with optional images.
// objectIDs are stored in session records; dataURLs are used for LLM context.
func (a *Agent) SendWithImages(ctx context.Context, message string, objectIDs, dataURLs []string) (string, error) {
	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return "", ErrBusy
	}
	a.state = StateBusy
	ctx, a.cancel = context.WithCancel(ctx)
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.state = StateIdle
		a.cancel = nil
		a.mu.Unlock()
	}()

	// Handle chat commands (only known commands, not arbitrary /paths)
	if strings.HasPrefix(message, "/") {
		parts := strings.Fields(message)
		switch parts[0] {
		case "/model", "/finding", "/findings":
			return a.handleCommand(message)
		}
	}

	return a.agentLoop(ctx, message, objectIDs, dataURLs)
}

// Abort cancels the current task.
func (a *Agent) Abort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// Close releases all resources held by the agent.
func (a *Agent) Close() {
	a.Abort()
}

// LoadSession switches to the given session. Must be called in Idle state.
func (a *Agent) LoadSession(session *memory.Session) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != StateIdle {
		return ErrBusy
	}
	a.session = session
	a.promptTokens = 0
	a.outputTokens = 0
	return nil
}

// SetObjects sets the object store reference.
func (a *Agent) SetObjects(store *objstore.Store) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.objects = store
}

// SetAnalysis sets the analysis engine for the current session.
func (a *Agent) SetAnalysis(engine *analysis.Engine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.analysis = engine
}

// Findings returns all global findings.
func (a *Agent) Findings() []findings.Finding {
	return a.findings.All()
}

// ToolInfoItem describes a tool for listing.
type ToolInfoItem struct {
	Name        string
	Description string
	Category    string
	Source      string
}

// ListTools returns all available tools with metadata.
func (a *Agent) ListTools() []ToolInfoItem {
	var items []ToolInfoItem

	// Builtin tools
	items = append(items, ToolInfoItem{Name: "resolve-date", Description: "Resolve relative date expressions", Category: "read", Source: "builtin"})

	// Analysis tools
	hasData := a.analysis != nil && a.analysis.HasData()
	items = append(items, ToolInfoItem{Name: "load-data", Description: "Load CSV/JSON/JSONL file", Category: "read", Source: "analysis"})
	items = append(items, ToolInfoItem{Name: "reset-analysis", Description: "Drop all tables", Category: "write", Source: "analysis"})
	items = append(items, ToolInfoItem{Name: "create-report", Description: "Create markdown report", Category: "read", Source: "analysis"})
	if hasData {
		items = append(items, ToolInfoItem{Name: "describe-data", Description: "Show table metadata", Category: "read", Source: "analysis"})
		items = append(items, ToolInfoItem{Name: "query-sql", Description: "Execute SQL query", Category: "read", Source: "analysis"})
		items = append(items, ToolInfoItem{Name: "query-preview", Description: "NL to SQL generation", Category: "read", Source: "analysis"})
		items = append(items, ToolInfoItem{Name: "suggest-analysis", Description: "Suggest analysis perspectives", Category: "read", Source: "analysis"})
		items = append(items, ToolInfoItem{Name: "quick-summary", Description: "Query + LLM summary", Category: "read", Source: "analysis"})
		items = append(items, ToolInfoItem{Name: "list-tables", Description: "List all tables", Category: "read", Source: "analysis"})
		items = append(items, ToolInfoItem{Name: "promote-finding", Description: "Save insight to findings", Category: "write", Source: "analysis"})
	}

	// Shell script tools
	for _, t := range a.toolRegistry.All() {
		items = append(items, ToolInfoItem{Name: t.Name, Description: t.Description, Category: string(t.Category), Source: "shell"})
	}

	return items
}

// PinnedAll returns all pinned facts.
func (a *Agent) PinnedAll() []memory.PinnedFact {
	return a.pinned.All()
}

// PinnedSet creates or updates a pinned fact.
func (a *Agent) PinnedSet(key, content string) error {
	a.pinned.Set(key, content)
	return a.pinned.Save()
}

// PinnedDelete removes a pinned fact.
func (a *Agent) PinnedDelete(key string) error {
	a.pinned.Delete(key)
	return a.pinned.Save()
}

// LLMStatus returns current LLM and memory status.
func (a *Agent) LLMStatus() struct {
	Backend       string `json:"backend"`
	HotMessages   int    `json:"hot_messages"`
	WarmSummaries int    `json:"warm_summaries"`
	SessionID     string `json:"session_id"`
	PromptTokens  int    `json:"prompt_tokens"`
	OutputTokens  int    `json:"output_tokens"`
} {
	hot, warm := 0, 0
	sessionID := ""
	if a.session != nil {
		sessionID = a.session.ID
		for _, r := range a.session.Records {
			switch r.Tier {
			case memory.TierHot:
				hot++
			case memory.TierWarm:
				warm++
			}
		}
	}
	return struct {
		Backend       string `json:"backend"`
		HotMessages   int    `json:"hot_messages"`
		WarmSummaries int    `json:"warm_summaries"`
		SessionID     string `json:"session_id"`
		PromptTokens  int    `json:"prompt_tokens"`
		OutputTokens  int    `json:"output_tokens"`
	}{
		Backend:       a.backend.Name(),
		HotMessages:   hot,
		WarmSummaries: warm,
		SessionID:     sessionID,
		PromptTokens:  a.promptTokens,
		OutputTokens:  a.outputTokens,
	}
}

// --- internal ---

// agentLoop implements the core agent execution loop.
// Design: docs/en/agent-data-flow.md Section 2.2
func (a *Agent) agentLoop(ctx context.Context, userMessage string, objectIDs, dataURLs []string) (string, error) {
	if a.session == nil {
		a.session = &memory.Session{ID: "default", Records: []memory.Record{}}
	}

	logger.Info("agentLoop: session=%s message=%s objects=%d", a.session.ID, logger.Truncate(userMessage, 100), len(objectIDs))

	// Step 1: Add user message to session
	// ObjectIDs stored in record for persistence; dataURLs used for LLM context
	a.session.AddUserMessage(userMessage)
	if len(objectIDs) > 0 || len(dataURLs) > 0 {
		last := &a.session.Records[len(a.session.Records)-1]
		last.ObjectIDs = objectIDs
		last.ImageURLs = dataURLs // kept for LLM context (BuildMessages)
	}
	_ = a.session.Save() // auto-save after user message

	allTools := a.buildToolDefs()
	logger.Debug("agentLoop: %d tools available", len(allTools))
	toolsExecutedLastRound := false

	// Step 2: Agent loop (max rounds)
	for round := 0; round < maxToolRounds; round++ {
		if err := ctx.Err(); err != nil {
			return "(Cancelled)", nil
		}

		// LM Studio spec: after tool execution, call WITHOUT tools
		// to force text response. Never re-enable tools after empty response.
		var tools []llm.ToolDef
		if toolsExecutedLastRound {
			tools = nil
		} else {
			tools = allTools
		}

		messages := a.chat.BuildMessages(
			a.session,
			a.pinned.FormatForPrompt(),
			a.findings.FormatForPrompt(),
		)

		logger.Debug("agentLoop: round=%d messages=%d tools=%d backend=%s", round, len(messages), len(tools), a.backend.Name())

		var resp *llm.Response
		var err error

		if tools == nil && a.streamHandler != nil {
			// Final text response round (after tool execution): stream tokens
			// to the frontend for real-time display. tools=nil guarantees the
			// LLM won't produce proper tool calls. Any residual gemma-style
			// text tool tags are cleaned from the accumulated response below;
			// the streaming preview is ephemeral and replaced by the clean message.
			resp, err = a.backend.ChatStream(ctx, messages, nil, func(token string, _ []llm.ToolCall, done bool) {
				a.streamHandler(token, done)
			})
		} else {
			// Tool-calling round or no stream handler: non-streaming.
			// Prevents gemma tool call markup from leaking to the UI.
			resp, err = a.backend.Chat(ctx, messages, tools)
		}
		if err != nil {
			logger.Error("agentLoop: LLM error: %v", err)
			return "", fmt.Errorf("LLM: %w", err)
		}

		// Accumulate token usage
		a.promptTokens += resp.PromptTokens
		a.outputTokens += resp.OutputTokens

		// Clean response: thinking tags + gemma text tool calls (every round)
		resp.Content = chat.CleanResponse(resp.Content)
		resp.Content = stripGemmaToolCallTags(resp.Content)
		resp.Content = strings.TrimSpace(resp.Content)
		logger.Debug("agentLoop: response content=%s toolCalls=%d", logger.Truncate(resp.Content, 200), len(resp.ToolCalls))

		// --- No tool calls: final response or empty ---
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				a.session.AddAssistantMessage(resp.Content)
				_ = a.session.Save()
			} else {
				logger.Debug("agentLoop: empty final response, ending loop")
			}
			// Post-response background tasks
			a.postResponseTasks(ctx)
			return resp.Content, nil
		}

		// --- Tool calls present ---
		// Record assistant's tool call request in session.
		// This is REQUIRED for valid conversation history — LM Studio expects
		// an assistant message with tool_call info before tool result messages.
		// Without this, the LLM doesn't know tools were already called and
		// tries to call them again.
		toolNames := make([]string, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			toolNames[i] = tc.Name
		}
		assistantContent := resp.Content
		if assistantContent == "" {
			assistantContent = fmt.Sprintf("[Calling: %s]", strings.Join(toolNames, ", "))
		}
		a.session.AddAssistantMessage(assistantContent)

		// Execute each tool call
		for _, tc := range resp.ToolCalls {
			if a.progressHandler != nil {
				a.progressHandler(tc.Name)
			}
			logger.Info("agentLoop: tool_call name=%s args=%s", tc.Name, logger.Truncate(tc.Arguments, 200))
			result := a.executeTool(ctx, tc)
			logger.Debug("agentLoop: tool_result name=%s result=%s", tc.Name, logger.Truncate(result, 200))
			a.session.AddToolResult(tc.ID, tc.Name, result)
		}
		_ = a.session.Save() // auto-save after tool execution
		toolsExecutedLastRound = true
	}

	logger.Debug("agentLoop: max rounds (%d) reached", maxToolRounds)
	return "(Max tool rounds reached)", nil
}

// postResponseTasks runs background tasks after a final response.
// Design: docs/en/agent-data-flow.md Section 4.1
func (a *Agent) postResponseTasks(ctx context.Context) {
	go a.generateTitleIfNeeded(ctx)
	go a.compactMemoryIfNeeded(ctx)
	go a.extractPinnedMemories(ctx)
}

// compactMemoryIfNeeded summarizes old hot messages when token budget exceeded.
// Design: docs/en/agent-data-flow.md Section 4.2
func (a *Agent) compactMemoryIfNeeded(ctx context.Context) {
	if a.session == nil {
		return
	}

	summarizer := func(c context.Context, text string) (string, error) {
		messages := []llm.Message{
			{Role: "system", Content: "Summarize the following conversation segment concisely. Preserve key facts, decisions, and context. Use the same language as the conversation."},
			{Role: "user", Content: text},
		}
		resp, err := a.backend.Chat(c, messages, nil)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}

	compacted, err := a.session.CompactIfNeeded(ctx, memory.CompactOptions{
		HotTokenLimit: a.cfg.Memory.HotTokenLimit,
		Summarizer:    summarizer,
	})
	if err != nil {
		logger.Error("compactMemory: %v", err)
		return
	}
	if compacted {
		logger.Info("compactMemory: compacted session %s", a.session.ID)
		_ = a.session.Save()
	}
}

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) string {
	switch tc.Name {
	case "resolve-date":
		result, err := chat.ResolveDate(tc.Arguments)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	case "list-objects":
		return a.toolListObjects(tc.Arguments)
	case "get-object":
		return a.toolGetObject(tc.Arguments)
	case "load-data", "describe-data", "query-sql", "query-preview", "suggest-analysis", "quick-summary", "list-tables", "reset-analysis", "create-report", "promote-finding":
		if a.analysis == nil {
			return "Error: no analysis engine available"
		}
		result, err := a.executeAnalysisTool(ctx, tc.Name, tc.Arguments)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	default:
		// Check shell script tool registry
		if tool, ok := a.toolRegistry.Get(tc.Name); ok {
			// MITL check for write/execute tools
			if tool.NeedsMITL() {
				a.mu.Lock()
				h := a.mitlHandler
				a.mu.Unlock()
				if h != nil {
					approved := h(MITLRequest{
						ToolName:  tc.Name,
						Arguments: tc.Arguments,
						Category:  string(tool.Category),
					})
					if !approved {
						return "Tool execution rejected by user."
					}
				}
			}
			result, err := toolcall.Execute(context.Background(), tool, tc.Arguments)
			if err != nil {
				return fmt.Sprintf("Error: %v", err)
			}
			return result
		}
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}
}

func (a *Agent) buildToolDefs() []llm.ToolDef {
	tools := []llm.ToolDef{
		{
			Name:        "resolve-date",
			Description: "Resolve relative date expressions to absolute dates. Use when you need to calculate dates like 'last Thursday', '3 weeks ago', 'first Monday of last month'.",
			Parameters:  chat.ResolveDateToolDef(),
		},
	}

	// Add analysis tools (dynamically filtered by data presence)
	if a.analysis != nil {
		tools = append(tools, analysisTools(a.analysis.HasData())...)
	}

	// Add shell script tools from registry
	for _, t := range a.toolRegistry.All() {
		tools = append(tools, llm.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.ToolDefParams(),
		})
	}

	return tools
}

func (a *Agent) handleCommand(message string) (string, error) {
	parts := strings.Fields(message)
	cmd := parts[0]

	switch cmd {
	case "/model":
		return a.handleModelCommand(parts[1:])
	case "/finding":
		return a.handleFindingCommand(parts[1:])
	case "/findings":
		return a.handleFindingsCommand()
	default:
		return fmt.Sprintf("Unknown command: %s", cmd), nil
	}
}

func (a *Agent) handleModelCommand(args []string) (string, error) {
	if len(args) == 0 {
		return fmt.Sprintf("Current backend: %s\nAvailable: local, vertex", a.backend.Name()), nil
	}

	target := args[0]
	switch target {
	case "local":
		a.setBackend(config.BackendLocal)
		return "Switched to local LLM backend.", nil
	case "vertex":
		a.setBackend(config.BackendVertexAI)
		return "Switched to Vertex AI backend.", nil
	default:
		return fmt.Sprintf("Unknown backend: %s. Available: local, vertex", target), nil
	}
}

func (a *Agent) handleFindingCommand(args []string) (string, error) {
	if len(args) == 0 {
		return "Usage: /finding <text to remember>", nil
	}
	content := strings.Join(args, " ")
	sessionID := ""
	sessionTitle := ""
	if a.session != nil {
		sessionID = a.session.ID
		sessionTitle = a.session.Title
	}
	f := a.findings.Add(content, sessionID, sessionTitle, nil)
	if err := a.findings.Save(); err != nil {
		return "", fmt.Errorf("save finding: %w", err)
	}
	return fmt.Sprintf("Finding saved: %s (%s)", f.Content, f.CreatedLabel), nil
}

func (a *Agent) handleFindingsCommand() (string, error) {
	all := a.findings.All()
	if len(all) == 0 {
		return "No findings yet.", nil
	}
	var sb strings.Builder
	for _, f := range all {
		data, _ := json.Marshal(f)
		sb.WriteString(string(data))
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

func (a *Agent) setBackend(backend config.LLMBackend) {
	switch backend {
	case config.BackendVertexAI:
		a.backend = llm.NewVertex(a.cfg.LLM.VertexAI)
	default:
		a.backend = llm.NewLocal(a.cfg.LLM.Local)
	}
}

// generateTitleIfNeeded generates a session title from the first user message.
func (a *Agent) generateTitleIfNeeded(ctx context.Context) {
	if a.session == nil || a.session.Title != "New Session" {
		return
	}

	var firstUser string
	for _, r := range a.session.Records {
		if r.Role == "user" && r.Tier == memory.TierHot {
			firstUser = r.Content
			break
		}
	}
	if firstUser == "" {
		return
	}

	messages := []llm.Message{
		{Role: "system", Content: "Generate a very short title (under 30 chars) for a chat that starts with the following message. Reply with ONLY the title, no quotes, no explanation. Use the same language as the message."},
		{Role: "user", Content: firstUser},
	}

	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		return
	}

	title := strings.TrimSpace(resp.Content)
	if title == "" || len(title) > 60 {
		return
	}

	a.session.Title = title
	_ = a.session.Save()

	a.mu.Lock()
	h := a.titleHandler
	a.mu.Unlock()
	if h != nil {
		h(a.session.ID, title)
	}
}

const defaultSystemPrompt = `You are a helpful assistant with data analysis capabilities.
You can use tools to help answer questions.

Before calling a tool, briefly explain what you are about to do and why. For example:
- Before query-sql: show the SQL you will execute and explain the intent
- Before load-data: explain which file you will load and what table name you will use
- Before suggest-analysis or query-preview: explain the analysis perspective
This helps the user understand and verify your approach.

When asked about dates, use the resolve-date tool if you are unsure about the calculation.

When you discover a significant analysis insight (a pattern, anomaly, or conclusion that would be valuable across sessions), use the promote-finding tool to save it to the global findings store.

When the user asks you to create a report, summary document, or formatted output, you MUST use the create-report tool. Do not write the report as a chat message — always call the create-report tool so the report is properly structured and rendered with full markdown support.

When the user shares images in the conversation, the image data is included in the conversation context.

To reference images or other objects from the session:
1. Use the list-objects tool to discover available objects (images, reports, files)
2. Use the get-object tool to retrieve an object by its ID
3. In reports, reference images with: ![description](object:ID)
Never fabricate image URLs or object IDs. Always use list-objects first to find valid IDs.`

// extractPinnedMemories runs after each response to auto-extract important facts.
// This is a system task, not an LLM tool — the backend drives the extraction.
func (a *Agent) extractPinnedMemories(ctx context.Context) {
	if a.session == nil {
		return
	}

	// Collect last 4 hot messages for analysis
	var recentRecords []memory.Record
	for _, r := range a.session.Records {
		if r.Tier == memory.TierHot {
			recentRecords = append(recentRecords, r)
		}
	}
	if len(recentRecords) > 4 {
		recentRecords = recentRecords[len(recentRecords)-4:]
	}
	if len(recentRecords) < 2 {
		return // need at least a user + assistant exchange
	}

	// Build conversation text for extraction
	var conversation strings.Builder
	for _, r := range recentRecords {
		if r.Role == "tool" {
			continue
		}
		conversation.WriteString(fmt.Sprintf("[%s]: %s\n", r.Role, r.Content))
	}

	existing := a.pinned.FormatExistingForExtraction()

	messages := []llm.Message{
		{Role: "system", Content: `Analyze the conversation below and extract important facts worth remembering long-term.
Categories: preference, decision, fact, context
Rules:
- Only extract genuinely important, reusable information
- Skip greetings, small talk, and transient details
- If nothing is important, respond with exactly: NONE
- Otherwise respond with one fact per line in format: category|english fact|native language expression
  Example: preference|User prefers Go over Python|ユーザーはPythonよりGoを好む
- The native language expression should match the language the user used in the conversation
- If the conversation is already in English, the native expression can be the same as the English fact
- Do not repeat facts already known
Already known:
` + existing},
		{Role: "user", Content: conversation.String()},
	}

	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		logger.Error("extractPinnedMemories: %v", err)
		return
	}

	text := strings.TrimSpace(resp.Content)
	if text == "" || strings.ToUpper(text) == "NONE" {
		return
	}

	added := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}
		category := strings.TrimSpace(parts[0])
		fact := strings.TrimSpace(parts[1])
		native := ""
		if len(parts) >= 3 {
			native = strings.TrimSpace(parts[2])
		}
		if fact == "" {
			continue
		}

		if a.pinned.Add(memory.PinnedFact{
			Fact:       fact,
			NativeFact: native,
			Category:   category,
		}) {
			added++
		}
	}

	if added > 0 {
		logger.Info("extractPinnedMemories: added %d facts", added)
		_ = a.pinned.Save()

		// Notify frontend
		a.mu.Lock()
		h := a.pinnedHandler
		a.mu.Unlock()
		if h != nil {
			h()
		}
	}
}

// stripGemmaToolCallTags removes gemma-style text tool call tags from content.
// These occur when the model outputs tool calls as text instead of structured API calls.
func stripGemmaToolCallTags(text string) string {
	result := text
	for {
		start := strings.Index(result, "<|tool_call>")
		if start < 0 {
			start = strings.Index(result, "<tool_call>")
			if start < 0 {
				break
			}
		}

		end := strings.Index(result[start:], "<tool_call|>")
		endLen := len("<tool_call|>")
		if end < 0 {
			end = strings.Index(result[start:], "</tool_call>")
			endLen = len("</tool_call>")
			if end < 0 {
				// No closing tag — strip from start to end of string
				result = result[:start]
				break
			}
		}
		result = result[:start] + result[start+end+endLen:]
	}
	return strings.TrimSpace(result)
}
