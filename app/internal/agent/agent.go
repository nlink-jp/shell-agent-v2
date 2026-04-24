// Package agent implements the core agent state machine and execution loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nlink-jp/shell-agent-v2/internal/chat"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
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

	streamHandler StreamHandler
}

// New creates a new Agent with the given configuration.
func New(cfg *config.Config) *Agent {
	a := &Agent{
		cfg:      cfg,
		state:    StateIdle,
		findings: findings.NewStore(),
		chat:     chat.New(defaultSystemPrompt),
	}
	a.setBackend(cfg.LLM.DefaultBackend)
	_ = a.findings.Load()
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
			"", // TODO: pinned context
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

		// No tool calls — final response
		if len(resp.ToolCalls) == 0 {
			a.session.AddAssistantMessage(resp.Content)
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
	default:
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}
}

func (a *Agent) buildToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        "resolve-date",
			Description: "Resolve relative date expressions to absolute dates. Use when you need to calculate dates like 'last Thursday', '3 weeks ago', 'first Monday of last month'.",
			Parameters:  chat.ResolveDateToolDef(),
		},
	}
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

const defaultSystemPrompt = `You are a helpful assistant with data analysis capabilities.
You can use tools to help answer questions. When asked about dates, use the resolve-date tool if you are unsure about the calculation.`
