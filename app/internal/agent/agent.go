// Package agent implements the core agent state machine and execution loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// validGuardianName restricts MCP guardian profile names so the
// `mcp__<guardian>__<tool>` flat namespace stays unambiguous. The
// double-underscore separator means we cannot allow `_` runs in
// guardian names without inviting parser confusion
// (security-hardening-2.md H3).
var validGuardianName = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// State represents the agent's execution state.
type State string

const (
	StateIdle State = "idle"
	StateBusy State = "busy"
)

// maxToolRounds is now read from cfg.Agent.MaxToolRoundsResolved()
// at agent-loop entry. The constant remains as a reference for any
// out-of-config call site (currently none).
const maxToolRounds = config.DefaultMaxToolRounds

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

// MITLResponse is the user's decision on a MITL request.
type MITLResponse struct {
	Approved bool   `json:"approved"`
	Feedback string `json:"feedback"` // non-empty when rejected with reason
}

// MITLHandler is called when a tool requires Man-In-The-Loop approval.
type MITLHandler func(req MITLRequest) MITLResponse

// BgTaskEvent is emitted at the start and end of each post-response
// background task (title generation, memory compaction, pinned-fact
// extraction). Phase is "start" or "end"; Error is populated only on
// "end" and only for non-cancel failures.
type BgTaskEvent struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	Error string `json:"error"`
}

// BgTaskHandler receives BgTaskEvent notifications. Bindings register
// one to bridge into the Wails event bus for the footer indicator.
type BgTaskHandler func(BgTaskEvent)

// Agent orchestrates chat, analysis, tool execution, and memory.
type Agent struct {
	cfg     *config.Config
	state   State
	mu      sync.Mutex
	cancel  context.CancelFunc

	// postCancel cancels the in-flight post-response task group
	// (generateTitleIfNeeded / compactMemoryIfNeeded /
	// extractPinnedMemories). Held separately from cancel so that
	// after SendWithImages returns and the user clicks Abort, we
	// can still terminate background goroutines that haven't
	// finished. CancelFunc is safe to call multiple times and on
	// already-finished contexts, so we don't bother clearing it.
	postCancel context.CancelFunc

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
	bgTaskHandler   BgTaskHandler
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
		MaxOutputBytes: rs.MaxOutputBytes,
		SessionsDir:    filepath.Join(config.DataDir(), "sessions"),
	})
	if err != nil {
		logger.Info("sandbox: %v — sandbox tools will be unavailable", err)
		return
	}
	bin, _ := eng.Detect()

	// Image-readiness gate: sandbox-* tools only register when
	// the user has selected an Active image AND that image is
	// present on the local engine. Without this check,
	// "Enabled=true" + an empty or missing image means tools
	// would register but every Exec would fail.
	if rs.Image == "" {
		logger.Info("sandbox: no Active image selected — pick one or click Build in the Sandbox tab. Sandbox tools will stay hidden.")
		return
	}
	ready, err := eng.ImageReady(context.Background(), rs.Image)
	if err != nil {
		logger.Info("sandbox: image readiness probe for %q failed: %v — sandbox tools will stay hidden", rs.Image, err)
		return
	}
	if !ready {
		logger.Info("sandbox: image %q is not present on %s — pick another from the Sandbox tab or rebuild. Sandbox tools will stay hidden.", rs.Image, bin)
		return
	}

	a.sandbox = eng
	logger.Info("sandbox: enabled (engine=%s, image=%s)", bin, rs.Image)

	// Sweep any containers left behind by a previous launch that
	// crashed or was SIGKILL'd. The label filter inside
	// engine.StopAll keeps it scoped to our own containers.
	if err := eng.StopAll(context.Background()); err != nil {
		logger.Info("sandbox: startup sweep failed (non-fatal): %v", err)
	}
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

// SetBgTaskHandler registers a handler for post-response background
// task lifecycle events. The bindings layer uses this to forward
// start/end events into the Wails event bus.
func (a *Agent) SetBgTaskHandler(h BgTaskHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bgTaskHandler = h
}

// notifyBg invokes the registered BgTaskHandler if any. The handler
// is read under the lock, then invoked outside it so a slow handler
// can't block other agent operations.
func (a *Agent) notifyBg(e BgTaskEvent) {
	a.mu.Lock()
	h := a.bgTaskHandler
	a.mu.Unlock()
	if h != nil {
		h(e)
	}
}

// trackBg wraps a post-response task body with start/end logging
// and notification. A context.Canceled return (the auto-cancel case
// when the next Send arrives) is logged as INFO and reported with an
// empty Error so the UI does not flash red. Any other error is logged
// at ERROR and bubbled up via the end event for the footer.
func (a *Agent) trackBg(ctx context.Context, name string, fn func() error) {
	logger.Info("bg-task %s: start", name)
	a.notifyBg(BgTaskEvent{Name: name, Phase: "start"})
	err := fn()
	msg := ""
	switch {
	case err == nil:
		logger.Info("bg-task %s: done", name)
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		logger.Info("bg-task %s: canceled (next Send)", name)
	default:
		logger.Error("bg-task %s: %v", name, err)
		msg = err.Error()
	}
	a.notifyBg(BgTaskEvent{Name: name, Phase: "end", Error: msg})
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
//
// Returns an error if the guard wrap fails — see security-hardening-2.md
// L1. Caller surfaces the error to the user instead of feeding
// unwrapped untrusted content into the LLM.
func (a *Agent) buildMessagesV2(ctx context.Context, budget config.ContextBudgetConfig) ([]llm.Message, error) {
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

	res, err := contextbuild.Build(ctx, a.session, cache, contextbuild.BuildOptions{
		SystemPrompt:        systemPrompt,
		MaxContextTokens:    budget.MaxContextTokens,
		MaxToolResultTokens: budget.MaxToolResultTokens,
		OutputReserve:       budget.OutputReserveResolved(),
		SummarizerID:        a.backend.Name() + "/" + a.currentModelName(),
		Summarize:           summarize,
		WrapUserToolContent: a.chat.WrapUserToolContent,
	})
	if err != nil {
		return nil, err
	}

	if !res.UsedCache && res.SummarizedSpan > 0 {
		if cerr := cache.Save(a.session.ID); cerr != nil {
			logger.Error("buildMessagesV2: save cache: %v", cerr)
		}
	}
	logger.Info("buildMessagesV2: raw=%d summarized=%d cache_hit=%v total_tokens=%d budget=%d",
		res.IncludedRaw, res.SummarizedSpan, res.UsedCache, res.TotalTokens, budget.MaxContextTokens)
	return res.Messages, nil
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
//
// Concurrency model: the agent's state stays Busy from the moment
// this method takes the lock until the post-response background
// tasks complete (handled by postResponseTasks). The input field on
// the frontend keys off Busy, so the user physically cannot send a
// new message — including a slash command — while title generation,
// memory compaction, or pinned-fact extraction is still running.
// This prevents the rapid-conversation race that would otherwise
// drop pinned facts and leave sessions stuck on "New Session".
//
// To bail out of a stuck post-task (e.g. 429 retry), use Abort —
// it fires both cancel funcs and lets the trailing goroutine in
// postResponseTasks return state to Idle.
func (a *Agent) SendWithImages(ctx context.Context, message string, objectIDs, dataURLs []string) (string, error) {
	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return "", ErrBusy
	}
	a.state = StateBusy
	ctx, a.cancel = context.WithCancel(ctx)
	a.mu.Unlock()

	// Slash commands run inside the Busy window so the user can't
	// race two of them, but they don't go through agentLoop and
	// don't trigger post-response tasks — release state directly
	// before returning. Unrecognised slash inputs fall through to
	// agentLoop and are treated as ordinary messages (matches the
	// pre-existing behaviour).
	if strings.HasPrefix(message, "/") {
		parts := strings.Fields(message)
		switch parts[0] {
		case "/model", "/finding", "/findings", "/help":
			result, err := a.handleCommand(message)
			a.mu.Lock()
			a.state = StateIdle
			a.cancel = nil
			a.mu.Unlock()
			if err != nil {
				return "", err
			}
			return "[CMD]" + result, nil
		}
	}

	// agentLoop fires postResponseTasks via defer on every return
	// path; that goroutine is responsible for dropping state back
	// to Idle once all three background tasks complete.
	return a.agentLoop(ctx, message, objectIDs, dataURLs)
}

// Abort cancels the current task and any in-flight post-response
// goroutines. Cancel funcs are safe to call repeatedly and on
// already-finished contexts, so we don't bother clearing them.
func (a *Agent) Abort() {
	a.mu.Lock()
	cancel := a.cancel
	postCancel := a.postCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if postCancel != nil {
		postCancel()
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
	// Take guardiansMu for the whole start sequence so concurrent
	// readers (ListTools, buildToolDefs, executeTool) see a
	// consistent map. Process spawning happens inside the lock —
	// startup is a one-shot blocking phase, not a hot path.
	a.guardiansMu.Lock()
	defer a.guardiansMu.Unlock()
	a.mcpStatuses = nil
	for _, p := range a.cfg.Tools.MCPProfiles {
		if !p.Enabled || p.Name == "" || p.Binary == "" {
			a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "disabled"})
			continue
		}
		if !validGuardianName.MatchString(p.Name) {
			// Reject profiles whose name doesn't match the
			// allowed character set so the dispatcher's
			// `mcp__<name>__<tool>` parsing stays unambiguous
			// (security-hardening-2.md H3). Underscores and
			// double-underscores in particular collide with the
			// separator.
			err := fmt.Errorf("invalid guardian name %q: must match %s", p.Name, validGuardianName)
			logger.Error("MCP guardian %q rejected: %v", p.Name, err)
			a.mcpStatuses = append(a.mcpStatuses, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()})
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
		// Tag the guardian so its drained stderr lines (and any
		// future log lines) carry the profile name.
		g.SetName(p.Name)
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
	// Mirror SendWithImages: drain any in-flight post-response
	// goroutines (compactMemoryIfNeeded / generateTitleIfNeeded
	// / extractPinnedMemories) before reassigning a.session, so
	// no background reader observes a torn swap.
	a.postTasksWg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != StateIdle {
		return ErrBusy
	}
	a.session = session
	a.promptTokens = 0
	a.outputTokens = 0

	// Ensure the per-session work directory exists regardless of
	// whether the sandbox is enabled. Shell tools learn its host
	// path via SHELL_AGENT_WORK_DIR and may write artefacts there
	// for the LLM to surface via the register-object tool.
	// Design: docs/en/work-dir-shell-bridge.md.
	if workDir := a.sessionWorkDir(); workDir != "" {
		if err := os.MkdirAll(workDir, 0700); err != nil {
			logger.Error("agent: workdir create %q: %v", workDir, err)
		}
	}
	return nil
}

// sessionWorkDir returns the absolute host path of the current
// session's work directory, or "" when no session is loaded.
// Shared by LoadSession (creation) and the shell-tool dispatcher
// branch (env var injection).
func (a *Agent) sessionWorkDir() string {
	if a.session == nil {
		return ""
	}
	return filepath.Join(memory.SessionDir(a.session.ID), "work")
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
//
// MITLDefault carries the gate's default for *this* tool, ignoring
// any current MITLOverrides entry. The Settings UI uses it to
// render the toggle in its "as-shipped" state and decides whether
// to persist an override (toggle differs from default → write
// override; toggle equals default → delete override). Surfacing
// this from the backend keeps the UI in sync after Phase B's
// IsToolMITLRequired routing change (security-hardening-2.md
// follow-up — the UI used to compute the default locally from
// category/source and that calculation was stale).
type ToolInfoItem struct {
	Name        string
	Description string
	Category    string
	Source      string
	MITLDefault bool
}

// ListTools returns all available tools with metadata.
//
// MITLDefault on each item reflects the gate's intrinsic default
// (ignoring any user MITLOverrides). The Settings UI relies on this
// to render the toggle correctly — see ToolInfoItem doc.
func (a *Agent) ListTools() []ToolInfoItem {
	var items []ToolInfoItem

	add := func(item ToolInfoItem) {
		item.MITLDefault = a.toolMITLDefault(item.Name, item.Category, item.Source)
		items = append(items, item)
	}

	// Builtin tools
	add(ToolInfoItem{Name: "resolve-date", Description: "Resolve relative date expressions", Category: "read", Source: "builtin"})

	// Analysis tools. Mirrors the dispatcher's analysisTools() —
	// default exposes all 11 every round so the LLM can plan multi-
	// step workflows; legacy filter behaviour is preserved behind
	// cfg.Tools.HideAnalysisToolsUntilDataLoaded for users on
	// weaker local backends. See docs/en/agent-tool-visibility.md.
	hasData := a.analysis != nil && a.analysis.HasData()
	hideUntilDataLoaded := a.cfg.Tools.HideAnalysisToolsUntilDataLoaded
	add(ToolInfoItem{Name: "load-data", Description: "Load a CSV/JSON/JSONL file from a host path into the analysis database as a table", Category: "read", Source: "analysis"})
	add(ToolInfoItem{Name: "reset-analysis", Description: "Drop every table in the current session's analysis database (destructive)", Category: "write", Source: "analysis"})
	add(ToolInfoItem{Name: "create-report", Description: "Render a markdown report and save it to the session's object store", Category: "read", Source: "analysis"})
	add(ToolInfoItem{Name: "list-objects", Description: "List every object (image / blob / report) stored in the current session, with type, MIME, name, and creation time", Category: "read", Source: "analysis"})
	add(ToolInfoItem{Name: "get-object", Description: "Retrieve an object's content by ID (32-hex; legacy 12-hex IDs still work). Images come back as a marker the chat resolves; text/data returns inline", Category: "read", Source: "analysis"})
	add(ToolInfoItem{Name: "register-object", Description: "Move a file from the session work directory ($SHELL_AGENT_WORK_DIR / sandbox /work) into the central object store and return an object:<ID> the chat can render. No-sandbox equivalent of sandbox-register-object.", Category: "write", Source: "analysis"})
	if hasData || !hideUntilDataLoaded {
		add(ToolInfoItem{Name: "describe-data", Description: "Show columns, row count, and saved description for a table; optionally set a description", Category: "read", Source: "analysis"})
		add(ToolInfoItem{Name: "query-sql", Description: "Run a SELECT you write yourself and return raw rows — fastest, no LLM round-trip", Category: "read", Source: "analysis"})
		add(ToolInfoItem{Name: "query-preview", Description: "Natural-language question → LLM-generated SQL → executed → returns SQL + rows", Category: "read", Source: "analysis"})
		add(ToolInfoItem{Name: "suggest-analysis", Description: "Brainstorm 3-5 analysis angles with sample SQL (does NOT execute)", Category: "read", Source: "analysis"})
		add(ToolInfoItem{Name: "quick-summary", Description: "SQL → execute → LLM-generated narrative summary of patterns/outliers", Category: "read", Source: "analysis"})
		add(ToolInfoItem{Name: "list-tables", Description: "List every loaded table with its row count and column list", Category: "read", Source: "analysis"})
		add(ToolInfoItem{Name: "promote-finding", Description: "Save an insight to the cross-session global Findings store", Category: "write", Source: "analysis"})
		add(ToolInfoItem{Name: "analyze-data", Description: "Sliding-window deep analysis: chunks the table, asks the LLM per chunk, accumulates findings, returns a markdown report. Heaviest analysis tool (multiple LLM calls).", Category: "read", Source: "analysis"})
	}

	// Shell script tools
	for _, t := range a.toolRegistry.All() {
		add(ToolInfoItem{Name: t.Name, Description: t.Description, Category: string(t.Category), Source: "shell"})
	}

	// Sandbox tools (all treated as "execute")
	if a.sandbox != nil {
		for _, td := range sandboxToolDefs() {
			add(ToolInfoItem{Name: td.Name, Description: td.Description, Category: "execute", Source: "sandbox"})
		}
	}

	// MCP guardian tools (all treated as "execute" — external service operations)
	a.guardiansMu.RLock()
	for name, g := range a.guardians {
		for _, t := range g.Tools() {
			add(ToolInfoItem{
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

// toolMITLDefault is the per-tool MITL default ignoring any
// MITLOverrides entry. Mirrors the resolution rules in
// IsToolMITLRequired so the Settings UI can render the toggle in
// its as-shipped state. Centralising the resolution here would be
// cleaner — leave that for a follow-up; for now, keep the rules in
// sync between this and IsToolMITLRequired.
func (a *Agent) toolMITLDefault(name, category, source string) bool {
	if strings.HasPrefix(name, "mcp__") || source == "mcp" {
		return true
	}
	if strings.HasPrefix(name, "sandbox-") || source == "sandbox" {
		return true
	}
	if def, ok := analysisToolMITLDefault[name]; ok {
		return def
	}
	// Shell tools: write/execute categories require MITL by default.
	switch toolcall.Category(category) {
	case toolcall.CategoryWrite, toolcall.CategoryExecute:
		return true
	}
	return false
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

	// Background post-response tasks fire on every return path —
	// success, max-rounds, or any error. The helpers each guard
	// their own preconditions (Title=="New Session", token-budget
	// thresholds, recent-records gating) so spurious invocations
	// don't trigger spurious LLM calls. The goroutine spawned here
	// is also responsible for returning state to Idle once all
	// post-tasks finish (see postResponseTasks).
	defer a.postResponseTasks(ctx)

	// Avoid logging full user message at Info level (may contain sensitive data).
	logger.Info("agentLoop: session=%s message_len=%d objects=%d", a.session.ID, len(userMessage), len(objectIDs))
	logger.Debug("agentLoop: message=%s", logger.Truncate(userMessage, 100))
	if len(objectIDs) > 0 {
		logger.Debug("agentLoop: attached objectIDs (in order)=%v dataURL_count=%d", objectIDs, len(dataURLs))
	}

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

	// Loop-detection ring buffer (Feature 1 of agent-loop-resilience).
	// Local to one agent turn — a fresh user message starts clean.
	var recentToolCalls []toolCallTrace

	// Empty-response retry: when the LLM returns content="" with no
	// tool calls right after a successful tool execution, give it one
	// chance to wrap up before exiting silently. Both flags are
	// per-turn; emptyRetryDone gates the one-shot retry,
	// injectEmptyNudge is consumed in the next round.
	var emptyRetryDone, injectEmptyNudge bool

	// Step 2: Agent loop (max rounds — configurable via Settings)
	maxRounds := a.cfg.Agent.MaxToolRoundsResolved()
	for round := range maxRounds {
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
			built, err := a.buildMessagesV2(ctx, budget)
			if err != nil {
				// Fail-closed: BuildMessages returns an error only when
				// guard.Wrap fails (essentially crypto/rand catastrophe).
				// Better to surface the failure than feed unwrapped
				// untrusted content to the LLM (security-hardening-2.md L1).
				logger.Error("agentLoop: buildMessagesV2: %v", err)
				return "", fmt.Errorf("build messages: %w", err)
			}
			messages = built
		} else {
			buildResult, err := a.chat.BuildMessagesWithBudget(
				a.session,
				a.pinned.FormatForPrompt(),
				a.findings.FormatForPrompt(),
				chat.BuildOptions{
					MaxConversationTokens: budget.MaxContextTokens,
					MaxWarmTokens:         budget.MaxWarmTokens,
					MaxToolResultTokens:   budget.MaxToolResultTokens,
				},
			)
			if err != nil {
				logger.Error("agentLoop: BuildMessagesWithBudget: %v", err)
				return "", fmt.Errorf("build messages: %w", err)
			}
			messages = buildResult.Messages
			if buildResult.DroppedCount > 0 {
				logger.Debug("agentLoop: budget control dropped %d old messages (total ~%d tokens)", buildResult.DroppedCount, buildResult.TotalTokens)
			}
		}

		// Loop-detection: if the previous rounds show the same tool
		// failing 3× in a row, prepend a one-shot corrective hint
		// as a system message. The hint is NOT added to
		// session.Records — it's transient and lives only for this
		// LLM call. After firing we reset the buffer so we don't
		// re-fire on every subsequent round.
		if name, stuck := detectStuckLoop(recentToolCalls); stuck {
			messages = append([]llm.Message{{Role: "system", Content: loopHintFor(name)}}, messages...)
			logger.Info("agentLoop: loop-detection: %s hit error 3× in a row, injected corrective hint", name)
			recentToolCalls = nil
		}

		// Empty-response wrap-up nudge — set in the previous round
		// when Vertex returned content="" with no tool calls. The
		// flag is consumed here so we only inject it once.
		if injectEmptyNudge {
			messages = append([]llm.Message{{Role: "system", Content: emptyResponseNudge}}, messages...)
			injectEmptyNudge = false
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
			} else if !emptyRetryDone {
				// Vertex sometimes returns 0 output tokens after a
				// tool result. Give it one chance to wrap up before
				// the user is left staring at tool activity with no
				// final reply.
				emptyRetryDone = true
				injectEmptyNudge = true
				logger.Info("agentLoop: empty response after tool calls, retrying once with wrap-up nudge")
				continue
			} else {
				logger.Info("agentLoop: empty response after wrap-up retry, ending loop")
			}
			return resp.Content, nil
		}

		// --- Tool calls present ---
		// Record the assistant's tool calls structurally so the
		// next LLM turn replays them in the protocol-correct shape
		// (Vertex FunctionCall part / OpenAI tool_calls). The
		// chat UI substitutes a "Calling: foo" placeholder at
		// render time when Content is empty.
		callRecords := make([]memory.ToolCallRecord, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			callRecords[i] = memory.ToolCallRecord{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			}
		}
		a.session.AddAssistantMessageWithToolCalls(resp.Content, callRecords)

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
			a.session.AddToolResult(tc.ID, tc.Name, result, string(status))
			a.emitActivity(ActivityEvent{Type: "tool_end", Detail: tc.Name, Status: status})
			recentToolCalls = pushToolCallTrace(recentToolCalls, toolCallTrace{Name: tc.Name, Status: status})
		}
		_ = a.session.Save() // auto-save after tool execution
	}

	logger.Debug("agentLoop: max rounds (%d) reached", maxRounds)
	return "(Max tool rounds reached)", nil
}

// postResponseTasks launches background tasks after the agent loop
// returns: title generation, memory compaction, pinned-fact
// extraction. Each is wrapped in trackBg so the footer indicator
// (start/end events) and log lines stay symmetric.
//
// State machine: agent state stays Busy until all three goroutines
// finish; the trailing goroutine drops state back to Idle. This
// means the input field stays disabled while these tasks run, so
// the user cannot type a new message that would race with title
// generation or pinned-fact extraction (the previous fire-and-
// forget design caused tasks to be auto-cancelled in rapid
// conversations and pinned facts to be silently dropped).
//
// The tasks run under a context derived from parentCtx; the cancel
// is stashed on a.postCancel so Abort can interrupt them when the
// user explicitly wants to bail (e.g. a 429 retry that's taking
// too long). Without an Abort, the goroutine waits to completion.
//
// Design: docs/en/agent-data-flow.md §4.1,
// docs/en/background-task-indicator.md.
func (a *Agent) postResponseTasks(parentCtx context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	a.mu.Lock()
	a.postCancel = cancel
	a.mu.Unlock()

	a.postTasksWg.Add(3)
	go func() {
		defer a.postTasksWg.Done()
		a.trackBg(ctx, "title", func() error { return a.generateTitleIfNeeded(ctx) })
	}()
	go func() {
		defer a.postTasksWg.Done()
		a.trackBg(ctx, "memory-compaction", func() error { return a.compactMemoryIfNeeded(ctx) })
	}()
	go func() {
		defer a.postTasksWg.Done()
		a.trackBg(ctx, "pinned-extraction", func() error { return a.extractPinnedMemories(ctx) })
	}()

	// Trailing goroutine: wait for all three tasks to finish, then
	// release the agent back to Idle. Done as a separate goroutine
	// so postResponseTasks itself returns immediately and the
	// caller (agentLoop's defer) doesn't block.
	go func() {
		a.postTasksWg.Wait()
		a.mu.Lock()
		a.state = StateIdle
		a.cancel = nil
		a.postCancel = nil
		a.mu.Unlock()
	}()
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
func (a *Agent) compactMemoryIfNeeded(ctx context.Context) error {
	if a.session == nil || a.cfg.Memory.UseV2 {
		return nil
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
		return err
	}
	if compacted {
		logger.Info("compactMemory: compacted session %s", a.session.ID)
		_ = a.session.Save()
	}
	return nil
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
	case "register-object":
		// Bridge a host-side artefact (typically produced by a
		// shell tool that wrote to $SHELL_AGENT_WORK_DIR) into
		// objstore so the chat can render it as object:<ID>.
		// Mirrors sandbox-register-object for the no-sandbox
		// flow. Design: docs/en/work-dir-shell-bridge.md.
		if a.IsToolMITLRequired(tc.Name) {
			if rejection := a.requestMITL(tc.Name, tc.Arguments, "write"); rejection != "" {
				return rejection, ActivityStatusError
			}
		}
		return a.toolRegisterObject(tc.Arguments)
	case "load-data", "describe-data", "query-sql", "query-preview", "suggest-analysis", "quick-summary", "list-tables", "reset-analysis", "create-report", "promote-finding", "analyze-data":
		if a.analysis == nil {
			return "Error: no analysis engine available", ActivityStatusError
		}
		// MITL is gated centrally here so the Settings → Tools toggle
		// (cfg.Tools.MITLOverrides via IsToolMITLRequired) takes
		// effect for analysis tools. Before security-hardening-2.md
		// H1+H2 the toggle was a no-op for this branch — load-data,
		// reset-analysis and promote-finding ran without confirmation
		// regardless, while query-sql / analyze-data prompted
		// regardless. The hard-coded MITL calls inside
		// executeAnalysisTool have been removed.
		if a.IsToolMITLRequired(tc.Name) {
			category := analysisToolMITLCategory(tc.Name)
			if rejection := a.requestMITL(tc.Name, tc.Arguments, category); rejection != "" {
				return rejection, ActivityStatusError
			}
		}
		result, err := a.executeAnalysisTool(ctx, tc.Name, tc.Arguments)
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
			a.guardiansMu.RLock()
			guardianName, toolName, ok := splitMCPName(strings.TrimPrefix(tc.Name, "mcp__"), a.guardians)
			var g *mcp.Guardian
			if ok {
				g = a.guardians[guardianName]
			}
			a.guardiansMu.RUnlock()
			if !ok {
				return "Error: invalid MCP tool name format", ActivityStatusError
			}
			if g == nil {
				return fmt.Sprintf("Error: MCP guardian %q not found", guardianName), ActivityStatusError
			}
			result, err := g.CallTool(toolName, json.RawMessage(tc.Arguments))
			if errors.Is(err, mcp.ErrToolFailed) {
				// Upstream MCP server signalled a tool-level
				// failure via result.isError. The body still
				// carries diagnostic content the LLM benefits
				// from seeing, so pass it through; only the
				// chat-bubble status flips to error.
				return string(result), ActivityStatusError
			}
			if err != nil {
				return fmt.Sprintf("Error: MCP %s: %v", toolName, err), ActivityStatusError
			}
			return string(result), ActivityStatusSuccess
		}

		// Check shell script tool registry
		if tool, ok := a.toolRegistry.Get(tc.Name); ok {
			// MITL routing matches every other tool source by going
			// through IsToolMITLRequired (which itself consults
			// MITLOverrides → mcp/sandbox prefix → analysisToolMITLDefault
			// → tool.NeedsMITL). Single source of truth keeps the
			// Settings UI's per-tool toggle accurate.
			if a.IsToolMITLRequired(tc.Name) {
				result := a.requestMITL(tc.Name, tc.Arguments, string(tool.Category))
				if result != "" {
					return result, ActivityStatusError
				}
			}
			// SHELL_AGENT_WORK_DIR injection — see
			// docs/en/work-dir-shell-bridge.md.
			result, err := toolcall.Execute(ctx, tool, tc.Arguments,
				toolcall.WithWorkDir(a.sessionWorkDir()))
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

	// Add analysis tools. Default exposes all 11 every round so the
	// LLM can plan multi-step "load → query → analyse → report"
	// workflows up front. Legacy filter behaviour is preserved
	// behind cfg.Tools.HideAnalysisToolsUntilDataLoaded for users
	// on weaker local backends. See docs/en/agent-tool-visibility.md.
	if a.analysis != nil {
		tools = append(tools, analysisTools(
			a.analysis.HasData(),
			a.cfg.Tools.HideAnalysisToolsUntilDataLoaded,
		)...)
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

// splitMCPName parses the part after `mcp__` of an MCP tool call name
// into (guardianName, toolName). It first tries the naive split on
// the first `__`; if the resulting guardianName isn't registered, it
// walks the registered guardians and picks the longest one whose name
// is a prefix of `rest` followed by `__`. This makes the parser
// tolerant of guardian or upstream-tool names containing `__`
// (security-hardening-2.md H3). Caller must hold guardiansMu.
func splitMCPName(rest string, guardians map[string]*mcp.Guardian) (string, string, bool) {
	if i := strings.Index(rest, "__"); i > 0 {
		guardian := rest[:i]
		tool := rest[i+2:]
		if tool != "" {
			if _, ok := guardians[guardian]; ok {
				return guardian, tool, true
			}
		}
	}
	// Fall back to longest-prefix match against registered names.
	var bestGuardian string
	for name := range guardians {
		marker := name + "__"
		if !strings.HasPrefix(rest, marker) {
			continue
		}
		if len(name) > len(bestGuardian) {
			bestGuardian = name
		}
	}
	if bestGuardian == "" {
		return "", "", false
	}
	tool := rest[len(bestGuardian)+2:]
	if tool == "" {
		return "", "", false
	}
	return bestGuardian, tool, true
}

// IsToolMITLRequired checks if a tool requires MITL approval,
// considering per-tool overrides in config.
//
// Priority:
//  1. cfg.Tools.MITLOverrides[name] — user override always wins
//  2. mcp__* / sandbox-* prefix → on by default
//  3. analysisToolMITLDefault map → per-tool default for analysis tools
//  4. otherwise (shell tools) → false; the dispatcher consults the
//     tool's own Category via tool.NeedsMITL()
//
// Before security-hardening-2.md H1+H2, the analysis-tool branch of
// the dispatcher bypassed this entirely: the Settings → Tools MITL
// toggle existed in the UI but had no effect for any analysis tool
// (load-data, reset-analysis, promote-finding could never be turned
// ON; query-sql / analyze-data could never be turned OFF, the call
// was hardcoded inside executeAnalysisTool). This routing closes that
// contract gap.
func (a *Agent) IsToolMITLRequired(toolName string) bool {
	if override, ok := a.cfg.Tools.MITLOverrides[toolName]; ok {
		return override
	}
	if strings.HasPrefix(toolName, "mcp__") {
		return true
	}
	if strings.HasPrefix(toolName, "sandbox-") {
		return true
	}
	if def, ok := analysisToolMITLDefault[toolName]; ok {
		return def
	}
	// Shell tools — consult the registry's own category. Without this
	// branch, the dispatcher's shell path used to compute MITL via
	// tool.NeedsMITL() directly; that left IsToolMITLRequired
	// disagreeing with the actual gate for shell tools, breaking the
	// Settings UI's per-tool default and the contract test
	// (TestListTools_MITLDefaultMatchesGate).
	if tool, ok := a.toolRegistry.Get(toolName); ok {
		return tool.NeedsMITL()
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
	var maxAttempts, backoffBaseSec, backoffMaxSec, jitterSec int
	switch backend {
	case config.BackendVertexAI:
		inner = llm.NewVertex(a.cfg.LLM.VertexAI)
		timeoutSec = a.cfg.LLM.VertexAI.VertexRequestTimeout()
		maxAttempts = a.cfg.LLM.VertexAI.RetryMaxAttempts
		backoffBaseSec = a.cfg.LLM.VertexAI.RetryBackoffBaseSeconds
		backoffMaxSec = a.cfg.LLM.VertexAI.RetryBackoffMaxSeconds
		jitterSec = a.cfg.LLM.VertexAI.RetryJitterSeconds
	default:
		inner = llm.NewLocal(a.cfg.LLM.Local)
		timeoutSec = a.cfg.LLM.Local.LocalRequestTimeout()
		maxAttempts = a.cfg.LLM.Local.RetryMaxAttempts
		backoffBaseSec = a.cfg.LLM.Local.RetryBackoffBaseSeconds
		backoffMaxSec = a.cfg.LLM.Local.RetryBackoffMaxSeconds
		jitterSec = a.cfg.LLM.Local.RetryJitterSeconds
	}
	policy := llm.RetryPolicyFrom(
		time.Duration(timeoutSec)*time.Second,
		maxAttempts, backoffBaseSec, backoffMaxSec, jitterSec,
	)
	// Surface retry backoffs to the UI so a slow round looks
	// like "rate-limited, retrying…" instead of a hang.
	policy.OnBackoff = func(attempt int, wait time.Duration, err error) {
		label := llm.ClassifyError(err)
		if label == "" {
			label = "transient error"
		}
		a.emitActivity(ActivityEvent{
			Type:   "retry_backoff",
			Detail: fmt.Sprintf("attempt %d: %s (waiting %s)", attempt, label, wait.Round(100*time.Millisecond)),
		})
	}
	a.backend = llm.WithRetry(inner, policy)
}

// generateTitleIfNeeded generates a session title from the first user message.
func (a *Agent) generateTitleIfNeeded(ctx context.Context) error {
	if a.session == nil || a.session.Title != "New Session" {
		return nil
	}

	var firstUser string
	for _, r := range a.session.Records {
		if r.Role == "user" && r.Tier == memory.TierHot {
			firstUser = r.Content
			break
		}
	}
	if firstUser == "" {
		return nil
	}

	messages := []llm.Message{
		{Role: "system", Content: "Generate a very short title (under 30 chars) for a chat that starts with the following message. Reply with ONLY the title, no quotes, no explanation. Use the same language as the message."},
		{Role: "user", Content: firstUser},
	}

	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		return err
	}

	title := strings.TrimSpace(resp.Content)
	if title == "" || len(title) > 60 {
		return nil
	}

	a.session.Title = title
	_ = a.session.Save()

	a.mu.Lock()
	h := a.titleHandler
	a.mu.Unlock()
	if h != nil {
		h(a.session.ID, title)
	}
	return nil
}

const defaultSystemPrompt = `You are a helpful assistant with data analysis capabilities.
You can use tools to help answer questions.
Always respond in the same language the user is using. If the user writes in Japanese, respond in Japanese. If in English, respond in English.

When you call a tool, include a brief explanation of what you are doing and why in the same response. For example, when calling query-sql, mention the SQL and its intent in the same message.

When asked about dates, use the resolve-date tool if you are unsure about the calculation.

When you discover a significant analysis insight (a pattern, anomaly, or conclusion that would be valuable across sessions), use the promote-finding tool to save it to the global findings store.

When the user asks you to create a report, summary document, or formatted output, you MUST use the create-report tool. Do not write the report as a chat message — always call the create-report tool so the report is properly structured and rendered with full markdown support. Use GitHub-flavored Markdown only; do NOT emit raw HTML tags (e.g. <br>, <table>, <details>, <sub>) — the renderer escapes them and they appear as plain text.

When the user shares images in the conversation, each attached image is preceded by a short text line of the form "Image (object ID: xxxxxxxxxxxx):". The ID immediately before an image is THAT image's persistent object ID — describe each image based ONLY on the content directly following its ID line, and reference images in reports using ![alt](object:ID) with that exact ID. Do NOT call list-objects to identify currently attached images; list-objects returns objects in unspecified order and will mis-correlate IDs with image content.

To reference objects from the session:
1. For images attached in the current message: read the anchor immediately preceding each image
2. For other objects (older images, reports, files): use the list-objects tool to discover available objects, then get-object to retrieve them
3. In reports, reference images with: ![description](object:ID)
Never fabricate image URLs or object IDs.`

// extractPinnedMemories runs after each response to auto-extract important facts.
// This is a system task, not an LLM tool — the backend drives the extraction.
func (a *Agent) extractPinnedMemories(ctx context.Context) error {
	if a.session == nil {
		return nil
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
		return nil // need at least a user + assistant exchange
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
		return err
	}

	text := strings.TrimSpace(resp.Content)
	if text == "" || strings.ToUpper(text) == "NONE" {
		return nil
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
	return nil
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
