// Package agent implements the core agent state machine and execution loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nlink-jp/nlk/jsonfix"
	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/chat"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/contextbuild"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/mcp"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"
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

// ErrMITLRejected is returned by tool dispatchers when the user
// rejected the MITL prompt. The user-facing rejection string
// (carried alongside as the result text) is what the LLM sees in
// the tool_result; the sentinel error lets the agentLoop tag the
// activity as ActivityStatusError so the chat bubble shows red
// instead of a misleading green check.
var ErrMITLRejected = errors.New("agent: MITL rejected")

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

// MITLResponse is the user's decision on a MITL request.
type MITLResponse struct {
	Approved bool   `json:"approved"`
	Feedback string `json:"feedback"` // non-empty when rejected with reason
}

// MITLHandler is called when a tool requires Man-In-The-Loop approval.
type MITLHandler func(req MITLRequest) MITLResponse

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
	activityHandler func(ActivityEvent)
	toolRegistry    *toolcall.Registry
	guardians       map[string]*mcp.Guardian
	guardiansMu     sync.RWMutex
	mcpStatuses     []MCPStatus
	sandbox         sandbox.Engine // nil when disabled or no engine on PATH
	postTasksWg     sync.WaitGroup // ensures post-response tasks finish before next Send

	// Token usage tracking (session-scoped, reset on session switch)
	promptTokens int
	outputTokens int
}

// New creates a new Agent with the given configuration.
func New(cfg *config.Config) *Agent {
	registry := toolcall.NewRegistry()
	_ = registry.ScanDir(cfg.Tools.ScriptDir)
	logger.Info("shell tools: scanned %d from %s", len(registry.All()), cfg.Tools.ScriptDir)

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
		guardians:    make(map[string]*mcp.Guardian),
	}
	a.startGuardians()
	a.maybeStartSandbox()
	a.setBackend(cfg.LLM.DefaultBackend)
	_ = a.findings.Load()
	_ = a.pinned.Load()
	return a
}

// maybeStartSandbox initialises a.sandbox when Sandbox.Enabled is true
// and a container engine is on PATH. Failure is non-fatal — the
// sandbox-* tools just stay hidden. The chat engine is told whether
// the sandbox is up so the system-prompt sandbox guidance only shows
// when the tools actually exist.
func (a *Agent) maybeStartSandbox() {
	defer func() {
		if a.chat != nil {
			a.chat.SetSandboxEnabled(a.sandbox != nil)
		}
	}()
	if !a.cfg.Sandbox.Enabled {
		return
	}
	rs := a.cfg.ResolvedSandbox()
	eng, err := sandbox.NewCLI(sandbox.Config{
		Engine:         rs.Engine,
		Image:          rs.Image,
		Network:        rs.Network,
		CPULimit:       rs.CPULimit,
		MemoryLimit:    rs.MemoryLimit,
		TimeoutSeconds: rs.TimeoutSeconds,
		SessionsDir:    filepath.Join(config.DataDir(), "sessions"),
	})
	if err != nil {
		logger.Info("sandbox: %v — sandbox tools will be unavailable", err)
		return
	}
	a.sandbox = eng
	bin, _ := eng.Detect()
	logger.Info("sandbox: enabled (engine=%s, image=%s)", bin, rs.Image)
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

// ActivityEventStatus is a coarse outcome label attached to
// tool_end events so the chat UI can render success / failure
// distinctly. running events leave this empty; tool_start uses
// it as a "best guess" placeholder (callers may overwrite at
// tool_end time).
type ActivityEventStatus string

const (
	ActivityStatusSuccess ActivityEventStatus = "success"
	ActivityStatusError   ActivityEventStatus = "error"
)

// ActivityEvent describes a transient agent activity surfaced
// to the UI. Type is one of "tool_start" / "tool_end" /
// "thinking"; Detail is the tool name (or thinking content);
// Status is "" for tool_start / thinking and "success" /
// "error" for tool_end.
type ActivityEvent struct {
	Type   string
	Detail string
	Status ActivityEventStatus
}

// SetActivityHandler sets the callback for agent activity events.
// Replaces the previous func(actType, detail string) signature
// so a tool_end event can carry success / failure status without
// the bindings layer having to guess.
func (a *Agent) SetActivityHandler(h func(ActivityEvent)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activityHandler = h
}

// emitActivity is a small helper so we don't repeat the
// nil-check at every call site.
func (a *Agent) emitActivity(ev ActivityEvent) {
	if a.activityHandler != nil {
		a.activityHandler(ev)
	}
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

// buildMessagesV2 assembles the LLM messages via the contextbuild package
// (memory-architecture-v2.md). Opt-in via Memory.UseV2.
func (a *Agent) buildMessagesV2(ctx context.Context, budget config.ContextBudgetConfig) []llm.Message {
	cache, err := contextbuild.LoadCache(a.session.ID)
	if err != nil {
		logger.Error("buildMessagesV2: load cache: %v", err)
		cache = &contextbuild.SummaryCache{}
	}

	systemPrompt := a.chat.BuildSystemPrompt(
		a.pinned.FormatForPrompt(),
		a.findings.FormatForPrompt(),
	)

	summarize := func(c context.Context, records []memory.Record) (string, error) {
		var sb strings.Builder
		for _, r := range records {
			sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", r.Timestamp.Format("15:04"), r.Role, r.Content))
		}
		msgs := []llm.Message{
			{Role: llm.RoleSystem, Content: "Summarize the following conversation segment concisely. Preserve key facts, decisions, and context. Use the same language as the conversation."},
			{Role: llm.RoleUser, Content: sb.String()},
		}
		resp, err := a.backend.Chat(c, msgs, nil)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}

	res := contextbuild.Build(ctx, a.session, cache, contextbuild.BuildOptions{
		SystemPrompt:        systemPrompt,
		MaxContextTokens:    budget.MaxContextTokens,
		MaxToolResultTokens: budget.MaxToolResultTokens,
		OutputReserve:       4096,
		SummarizerID:        a.backend.Name() + "/" + a.currentModelName(),
		Summarize:           summarize,
		WrapUserToolContent: a.chat.WrapUserToolContent,
	})

	if !res.UsedCache && res.SummarizedSpan > 0 {
		if err := cache.Save(a.session.ID); err != nil {
			logger.Error("buildMessagesV2: save cache: %v", err)
		}
	}
	logger.Info("buildMessagesV2: raw=%d summarized=%d cache_hit=%v total_tokens=%d budget=%d",
		res.IncludedRaw, res.SummarizedSpan, res.UsedCache, res.TotalTokens, budget.MaxContextTokens)
	return res.Messages
}

// currentModelName returns the active backend's configured model string.
func (a *Agent) currentModelName() string {
	switch a.currentBackendKey() {
	case config.BackendVertexAI:
		return a.cfg.LLM.VertexAI.Model
	default:
		return a.cfg.LLM.Local.Model
	}
}

// currentBackendKey returns the active backend's config key.
// Caller must already hold a.mu, or accept a stale read.
func (a *Agent) currentBackendKey() config.LLMBackend {
	if a.backend == nil {
		return a.cfg.LLM.DefaultBackend
	}
	return config.LLMBackend(a.backend.Name())
}

// currentBudget returns the per-backend context budget for the active backend.
func (a *Agent) currentBudget() config.ContextBudgetConfig {
	return a.cfg.ContextBudgetFor(a.currentBackendKey())
}

// currentHotTokenLimit returns the per-backend hot tier compaction trigger.
func (a *Agent) currentHotTokenLimit() int {
	return a.cfg.HotTokenLimitFor(a.currentBackendKey())
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
	// Wait for any previous post-response tasks to complete
	a.postTasksWg.Wait()

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
		case "/model", "/finding", "/findings", "/help":
			result, err := a.handleCommand(message)
			if err != nil {
				return "", err
			}
			return "[CMD]" + result, nil
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
	a.stopGuardians()
	if a.sandbox != nil {
		_ = a.sandbox.StopAll(context.Background())
	}
}

// MCPStatus holds the status of a guardian for UI display.
type MCPStatus struct {
	Name      string `json:"name"`
	Status    string `json:"status"`    // "running", "disabled", "error"
	ToolCount int    `json:"tool_count"`
	Error     string `json:"error,omitempty"`
}

// startGuardians launches MCP guardian processes from config.
func (a *Agent) startGuardians() {
	a.mcpStatuses = nil
	for _, p := range a.cfg.Tools.MCPProfiles {
		if !p.Enabled || p.Name == "" || p.Binary == "" {
			a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "disabled"})
			continue
		}
		binary, err := validateBinaryPath(p.Binary)
		if err != nil {
			logger.Error("MCP guardian %q binary validation failed: %v", p.Name, err)
			a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()})
			continue
		}
		profile, err := validateProfilePath(p.ProfilePath)
		if err != nil {
			logger.Error("MCP guardian %q profile validation failed: %v", p.Name, err)
			a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()})
			continue
		}
		g := mcp.NewGuardian(binary, "--profile", profile)
		if err := g.Start(); err != nil {
			logger.Error("MCP guardian %q start failed: %v", p.Name, err)
			a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()})
			continue
		}
		a.guardians[p.Name] = g
		toolCount := len(g.Tools())
		a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "running", ToolCount: toolCount})
		logger.Info("MCP guardian %q started (%d tools)", p.Name, toolCount)
	}
}

// validateBinaryPath ensures the path resolves to an existing executable
// regular file. Prevents arbitrary command execution if config is corrupted.
func validateBinaryPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty binary path")
	}
	expanded := config.ExpandPath(path)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("binary not found: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", abs)
	}
	if info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("not executable: %s", abs)
	}
	return abs, nil
}

// validateProfilePath validates the --profile arg for mcp-guardian, which
// accepts either a bare profile name or a file path. For paths, verify the
// file exists; for bare names, pass through after rejecting control chars.
func validateProfilePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty profile path")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("control characters not allowed in profile")
		}
	}
	if strings.ContainsRune(path, '/') || strings.HasPrefix(path, "~") {
		expanded := config.ExpandPath(path)
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return "", fmt.Errorf("invalid path: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("profile not found: %w", err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("profile is a directory: %s", abs)
		}
		return abs, nil
	}
	return path, nil
}

// MCPStatuses returns the status of all configured MCP guardians.
func (a *Agent) MCPStatuses() []MCPStatus {
	return a.mcpStatuses
}

// stopGuardians stops all running MCP guardian processes.
func (a *Agent) stopGuardians() {
	a.guardiansMu.Lock()
	defer a.guardiansMu.Unlock()
	for name, g := range a.guardians {
		g.Stop()
		logger.Info("MCP guardian %q stopped", name)
	}
	a.guardians = make(map[string]*mcp.Guardian)
}

// RestartGuardians stops and restarts all MCP guardians from current config.
func (a *Agent) RestartGuardians() {
	a.stopGuardians()
	a.guardiansMu.Lock()
	a.guardians = make(map[string]*mcp.Guardian)
	a.guardiansMu.Unlock()
	a.startGuardians()
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

// DeleteFindingsBySession removes all findings originating from the given session.
func (a *Agent) DeleteFindingsBySession(sessionID string) {
	a.findings.DeleteBySession(sessionID)
	_ = a.findings.Save()
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

	// Sandbox tools (all treated as "execute")
	if a.sandbox != nil {
		for _, td := range sandboxToolDefs() {
			items = append(items, ToolInfoItem{Name: td.Name, Description: td.Description, Category: "execute", Source: "sandbox"})
		}
	}

	// MCP guardian tools (all treated as "execute" — external service operations)
	a.guardiansMu.RLock()
	for name, g := range a.guardians {
		for _, t := range g.Tools() {
			items = append(items, ToolInfoItem{
				Name:        "mcp__" + name + "__" + t.Name,
				Description: "[" + name + "] " + t.Description,
				Category:    "execute",
				Source:      "mcp",
			})
		}
	}
	a.guardiansMu.RUnlock()

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

// PinnedDeleteByKeys bulk-removes pinned facts. Returns the count actually deleted.
func (a *Agent) PinnedDeleteByKeys(keys []string) (int, error) {
	n := a.pinned.DeleteByKeys(keys)
	if n == 0 {
		return 0, nil
	}
	return n, a.pinned.Save()
}

// FindingsDeleteByIDs bulk-removes findings. Returns the count actually deleted.
func (a *Agent) FindingsDeleteByIDs(ids []string) (int, error) {
	n := a.findings.DeleteByIDs(ids)
	if n == 0 {
		return 0, nil
	}
	return n, a.findings.Save()
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

	// Avoid logging full user message at Info level (may contain sensitive data).
	logger.Info("agentLoop: session=%s message_len=%d objects=%d", a.session.ID, len(userMessage), len(objectIDs))
	logger.Debug("agentLoop: message=%s", logger.Truncate(userMessage, 100))

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

	// Synchronous compaction before entering the loop
	a.compactIfOverBudget(ctx)

	// Step 2: Agent loop (max rounds)
	for round := 0; round < maxToolRounds; round++ {
		if err := ctx.Err(); err != nil {
			return "(Cancelled)", nil
		}

		// Compact again after tool rounds (tool results inflate context)
		if round > 0 {
			a.compactIfOverBudget(ctx)
		}

		// Pass tools every round to allow tool chaining (e.g. get-location → weather).
		// Verified: gemma-4 does not loop even with tools always available.
		// [Calling:] contamination is handled by BuildMessagesWithBudget.
		tools := allTools

		// Build messages with [Calling:] exclusion and optional token budget.
		// [Calling:] exclusion prevents LLM from mimicking the pattern.
		// Token budget is an optional safety net (0 = unlimited).
		// Design: docs/en/agent-data-flow.md Section 3.3
		// V2: contextbuild package (memory-architecture-v2.md), opt-in.
		budget := a.currentBudget()
		var messages []llm.Message
		if a.cfg.Memory.UseV2 {
			messages = a.buildMessagesV2(ctx, budget)
		} else {
			buildResult := a.chat.BuildMessagesWithBudget(
				a.session,
				a.pinned.FormatForPrompt(),
				a.findings.FormatForPrompt(),
				chat.BuildOptions{
					MaxConversationTokens: budget.MaxContextTokens,
					MaxWarmTokens:         budget.MaxWarmTokens,
					MaxToolResultTokens:   budget.MaxToolResultTokens,
				},
			)
			messages = buildResult.Messages
			if buildResult.DroppedCount > 0 {
				logger.Debug("agentLoop: budget control dropped %d old messages (total ~%d tokens)", buildResult.DroppedCount, buildResult.TotalTokens)
			}
		}

		logger.Debug("agentLoop: round=%d messages=%d tools=%d backend=%s v2=%v", round, len(messages), len(tools), a.backend.Name(), a.cfg.Memory.UseV2)

		var resp *llm.Response
		var err error

		// Streaming disabled: tools are always present (tool chaining),
		// so we can't predict which round will be the final text response.
		// Local backend also has gemma tag leakage issues with streaming.
		canStream := false
		if canStream {
			resp, err = a.backend.ChatStream(ctx, messages, nil, func(token string, _ []llm.ToolCall, done bool) {
				a.streamHandler(token, done)
			})
		} else {
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

		// Emit thinking activity for LLM explanation text (transient, not a chat message)
		if resp.Content != "" {
			a.emitActivity(ActivityEvent{Type: "thinking", Detail: resp.Content})
		}

		// Execute each tool call
		for _, tc := range resp.ToolCalls {
			a.emitActivity(ActivityEvent{Type: "tool_start", Detail: tc.Name})
			// Avoid logging full tool arguments at Info level (may contain credentials, paths, etc.)
			logger.Info("agentLoop: tool_call name=%s args_len=%d", tc.Name, len(tc.Arguments))
			logger.Debug("agentLoop: tool_call args=%s", logger.Truncate(tc.Arguments, 200))
			result, status := a.executeTool(ctx, tc)
			logger.Debug("agentLoop: tool_result name=%s status=%s result=%s", tc.Name, status, logger.Truncate(result, 200))
			a.session.AddToolResult(tc.ID, tc.Name, result)
			a.emitActivity(ActivityEvent{Type: "tool_end", Detail: tc.Name, Status: status})
		}
		_ = a.session.Save() // auto-save after tool execution
	}

	logger.Debug("agentLoop: max rounds (%d) reached", maxToolRounds)
	return "(Max tool rounds reached)", nil
}

// postResponseTasks launches background tasks after a final response.
// Tasks run concurrently in background. The next Send() call waits
// for completion via postTasksWg before proceeding.
// Design: docs/en/agent-data-flow.md Section 4.1
func (a *Agent) postResponseTasks(ctx context.Context) {
	a.postTasksWg.Add(3)
	go func() { defer a.postTasksWg.Done(); a.generateTitleIfNeeded(ctx) }()
	go func() { defer a.postTasksWg.Done(); a.compactMemoryIfNeeded(ctx) }()
	go func() { defer a.postTasksWg.Done(); a.extractPinnedMemories(ctx) }()
}

// compactIfOverBudget runs compaction synchronously before BuildMessages.
// This ensures the context stays within budget for local LLMs.
//
// When Memory.UseV2 is true, this is a no-op: contextbuild handles
// older-tail folding non-destructively. Running both paths would let
// v1's destructive compaction overwrite records that v2 wants to keep.
func (a *Agent) compactIfOverBudget(ctx context.Context) {
	if a.session == nil || a.cfg.Memory.UseV2 {
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
		HotTokenLimit: a.currentHotTokenLimit(),
		Summarizer:    summarizer,
	})
	if err != nil {
		logger.Error("compactIfOverBudget: %v", err)
	}
	if compacted {
		logger.Info("compactIfOverBudget: compacted session %s", a.session.ID)
		_ = a.session.Save()
	}
}

// compactMemoryIfNeeded summarizes old hot messages when token budget exceeded.
// Design: docs/en/agent-data-flow.md Section 4.2 (async safety net).
// Skipped when Memory.UseV2 is true (see compactIfOverBudget).
func (a *Agent) compactMemoryIfNeeded(ctx context.Context) {
	if a.session == nil || a.cfg.Memory.UseV2 {
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
		HotTokenLimit: a.currentHotTokenLimit(),
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

// requestMITL sends a MITL request and returns "" if approved,
// or a rejection message for the LLM if rejected.
func (a *Agent) requestMITL(toolName, arguments, category string) string {
	a.mu.Lock()
	h := a.mitlHandler
	a.mu.Unlock()
	if h == nil {
		return "" // no handler = auto-approve
	}

	resp := h(MITLRequest{
		ToolName:  toolName,
		Arguments: arguments,
		Category:  category,
	})
	if resp.Approved {
		return ""
	}

	if resp.Feedback != "" {
		return fmt.Sprintf("User rejected this operation.\nFeedback: %s\nPlease revise your approach based on the feedback.", resp.Feedback)
	}
	return "Tool execution rejected by user."
}

// normalizeToolArgs runs jsonfix.Extract over the LLM-supplied
// arguments only when a vanilla json.Unmarshal would fail. This
// is the lazy path RFP §3 calls for: well-formed JSON (which
// Vertex always produces and gemma usually does) passes through
// completely untouched, and only malformed wrappers — markdown
// fences, single quotes, trailing commas, surrounding prose —
// invoke the repair pass.
//
// An earlier (eager) version of this helper sent every payload
// through jsonfix; that re-serialised whitespace inside complex
// string values, which read as a content change to anyone
// staring at the log. Lazy is safer and easier to audit.
func normalizeToolArgs(raw string) string {
	if raw == "" {
		return raw
	}
	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err == nil {
		return raw
	}
	fixed, err := jsonfix.Extract(raw)
	if err != nil {
		return raw
	}
	return fixed
}

// executeTool runs a tool call and returns (resultText, status)
// where status is the ActivityEvent status to attach to the
// tool_end event. Phase B classification:
//   - sandbox-run-shell / sandbox-run-python: status follows the
//     container's exit code / timeout (handled inside
//     executeSandboxTool which returns the typed status).
//   - All other branches: explicit Go-side errors map to
//     ActivityStatusError; everything else is ActivityStatusSuccess.
func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, ActivityEventStatus) {
	tc.Arguments = normalizeToolArgs(tc.Arguments)
	switch tc.Name {
	case "resolve-date":
		result, err := chat.ResolveDate(tc.Arguments)
		if err != nil {
			return fmt.Sprintf("Error: %v", err), ActivityStatusError
		}
		return result, ActivityStatusSuccess
	case "list-objects":
		return a.toolListObjects(tc.Arguments), ActivityStatusSuccess
	case "get-object":
		return a.toolGetObject(tc.Arguments), ActivityStatusSuccess
	case "load-data", "describe-data", "query-sql", "query-preview", "suggest-analysis", "quick-summary", "list-tables", "reset-analysis", "create-report", "promote-finding", "analyze-data":
		if a.analysis == nil {
			return "Error: no analysis engine available", ActivityStatusError
		}
		result, err := a.executeAnalysisTool(ctx, tc.Name, tc.Arguments)
		if errors.Is(err, ErrMITLRejected) {
			// MITL rejection: keep the dispatcher's
			// user-facing rejection text as the result so the
			// LLM sees why nothing ran, but mark the activity
			// as error so the bubble doesn't show a green
			// check next to "Tool execution rejected by user."
			return result, ActivityStatusError
		}
		if err != nil {
			return fmt.Sprintf("Error: %v", err), ActivityStatusError
		}
		return result, ActivityStatusSuccess
	default:
		// Sandbox tools (prefixed with "sandbox-")
		if strings.HasPrefix(tc.Name, "sandbox-") {
			if a.IsToolMITLRequired(tc.Name) {
				if rejection := a.requestMITL(tc.Name, tc.Arguments, "execute"); rejection != "" {
					return rejection, ActivityStatusError
				}
			}
			return a.executeSandboxTool(ctx, tc.Name, tc.Arguments)
		}
		// Check MCP guardian tools (prefixed with "mcp__")
		if strings.HasPrefix(tc.Name, "mcp__") {
			// MITL for MCP: default on, can be overridden per tool
			if a.IsToolMITLRequired(tc.Name) {
				if rejection := a.requestMITL(tc.Name, tc.Arguments, "execute"); rejection != "" {
					return rejection, ActivityStatusError
				}
			}
			parts := strings.SplitN(strings.TrimPrefix(tc.Name, "mcp__"), "__", 2)
			if len(parts) != 2 {
				return "Error: invalid MCP tool name format", ActivityStatusError
			}
			a.guardiansMu.RLock()
			g, ok := a.guardians[parts[0]]
			a.guardiansMu.RUnlock()
			if !ok {
				return fmt.Sprintf("Error: MCP guardian %q not found", parts[0]), ActivityStatusError
			}
			result, err := g.CallTool(parts[1], json.RawMessage(tc.Arguments))
			if err != nil {
				return fmt.Sprintf("Error: MCP %s: %v", parts[1], err), ActivityStatusError
			}
			return string(result), ActivityStatusSuccess
		}

		// Check shell script tool registry
		if tool, ok := a.toolRegistry.Get(tc.Name); ok {
			// MITL check for write/execute tools
			needsMITL := tool.NeedsMITL() // category-based default
			if override, ok := a.cfg.Tools.MITLOverrides[tc.Name]; ok {
				needsMITL = override
			}
			if needsMITL {
				result := a.requestMITL(tc.Name, tc.Arguments, string(tool.Category))
				if result != "" {
					return result, ActivityStatusError
				}
			}
			result, err := toolcall.Execute(ctx, tool, tc.Arguments)
			if err != nil {
				return fmt.Sprintf("Error: %v", err), ActivityStatusError
			}
			return result, ActivityStatusSuccess
		}
		return fmt.Sprintf("Error: unknown tool %q", tc.Name), ActivityStatusError
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
	logger.Debug("buildToolDefs: registry has %d shell tools", len(a.toolRegistry.All()))
	for _, t := range a.toolRegistry.All() {
		tools = append(tools, llm.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.ToolDefParams(),
		})
	}

	// Add sandbox tools when the engine is running
	if a.sandbox != nil {
		tools = append(tools, sandboxToolDefs()...)
	}

	// Add MCP guardian tools
	a.guardiansMu.RLock()
	for name, g := range a.guardians {
		for _, t := range g.Tools() {
			var params any
			if len(t.InputSchema) > 0 {
				json.Unmarshal(t.InputSchema, &params)
			}
			tools = append(tools, llm.ToolDef{
				Name:        "mcp__" + name + "__" + t.Name,
				Description: "[" + name + "] " + t.Description,
				Parameters:  params,
			})
		}
	}
	a.guardiansMu.RUnlock()

	// Filter out disabled tools
	disabled := make(map[string]bool)
	for _, name := range a.cfg.Tools.DisabledTools {
		disabled[name] = true
	}
	if len(disabled) > 0 {
		var filtered []llm.ToolDef
		for _, t := range tools {
			if !disabled[t.Name] {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}

	return tools
}

// IsToolMITLRequired checks if a tool requires MITL approval,
// considering per-tool overrides in config.
// Default: MCP tools and write/execute shell tools require MITL.
func (a *Agent) IsToolMITLRequired(toolName string) bool {
	if override, ok := a.cfg.Tools.MITLOverrides[toolName]; ok {
		return override
	}
	// Default: MCP and sandbox tools require MITL.
	if strings.HasPrefix(toolName, "mcp__") {
		return true
	}
	if strings.HasPrefix(toolName, "sandbox-") {
		return true
	}
	return false
}

func (a *Agent) handleCommand(message string) (string, error) {
	parts := strings.Fields(message)
	cmd := parts[0]

	switch cmd {
	case "/help":
		return a.handleHelpCommand()
	case "/model":
		return a.handleModelCommand(parts[1:])
	case "/finding":
		return a.handleFindingCommand(parts[1:])
	case "/findings":
		return a.handleFindingsCommand()
	default:
		return fmt.Sprintf("Unknown command: %s\nType /help for available commands.", cmd), nil
	}
}

func (a *Agent) handleHelpCommand() (string, error) {
	return `**Available Commands**

| Command | Description |
|---------|-------------|
| /help | Show this help |
| /model | Show current backend |
| /model local | Switch to local LLM |
| /model vertex | Switch to Vertex AI |
| /finding <text> | Manually add a finding |
| /findings | List all findings |`, nil
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
	sb.WriteString(fmt.Sprintf("**Findings** (%d)\n\n", len(all)))
	for _, f := range all {
		tags := ""
		if len(f.Tags) > 0 {
			tags = " `" + strings.Join(f.Tags, "` `") + "`"
		}
		sb.WriteString(fmt.Sprintf("- %s%s\n", f.Content, tags))
		if f.OriginSessionTitle != "" {
			sb.WriteString(fmt.Sprintf("  *%s — %s*\n", f.OriginSessionTitle, f.CreatedLabel))
		}
	}
	return sb.String(), nil
}

// RestartLLMBackend rebuilds a.backend from the current cfg so
// changes to LLM.{Local,VertexAI}.RequestTimeoutSeconds (or other
// per-backend settings the wrapper consults) take effect without
// an app restart. Keeps the currently-selected backend.
func (a *Agent) RestartLLMBackend() {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := config.BackendLocal
	if a.backend != nil && a.backend.Name() == "vertex_ai" {
		current = config.BackendVertexAI
	}
	a.setBackend(current)
}

func (a *Agent) setBackend(backend config.LLMBackend) {
	var inner llm.Backend
	var timeoutSec int
	switch backend {
	case config.BackendVertexAI:
		inner = llm.NewVertex(a.cfg.LLM.VertexAI)
		timeoutSec = a.cfg.LLM.VertexAI.VertexRequestTimeout()
	default:
		inner = llm.NewLocal(a.cfg.LLM.Local)
		timeoutSec = a.cfg.LLM.Local.LocalRequestTimeout()
	}
	policy := llm.DefaultRetryPolicy(time.Duration(timeoutSec) * time.Second)
	a.backend = llm.WithRetry(inner, policy)
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
Always respond in the same language the user is using. If the user writes in Japanese, respond in Japanese. If in English, respond in English.

When you call a tool, include a brief explanation of what you are doing and why in the same response. For example, when calling query-sql, mention the SQL and its intent in the same message.

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
