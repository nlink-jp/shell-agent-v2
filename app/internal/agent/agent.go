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
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
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

	streamHandler StreamHandler
	titleHandler  TitleHandler
	mitlHandler   MITLHandler
	toolRegistry  *toolcall.Registry
}

// New creates a new Agent with the given configuration.
func New(cfg *config.Config) *Agent {
	registry := toolcall.NewRegistry()
	_ = registry.ScanDir(cfg.Tools.ScriptDir)

	a := &Agent{
		cfg:          cfg,
		state:        StateIdle,
		findings:     findings.NewStore(),
		pinned:       memory.NewPinnedStore(),
		chat:         chat.New(defaultSystemPrompt),
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

// CurrentBackend returns the name of the active LLM backend.
func (a *Agent) CurrentBackend() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.backend == nil {
		return ""
	}
	return a.backend.Name()
}

// Send processes a user message. Returns ErrBusy if the agent is not idle.
func (a *Agent) Send(ctx context.Context, message string) (string, error) {
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

	// Handle chat commands
	if strings.HasPrefix(message, "/") {
		return a.handleCommand(message)
	}

	return a.agentLoop(ctx, message)
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
	return nil
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

// --- internal ---

func (a *Agent) agentLoop(ctx context.Context, userMessage string) (string, error) {
	if a.session == nil {
		a.session = &memory.Session{ID: "default", Records: []memory.Record{}}
	}

	// Add user message to session
	a.session.AddUserMessage(userMessage)

	tools := a.buildToolDefs()

	for round := 0; round < maxToolRounds; round++ {
		if err := ctx.Err(); err != nil {
			return "(Cancelled)", nil
		}

		messages := a.chat.BuildMessages(
			a.session,
			a.pinned.FormatForPrompt(),
			a.findings.FormatForPrompt(),
		)

		resp, err := a.backend.ChatStream(ctx, messages, tools, func(token string, toolCalls []llm.ToolCall, done bool) {
			a.mu.Lock()
			h := a.streamHandler
			a.mu.Unlock()
			if h != nil && token != "" {
				h(token, done)
			}
		})
		if err != nil {
			return "", fmt.Errorf("LLM: %w", err)
		}

		// Clean thinking tags from response
		resp.Content = chat.CleanResponse(resp.Content)

		// No tool calls — final response
		if len(resp.ToolCalls) == 0 {
			a.session.AddAssistantMessage(resp.Content)
			go a.generateTitleIfNeeded(ctx)
			return resp.Content, nil
		}

		// Record assistant message with tool calls
		a.session.AddAssistantMessage(resp.Content)

		// Execute tool calls
		for _, tc := range resp.ToolCalls {
			result := a.executeTool(tc)
			a.session.AddToolResult(tc.ID, tc.Name, result)
		}
	}

	return "(Max tool rounds reached)", nil
}

func (a *Agent) executeTool(tc llm.ToolCall) string {
	switch tc.Name {
	case "resolve-date":
		result, err := chat.ResolveDate(tc.Arguments)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	case "load-data", "describe-data", "query-sql", "list-tables", "reset-analysis", "promote-finding":
		if a.analysis == nil {
			return "Error: no analysis engine available"
		}
		result, err := a.executeAnalysisTool(tc.Name, tc.Arguments)
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

When asked about dates, use the resolve-date tool if you are unsure about the calculation.

When you discover a significant analysis insight (a pattern, anomaly, or conclusion that would be valuable across sessions), use the promote-finding tool to save it to the global findings store. This allows other sessions to reference the insight without re-analyzing the data.`
