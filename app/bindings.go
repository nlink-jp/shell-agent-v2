package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"path/filepath"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/bundled"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox/imagebuild"
	"github.com/nlink-jp/shell-agent-v2/internal/sessionio"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Bindings is the thin Wails binding layer.
// All business logic is delegated to the agent and analysis packages.
type Bindings struct {
	ctx        context.Context
	agent      *agent.Agent
	cfg        *config.Config
	analysis   *analysis.Engine
	objects    *objstore.Store

	// MITL response routing. Each in-flight request owns its own
	// channel held in mitlReq; resolved by Approve/Reject*. A
	// stray click while no request is pending finds mitlReq nil
	// and no-ops, instead of leaving a stale value in a buffered
	// chan that the next request would silently consume.
	mitlMu  sync.Mutex
	mitlReq *mitlSlot

	// Sandbox image build single-flight.
	buildMu       sync.Mutex
	buildInFlight bool
}

// ErrBuildInProgress is returned by BuildSandboxImage when a
// previous build is still running.
var ErrBuildInProgress = errors.New("sandbox: image build already in progress")

// mitlSlot is the per-request state attached to b.mitlReq while
// a tool is awaiting MITL approval.
type mitlSlot struct {
	req agent.MITLRequest
	ch  chan agent.MITLResponse
}

// NewBindings creates a new Bindings instance.
func NewBindings() *Bindings {
	return &Bindings{}
}

func (b *Bindings) startup(ctx context.Context) {
	b.ctx = ctx

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("Warning: using default config:", err.Error())
		cfg = config.Default()
	}
	b.cfg = cfg

	_ = logger.Init(config.DataDir())
	logger.SetLevel(parseLogLevel(cfg.LogLevelString()))
	logger.Info("shell-agent-v2 starting, config=%s log_level=%s", config.ConfigPath(), cfg.LogLevelString())

	if installed, err := bundled.Install(cfg.Tools.ScriptDir); err != nil {
		logger.Error("bundled tool install: %v", err)
	} else if len(installed) > 0 {
		logger.Info("bundled tools: installed %d new (%v)", len(installed), installed)
	}

	b.objects = objstore.NewStore()
	_ = b.objects.Load()

	b.agent = agent.New(cfg)
	b.agent.SetObjects(b.objects)
	b.agent.SetStreamHandler(func(token string, done bool) {
		wailsRuntime.EventsEmit(b.ctx, "agent:stream", map[string]any{
			"token": token,
			"done":  done,
		})
	})
	b.agent.SetTitleHandler(func(sessionID, title string) {
		wailsRuntime.EventsEmit(b.ctx, "session:title", map[string]any{
			"session_id": sessionID,
			"title":      title,
		})
	})
	b.agent.SetGlobalMemoryHandler(func() {
		wailsRuntime.EventsEmit(b.ctx, "global_memory:updated", nil)
	})
	b.agent.SetSessionMemoryHandler(func() {
		wailsRuntime.EventsEmit(b.ctx, "session_memory:updated", nil)
	})
	b.agent.SetFindingsHandler(func() {
		wailsRuntime.EventsEmit(b.ctx, "findings:updated", nil)
	})
	b.agent.SetReportHandler(func(title, content string) {
		wailsRuntime.EventsEmit(b.ctx, "report:created", map[string]any{
			"title":   title,
			"content": content,
		})
	})
	b.agent.SetActivityHandler(func(ev agent.ActivityEvent) {
		payload := map[string]any{
			"type":   ev.Type,
			"detail": ev.Detail,
		}
		if ev.Status != "" {
			payload["status"] = string(ev.Status)
		}
		if ev.ToolCallID != "" {
			payload["tool_call_id"] = ev.ToolCallID
		}
		wailsRuntime.EventsEmit(b.ctx, "agent:activity", payload)
	})
	b.agent.SetBgTaskHandler(func(e agent.BgTaskEvent) {
		wailsRuntime.EventsEmit(b.ctx, "bg-task:"+e.Phase, map[string]any{
			"name":  e.Name,
			"error": e.Error,
		})
	})
	b.agent.SetMITLHandler(func(req agent.MITLRequest) agent.MITLResponse {
		ch := make(chan agent.MITLResponse, 1)
		b.mitlMu.Lock()
		b.mitlReq = &mitlSlot{req: req, ch: ch}
		b.mitlMu.Unlock()

		wailsRuntime.EventsEmit(b.ctx, "mitl:request", map[string]any{
			"tool_name": req.ToolName,
			"arguments": req.Arguments,
			"category":  req.Category,
		})

		resp := <-ch

		b.mitlMu.Lock()
		b.mitlReq = nil
		b.mitlMu.Unlock()
		return resp
	})

	// Restore window position and size
	w := cfg.UI.Window
	if w.Width > 0 && w.Height > 0 {
		wailsRuntime.WindowSetSize(ctx, w.Width, w.Height)
		wailsRuntime.WindowSetPosition(ctx, w.X, w.Y)
	}

	logger.Info("bindings: startup complete (agent=%v)", b.agent != nil)
}

func (b *Bindings) shutdown(_ context.Context) {
	// Save window position and size
	if b.ctx != nil && b.cfg != nil {
		w, h := wailsRuntime.WindowGetSize(b.ctx)
		x, y := wailsRuntime.WindowGetPosition(b.ctx)
		b.cfg.UI.Window.X = x
		b.cfg.UI.Window.Y = y
		b.cfg.UI.Window.Width = w
		b.cfg.UI.Window.Height = h
		_ = b.cfg.Save()
	}

	if b.analysis != nil {
		b.analysis.Close()
	}
	if b.agent != nil {
		b.agent.Close()
	}
	logger.Info("shell-agent-v2 shutdown complete")
	logger.Close()
}

// parseLogLevel maps the configured string ("debug" / "info" /
// "warn" / "error") to the corresponding logger.Level. Empty or
// unrecognised values fall back to LevelInfo, matching the
// privacy default in cfg.LogLevelString().
func parseLogLevel(s string) logger.Level {
	switch s {
	case "debug":
		return logger.LevelDebug
	case "warn":
		return logger.LevelWarn
	case "error":
		return logger.LevelError
	default:
		return logger.LevelInfo
	}
}

// --- Agent bindings ---

// IsBusy reports whether the agent is currently processing.
func (b *Bindings) IsBusy() bool {
	if b.agent == nil {
		return false
	}
	return b.agent.State() == agent.StateBusy
}

// Send sends a user message to the agent.
func (b *Bindings) Send(message string) (string, error) {
	if b.agent == nil {
		return "", fmt.Errorf("agent not initialised yet")
	}
	return b.agent.Send(b.ctx, message)
}

// SendWithImages sends a user message with attachments to the agent.
//
// As of v0.5 the parameter is named imageDataURLs for backward
// compatibility with the Wails-generated frontend binding, but the
// slice may contain a mix of image and text/markdown data URLs.
// Per-attachment routing happens inside via SaveDataURL → Type
// inspection: images stay on the multimodal path (ImageURLs +
// ObjectIDs), markdown / plain-text attachments flow into the
// new DocumentIDs slice and surface to the LLM via anchor lines
// prepended in the contextbuild render path.
func (b *Bindings) SendWithImages(message string, imageDataURLs []string) (string, error) {
	var imageObjectIDs []string
	var imageOnlyDataURLs []string
	var documentObjectIDs []string
	sessionID := ""
	if s := b.agent.CurrentSession(); s != nil {
		sessionID = s.ID
	}
	for _, du := range imageDataURLs {
		meta, err := b.objects.SaveDataURL(du, sessionID)
		if err != nil {
			logger.Error("SaveDataURL: %v", err)
			continue
		}
		switch meta.Type {
		case objstore.TypeImage:
			imageObjectIDs = append(imageObjectIDs, meta.ID)
			imageOnlyDataURLs = append(imageOnlyDataURLs, du)
		case objstore.TypeMarkdown:
			documentObjectIDs = append(documentObjectIDs, meta.ID)
		default:
			// SaveDataURL only emits TypeImage / TypeMarkdown /
			// TypeBlob today; an unexpected TypeBlob from the
			// chat input is a frontend misroute — log and skip
			// rather than dumping random bytes into the LLM.
			logger.Error("SendWithImages: attachment with unexpected type %q (mime=%s); skipping", meta.Type, meta.MimeType)
		}
	}
	return b.agent.SendWithAttachments(b.ctx, message, imageObjectIDs, imageOnlyDataURLs, documentObjectIDs)
}

// Abort cancels the current agent task.
func (b *Bindings) Abort() {
	logger.Info("Bindings.Abort: invoked from frontend")
	if b.agent != nil {
		b.agent.Abort()
	}
}

// GetState returns the current agent state ("idle" or "busy").
func (b *Bindings) GetState() string {
	if b.agent == nil {
		return "idle"
	}
	return string(b.agent.State())
}

// GetBackend returns the current LLM backend name.
func (b *Bindings) GetBackend() string {
	if b.agent == nil {
		return ""
	}
	return b.agent.CurrentBackend()
}

// --- Session bindings ---

// NewSession creates a new chat session and switches to it.
func (b *Bindings) NewSession() (string, error) {
	return b.newSession(false)
}

// NewPrivateSession creates a private chat session (Global
// Memory promotion suppressed) and switches to it. See
// docs/en/privacy-controls.md §2.
func (b *Bindings) NewPrivateSession() (string, error) {
	return b.newSession(true)
}

func (b *Bindings) newSession(private bool) (string, error) {
	if b.IsBusy() {
		return "", fmt.Errorf("agent is busy")
	}

	session := &memory.Session{
		ID:      fmt.Sprintf("sess-%d", nowUnixMilli()),
		Title:   "New Session",
		Private: private,
		Records: []memory.Record{},
	}
	if err := session.Save(); err != nil {
		return "", err
	}
	logger.Info("session created: id=%s private=%v", session.ID, session.Private)

	b.switchAnalysis(session.ID)
	if err := b.agent.LoadSession(session); err != nil {
		return "", err
	}

	return session.ID, nil
}

// LoadSession switches to an existing session and returns its messages.
func (b *Bindings) LoadSession(sessionID string) ([]MessageData, error) {
	if b.agent == nil {
		return nil, fmt.Errorf("agent not initialised yet")
	}
	if b.IsBusy() {
		return nil, fmt.Errorf("agent is busy")
	}

	session, err := memory.LoadSession(sessionID)
	if err != nil {
		return nil, err
	}

	b.switchAnalysis(sessionID)
	if err := b.agent.LoadSession(session); err != nil {
		return nil, err
	}

	// Filter records for frontend display
	// Design: docs/en/agent-data-flow.md Section 3.2
	var msgs []MessageData
	for i, r := range session.Records {
		switch r.Role {
		case "tool":
			// Reconstruct the live `tool-event` bubble: tool name
			// + status (success / error). Empty Status indicates
			// a session written before the field existed; default
			// to "success" so legacy chats render with a sane
			// styling rather than no bubble at all.
			// Design: docs/en/tool-event-restore.md.
			if r.ToolName == "" {
				continue
			}
			status := r.Status
			if status == "" {
				status = "success"
			}
			// Vertex Gemini sessions written before vertex.go
			// started synthesising FunctionCall IDs have empty
			// ToolCallID. Fall back to the absolute record index
			// as a synthetic id ("idx:N") so the click-to-inspect
			// overlay can still locate the record on
			// GetToolCallDetails. New sessions have real ids and
			// take this path through the non-empty branch.
			toolCallID := r.ToolCallID
			if toolCallID == "" {
				toolCallID = fmt.Sprintf("idx:%d", i)
			}
			msgs = append(msgs, MessageData{
				Role:       "tool-event",
				Content:    r.ToolName,
				Status:     status,
				Timestamp:  r.Timestamp.Format("15:04:05"),
				ToolCallID: toolCallID,
			})
			continue
		case "assistant":
			// Mirror the live chat behaviour: tool-call turns are
			// surfaced through the activity stream (tool_start /
			// tool_end and the transient "thinking" progressTool),
			// not as chat bubbles. Restoring them as bubbles would
			// also drag back any thought-style preamble the model
			// wrote into Content (e.g. Gemini 2.5 Flash sometimes
			// prefixes "シンクタイム: 3秒\n\n…" or "THOUGHT\n…"),
			// which never appeared in the live view.
			//   - Old format (pre-r3): Content="[Calling: foo]"
			//   - New format (post-r3): Content possibly non-empty
			//     plus ToolCalls non-empty (tracked via ToolCalls
			//     on the persisted Record).
			if strings.HasPrefix(r.Content, "[Calling:") {
				continue
			}
			if len(r.ToolCalls) > 0 {
				continue
			}
			if r.Content == "" {
				continue
			}
			msgs = append(msgs, MessageData{
				Role: r.Role, Content: r.Content,
				Timestamp: r.Timestamp.Format("15:04:05"),
			})
		case "summary":
			// Surface legacy summaries as a distinct block so the user
			// sees that older content was compacted, with its time range.
			content := r.Content
			if r.SummaryRange != nil {
				content = fmt.Sprintf("[%s — %s]\n%s",
					r.SummaryRange.From.Format("2006-01-02 15:04"),
					r.SummaryRange.To.Format("2006-01-02 15:04"),
					r.Content)
			}
			msgs = append(msgs, MessageData{
				Role: "summary", Content: content,
				Timestamp: r.Timestamp.Format("15:04:05"),
			})
		case "user":
			// User records keep their attached object IDs in
			// Record.ObjectIDs at send time; surface them so the
			// restored chat shows the same images the user
			// originally attached. Without this the restored
			// view is just text and the user can't follow what
			// was being discussed (the assistant's reply may
			// reference "this image" with no visual referent).
			md := MessageData{
				Role: r.Role, Content: r.Content,
				Timestamp: r.Timestamp.Format("15:04:05"),
			}
			if len(r.ObjectIDs) > 0 {
				md.ObjectIDs = append(md.ObjectIDs, r.ObjectIDs...)
			}
			msgs = append(msgs, md)
		default:
			msgs = append(msgs, MessageData{
				Role: r.Role, Content: r.Content,
				Timestamp: r.Timestamp.Format("15:04:05"),
			})
		}
	}
	// Legacy create-report ordering fix: pre-v0.2.3 sessions
	// wrote the report record into chat.json BEFORE the
	// matching tool record (toolCreateReport called
	// AddReportMessage immediately, while AddToolResult ran a
	// moment later in the agent loop). New sessions write them
	// in the correct "tool → report" order, but older sessions
	// would replay backwards. Swap any adjacent
	// (report, tool-event=create-report) pair so the chat pane
	// always sees "create-report bubble → report bubble".
	for i := 0; i+1 < len(msgs); i++ {
		if msgs[i].Role == "report" && msgs[i+1].Role == "tool-event" && msgs[i+1].Content == "create-report" {
			msgs[i], msgs[i+1] = msgs[i+1], msgs[i]
		}
	}
	return msgs, nil
}

// ListSessions returns all sessions.
func (b *Bindings) ListSessions() ([]memory.SessionInfo, error) {
	return memory.ListSessions()
}

// RenameSession updates a session title.
//
// Thin pass-through to agent.RenameSession (v0.4.5) — the
// pre-v0.4.5 path called memory.RenameSession directly, which
// only touched chat.json on disk and left the agent's
// in-memory a.session.Title untouched. Any subsequent
// a.session.Save() (after a Send / tool call / auto-title
// generation) overwrote the rename with the stale in-memory
// title, so the user-visible "rename worked" became "rename
// reverted on next launch". Routing through the agent layer
// keeps the in-memory copy and disk in sync.
func (b *Bindings) RenameSession(sessionID, title string) error {
	return b.agent.RenameSession(sessionID, title)
}

// DeleteSession removes a session and its associated objects.
//
// Thin pass-through to agent.DeleteSession (v0.4.2) — the
// state-machine gate, postTasksWg drain, active-session
// cleanup (Engine close + nil-clear of session/sessionMemory/
// findings), objstore cleanup, sandbox teardown, and dir
// removal all live there as one atomic-from-the-state-machine's-
// perspective operation. See docs/en/session-delete-ux.md.
func (b *Bindings) DeleteSession(sessionID string) error {
	if b.agent == nil {
		return fmt.Errorf("agent not initialised")
	}
	return b.agent.DeleteSession(b.ctx, sessionID)
}

// ExportSession packages a session into a .shellagent bundle via
// a native save dialog. Returns the destination path on success
// or an empty string if the user cancelled the dialog.
//
// For an active session the analysis Engine is closed before the
// bundle copy and re-created via switchAnalysis afterwards (the
// new instance is wired back into the agent so subsequent
// analysis tool calls work normally).
//
// Design: docs/en/session-import-export.md §4 / §6.
func (b *Bindings) ExportSession(sessionID string) (string, error) {
	if b.agent == nil {
		return "", fmt.Errorf("agent not initialised")
	}

	diskSession, err := memory.LoadSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("export: load session: %w", err)
	}

	defaultName := sessionio.SafeBundleFilename(diskSession.Title, sessionID, time.Now())
	dest, err := wailsRuntime.SaveFileDialog(b.ctx, wailsRuntime.SaveDialogOptions{
		DefaultFilename: defaultName,
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "shell-agent-v2 session", Pattern: "*.shellagent"},
		},
	})
	if err != nil {
		return "", err
	}
	if dest == "" {
		// User cancelled — surface as a no-op.
		return "", nil
	}

	current := b.agent.CurrentSession()
	wasActive := current != nil && current.ID == sessionID

	if _, _, err := b.agent.ExportSession(sessionID, dest, version); err != nil {
		// agent.ExportSession may have closed the active session's
		// analysis Engine before failing (e.g. if the bundle write
		// fails). Re-create it so the UI doesn't end up with a
		// dead engine pointer.
		if wasActive {
			b.switchAnalysis(sessionID)
		}
		return "", err
	}

	if wasActive {
		b.switchAnalysis(sessionID)
	}
	return dest, nil
}

// ImportSession opens a .shellagent bundle via a native open
// dialog, extracts it as a fresh session, and switches to it.
// Returns the new session ID, or an empty string if the user
// cancelled the dialog.
//
// Design: docs/en/session-import-export.md §5 / §6.
func (b *Bindings) ImportSession() (string, error) {
	if b.agent == nil {
		return "", fmt.Errorf("agent not initialised")
	}

	src, err := wailsRuntime.OpenFileDialog(b.ctx, wailsRuntime.OpenDialogOptions{
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "shell-agent-v2 session", Pattern: "*.shellagent"},
		},
	})
	if err != nil {
		return "", err
	}
	if src == "" {
		return "", nil
	}

	newID, _, _, _, err := b.agent.ImportSession(src)
	if err != nil {
		return "", err
	}

	// Auto-switch to the newly imported session — mirrors the
	// existing NewSession / NewPrivateSession contract: the binding
	// returns an ID and the session is already active.
	loaded, err := memory.LoadSession(newID)
	if err != nil {
		return "", fmt.Errorf("import: load new session: %w", err)
	}
	b.switchAnalysis(newID)
	if err := b.agent.LoadSession(loaded); err != nil {
		return "", err
	}
	return newID, nil
}

// MessageData is a message for the frontend.
//
// Status is meaningful for `tool-event` rows reconstructed in
// LoadSession (allowed values: "success" / "error"). Other roles
// leave it empty and the frontend ignores it.
//
// ObjectIDs is populated for user records that had attached images
// at send time (Record.ObjectIDs). The frontend turns them into
// `object:<id>` URLs and renders them via ObjectImage so a
// restored conversation shows the same images the user originally
// attached. Without this, restored sessions lose all visual
// context of what the user was discussing.
type MessageData struct {
	Role      string   `json:"role"`
	Content   string   `json:"content"`
	Timestamp string   `json:"timestamp"`
	Status    string   `json:"status,omitempty"`
	ObjectIDs []string `json:"object_ids,omitempty"`
	// ToolCallID is populated for `tool-event` rows reconstructed
	// in LoadSession so the frontend can fetch full args + result
	// via GetToolCallDetails when the user clicks the bubble.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// --- MITL bindings ---
//
// All three resolvers go through resolveMITL which atomically
// consumes the in-flight slot. Stray clicks (no request
// pending) and double-clicks on the same request are no-ops.

func (b *Bindings) resolveMITL(resp agent.MITLResponse) {
	b.mitlMu.Lock()
	slot := b.mitlReq
	b.mitlMu.Unlock()
	if slot == nil {
		return // no request in flight; UI race or stray click
	}
	select {
	case slot.ch <- resp:
	default: // already resolved (e.g. double-click); idempotent
	}
}

// ApproveMITL approves the pending tool execution.
func (b *Bindings) ApproveMITL() {
	b.resolveMITL(agent.MITLResponse{Approved: true})
}

// RejectMITL rejects the pending tool execution without feedback.
func (b *Bindings) RejectMITL() {
	b.resolveMITL(agent.MITLResponse{Approved: false})
}

// RejectMITLWithFeedback rejects with a reason for LLM revision.
func (b *Bindings) RejectMITLWithFeedback(feedback string) {
	b.resolveMITL(agent.MITLResponse{Approved: false, Feedback: feedback})
}

// --- Analysis bindings ---

// GetTables returns metadata for all tables in the current session.
func (b *Bindings) GetTables() []*analysis.TableMeta {
	if b.analysis == nil {
		return nil
	}
	return b.analysis.Tables()
}

// HasData reports whether the current session has analysis data.
func (b *Bindings) HasData() bool {
	if b.analysis == nil {
		return false
	}
	return b.analysis.HasData()
}

// --- Findings bindings ---

// FindingsResult is the JSON-serializable findings data for the frontend.
//
// FindingsResult is a per-session finding for the frontend.
// v0.2.0: SessionID/SessionTitle removed (findings are per-session
// now, the active session is implicit). See docs/en/memory-model.md §4.
type FindingsResult struct {
	ID             string   `json:"id"`
	Content        string   `json:"content"`
	Tags           []string `json:"tags"`
	CreatedLabel   string   `json:"created_label"`
	Source         string   `json:"source"`
	ToolOriginated bool     `json:"tool_originated"`
}

// GetFindings returns the active session's findings (or empty
// if no session loaded).
func (b *Bindings) GetFindings() []FindingsResult {
	all := b.agent.Findings()
	results := make([]FindingsResult, len(all))
	for i, f := range all {
		results[i] = FindingsResult{
			ID:             f.ID,
			Content:        f.Content,
			Tags:           f.Tags,
			CreatedLabel:   f.CreatedLabel,
			Source:         f.Source,
			ToolOriginated: f.ToolOriginated,
		}
	}
	return results
}

// ToolCallDetailsData mirrors agent.ToolCallDetails for the
// frontend's tool-event detail dialog. Surfaced via
// GetToolCallDetails when the user clicks a completed tool-event
// bubble in the chat pane.
type ToolCallDetailsData struct {
	ToolCallID      string `json:"tool_call_id"`
	ToolName        string `json:"tool_name"`
	Arguments       string `json:"arguments"`
	Result          string `json:"result"`
	Status          string `json:"status"`
	CallTimestamp   string `json:"call_timestamp"`
	ResultTimestamp string `json:"result_timestamp"`
}

// GetToolCallDetails returns the recorded args + result for the
// given tool-call ID. Used by the chat pane to populate the
// tool-event detail overlay when a completed bubble is clicked.
func (b *Bindings) GetToolCallDetails(toolCallID string) (ToolCallDetailsData, error) {
	d, err := b.agent.GetToolCallDetails(toolCallID)
	if err != nil {
		return ToolCallDetailsData{}, err
	}
	return ToolCallDetailsData{
		ToolCallID:      d.ToolCallID,
		ToolName:        d.ToolName,
		Arguments:       d.Arguments,
		Result:          d.Result,
		Status:          d.Status,
		CallTimestamp:   d.CallTimestamp,
		ResultTimestamp: d.ResultTimestamp,
	}, nil
}

// --- Settings bindings ---

// MCPProfileData is a MCP profile for the frontend.
type MCPProfileData struct {
	Name        string `json:"name"`
	Binary      string `json:"binary"`
	ProfilePath string `json:"profile_path"`
	Enabled     bool   `json:"enabled"`
}

// BackendBudgetData mirrors config.LocalConfig/VertexAIConfig token settings.
//
// v0.2.0: HotTokenLimit field removed (the v1 destructive
// compaction trigger is gone).
type BackendBudgetData struct {
	MaxContextTokens    int `json:"max_context_tokens"`
	MaxWarmTokens       int `json:"max_warm_tokens"`
	MaxToolResultTokens int `json:"max_tool_result_tokens"`
	OutputReserve       int `json:"output_reserve"`
}

// SandboxData mirrors config.SandboxConfig for the frontend.
type SandboxData struct {
	Enabled        bool   `json:"enabled"`
	Engine         string `json:"engine"`
	Image          string `json:"image"`
	Dockerfile     string `json:"dockerfile"`
	Network        bool   `json:"network"`
	CPULimit       string `json:"cpu_limit"`
	MemoryLimit    string `json:"memory_limit"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// SettingsData is the JSON-serializable settings for the frontend.
type SettingsData struct {
	DefaultBackend       string            `json:"default_backend"`
	LocalEndpoint        string            `json:"local_endpoint"`
	LocalModel           string            `json:"local_model"`
	LocalBudget          BackendBudgetData `json:"local_budget"`
	LocalTimeoutSeconds  int               `json:"local_timeout_seconds"`
	LocalRetryMaxAttempts int              `json:"local_retry_max_attempts"`
	VertexProject        string            `json:"vertex_project"`
	VertexRegion         string            `json:"vertex_region"`
	VertexModel          string            `json:"vertex_model"`
	VertexBudget         BackendBudgetData `json:"vertex_budget"`
	VertexTimeoutSeconds int               `json:"vertex_timeout_seconds"`
	VertexRetryMaxAttempts int             `json:"vertex_retry_max_attempts"`
	Theme          string            `json:"theme"`
	Location       string            `json:"location"`
	MCPProfiles    []MCPProfileData  `json:"mcp_profiles"`
	DisabledTools  []string          `json:"disabled_tools"`
	MITLOverrides  map[string]bool   `json:"mitl_overrides"`
	Sandbox        SandboxData       `json:"sandbox"`
	MaxToolRounds  int               `json:"max_tool_rounds"`
	LogLevel       string            `json:"log_level"` // debug | info | warn | error; "" → info
}

// GetSettings returns current settings.
func (b *Bindings) GetSettings() SettingsData {
	profiles := make([]MCPProfileData, len(b.cfg.Tools.MCPProfiles))
	for i, p := range b.cfg.Tools.MCPProfiles {
		profiles[i] = MCPProfileData{Name: p.Name, Binary: p.Binary, ProfilePath: p.ProfilePath, Enabled: p.Enabled}
	}
	toBudget := func(b config.ContextBudgetConfig) BackendBudgetData {
		return BackendBudgetData{
			MaxContextTokens:    b.MaxContextTokens,
			MaxWarmTokens:       b.MaxWarmTokens,
			MaxToolResultTokens: b.MaxToolResultTokens,
			OutputReserve:       b.OutputReserveResolved(),
		}
	}
	resolveAttempts := func(v int) int {
		if v <= 0 {
			return llm.DefaultMaxAttempts
		}
		return v
	}
	return SettingsData{
		DefaultBackend:        string(b.cfg.LLM.DefaultBackend),
		LocalEndpoint:         b.cfg.LLM.Local.Endpoint,
		LocalModel:            b.cfg.LLM.Local.Model,
		LocalBudget:           toBudget(b.cfg.LLM.Local.ContextBudget),
		LocalTimeoutSeconds:   b.cfg.LLM.Local.LocalRequestTimeout(),
		LocalRetryMaxAttempts: resolveAttempts(b.cfg.LLM.Local.RetryMaxAttempts),
		VertexProject:         b.cfg.LLM.VertexAI.ProjectID,
		VertexRegion:          b.cfg.LLM.VertexAI.Region,
		VertexModel:           b.cfg.LLM.VertexAI.Model,
		VertexBudget:          toBudget(b.cfg.LLM.VertexAI.ContextBudget),
		VertexTimeoutSeconds:  b.cfg.LLM.VertexAI.VertexRequestTimeout(),
		VertexRetryMaxAttempts: resolveAttempts(b.cfg.LLM.VertexAI.RetryMaxAttempts),
		Theme:          b.cfg.UI.Theme,
		Location:       b.cfg.Location,
		MCPProfiles:    profiles,
		DisabledTools:  b.cfg.Tools.DisabledTools,
		MITLOverrides:  b.cfg.Tools.MITLOverrides,
		MaxToolRounds:  b.cfg.Agent.MaxToolRoundsResolved(),
		Sandbox: SandboxData{
			Enabled:        b.cfg.Sandbox.Enabled,
			Engine:         b.cfg.Sandbox.Engine,
			Image:          b.cfg.Sandbox.Image,
			Dockerfile:     b.cfg.Sandbox.Dockerfile,
			Network:        b.cfg.Sandbox.Network,
			CPULimit:       b.cfg.Sandbox.CPULimit,
			MemoryLimit:    b.cfg.Sandbox.MemoryLimit,
			TimeoutSeconds: b.cfg.Sandbox.TimeoutSeconds,
		},
		LogLevel: b.cfg.LogLevelString(),
	}
}

// SaveSettings persists updated settings.
func (b *Bindings) SaveSettings(s SettingsData) error {
	prevSandbox := b.cfg.Sandbox
	// Snapshot LLM config so we can detect any change (model
	// name, endpoint, retry policy, context budget, …) and
	// rebuild the backend live. Without this the SettingsDialog
	// silently saved to disk but the running agent kept calling
	// the previous Local/Vertex client until the next app restart.
	prevLLM := b.cfg.LLM

	b.cfg.LLM.DefaultBackend = config.LLMBackend(s.DefaultBackend)
	b.cfg.LLM.Local.Endpoint = s.LocalEndpoint
	b.cfg.LLM.Local.Model = s.LocalModel
	b.cfg.LLM.Local.ContextBudget = config.ContextBudgetConfig{
		MaxContextTokens:    s.LocalBudget.MaxContextTokens,
		MaxWarmTokens:       s.LocalBudget.MaxWarmTokens,
		MaxToolResultTokens: s.LocalBudget.MaxToolResultTokens,
		OutputReserve:       s.LocalBudget.OutputReserve,
	}
	b.cfg.LLM.Local.RequestTimeoutSeconds = s.LocalTimeoutSeconds
	b.cfg.LLM.Local.RetryMaxAttempts = s.LocalRetryMaxAttempts
	b.cfg.LLM.VertexAI.ProjectID = s.VertexProject
	b.cfg.LLM.VertexAI.Region = s.VertexRegion
	b.cfg.LLM.VertexAI.Model = s.VertexModel
	b.cfg.LLM.VertexAI.ContextBudget = config.ContextBudgetConfig{
		MaxContextTokens:    s.VertexBudget.MaxContextTokens,
		MaxWarmTokens:       s.VertexBudget.MaxWarmTokens,
		MaxToolResultTokens: s.VertexBudget.MaxToolResultTokens,
		OutputReserve:       s.VertexBudget.OutputReserve,
	}
	b.cfg.LLM.VertexAI.RequestTimeoutSeconds = s.VertexTimeoutSeconds
	b.cfg.LLM.VertexAI.RetryMaxAttempts = s.VertexRetryMaxAttempts
	b.cfg.UI.Theme = s.Theme
	b.cfg.Location = s.Location
	b.cfg.Agent.MaxToolRounds = s.MaxToolRounds

	// Update MCP profiles
	profiles := make([]config.MCPProfileConfig, len(s.MCPProfiles))
	for i, p := range s.MCPProfiles {
		profiles[i] = config.MCPProfileConfig{Name: p.Name, Binary: p.Binary, ProfilePath: p.ProfilePath, Enabled: p.Enabled}
	}
	b.cfg.Tools.MCPProfiles = profiles
	b.cfg.Tools.DisabledTools = s.DisabledTools
	b.cfg.Tools.MITLOverrides = s.MITLOverrides

	b.cfg.Sandbox = config.SandboxConfig{
		Enabled:        s.Sandbox.Enabled,
		Engine:         s.Sandbox.Engine,
		Image:          s.Sandbox.Image,
		Dockerfile:     s.Sandbox.Dockerfile,
		Network:        s.Sandbox.Network,
		CPULimit:       s.Sandbox.CPULimit,
		MemoryLimit:    s.Sandbox.MemoryLimit,
		TimeoutSeconds: s.Sandbox.TimeoutSeconds,
	}

	// Logger level — apply live so the user can flip from
	// info → debug for diagnosis without an app restart, then
	// flip back. Empty string normalises to "info" on read so
	// invalid values don't strand the user at an unexpected
	// level.
	prevLogLevel := b.cfg.LogLevelString()
	b.cfg.Logger.Level = s.LogLevel

	if err := b.cfg.Save(); err != nil {
		return err
	}

	if newLevel := b.cfg.LogLevelString(); newLevel != prevLogLevel {
		logger.SetLevel(parseLogLevel(newLevel))
		logger.Info("logger level changed: %s → %s", prevLogLevel, newLevel)
	}

	// If the sandbox config changed, tear down running containers
	// so the next sandbox-* tool call recreates with the new
	// settings. Without this the user could toggle Network off in
	// Settings and an in-flight container would still have the old
	// (allowed) network until they restarted manually.
	if b.agent != nil && prevSandbox != b.cfg.Sandbox {
		b.agent.RestartSandbox()
	}
	// Same idea for the LLM backend: rebuild it whenever the
	// model, endpoint, retry policy, context budget, etc. changed.
	// LLMConfig is a struct of comparable types, so == suffices.
	if b.agent != nil && prevLLM != b.cfg.LLM {
		b.agent.RestartLLMBackend()
	}
	return nil
}

// RestartMCP restarts all MCP guardian processes from current config.
func (b *Bindings) RestartMCP() {
	if b.agent != nil {
		b.agent.RestartGuardians()
	}
}

// RestartLLMBackend rebuilds the agent's LLM backend so timeout
// and other per-backend settings take effect live, without an
// app restart.
func (b *Bindings) RestartLLMBackend() {
	if b.agent != nil {
		b.agent.RestartLLMBackend()
	}
}

// SidebarPrefsData is the persisted sidebar layout state — width
// in pixels and whether it starts collapsed. Returned by
// GetSidebarPrefs so the frontend can restore the user's
// preferred layout at startup; updated via SaveSidebarPrefs on
// resize-end and on collapse toggle.
type SidebarPrefsData struct {
	Width     int  `json:"width"`
	Collapsed bool `json:"collapsed"`
}

// GetSidebarPrefs returns the persisted sidebar width and
// collapsed flag, falling back to the package default width
// when the config has never been written.
func (b *Bindings) GetSidebarPrefs() SidebarPrefsData {
	w := b.cfg.UI.SidebarWidth
	if w <= 0 {
		w = config.DefaultSidebarWidth
	}
	return SidebarPrefsData{
		Width:     w,
		Collapsed: b.cfg.UI.SidebarCollapsed,
	}
}

// SaveSidebarPrefs persists the sidebar layout state. Lightweight
// because it's called on every resize-end / collapse toggle.
func (b *Bindings) SaveSidebarPrefs(width int, collapsed bool) error {
	if width > 0 {
		b.cfg.UI.SidebarWidth = width
	}
	b.cfg.UI.SidebarCollapsed = collapsed
	return b.cfg.Save()
}

// RestartSandbox tears down all sandbox containers and re-reads
// cfg.Sandbox so Settings changes (Enabled / Engine / Image / Network
// / limits) take effect without an app restart.
func (b *Bindings) RestartSandbox() {
	if b.agent != nil {
		b.agent.RestartSandbox()
	}
}

// SandboxImageInfo is one entry in the built-images library
// shown in the Sandbox tab.
type SandboxImageInfo struct {
	Tag       string `json:"tag"`
	Created   string `json:"created"` // ISO8601 (empty when unknown)
	SizeBytes int64  `json:"size_bytes"`
	Active    bool   `json:"active"` // true when this is cfg.Sandbox.Image
}

// SandboxImageStatus is the snapshot the Settings dialog
// reads on open and after each build event.
type SandboxImageStatus struct {
	ActiveTag             string             `json:"active_tag"`              // cfg.Sandbox.Image
	ActiveReady           bool               `json:"active_ready"`            // engine has the active tag locally
	ActivePinnedByDigest  bool               `json:"active_pinned_by_digest"` // image ref is digest-pinned (or locally content-addressed); see security-hardening-2.md H5
	Building              bool               `json:"building"`                // a build is in flight
	RecommendedDockerfile string             `json:"recommended_dockerfile"`  // imagebuild.RecommendedDockerfile
	CurrentDockerfile     string             `json:"current_dockerfile"`      // cfg.Sandbox.Dockerfile or recommended
	Images                []SandboxImageInfo `json:"images"`                  // locally-built sandbox images
}

// GetSandboxImageStatus is the "is the sandbox usable?" probe
// the Settings dialog reads on open and after each build event.
// Returns a usable shape even when no engine is available.
func (b *Bindings) GetSandboxImageStatus() SandboxImageStatus {
	s := SandboxImageStatus{
		RecommendedDockerfile: imagebuild.RecommendedDockerfile,
	}
	if b.cfg != nil {
		s.ActiveTag = b.cfg.Sandbox.Image
		s.ActivePinnedByDigest = imagebuild.IsImageDigestPinned(s.ActiveTag)
		s.CurrentDockerfile = b.cfg.Sandbox.Dockerfile
		if s.CurrentDockerfile == "" {
			s.CurrentDockerfile = imagebuild.RecommendedDockerfile
		}
	}
	b.buildMu.Lock()
	s.Building = b.buildInFlight
	b.buildMu.Unlock()

	eng, err := sandbox.NewCLI(b.sandboxConfigForProbe())
	if err != nil {
		return s
	}

	// List locally-built sandbox images. Engine errors map to
	// an empty list (Settings just shows "no images yet").
	if infos, err := eng.ListImages(b.ctx); err == nil {
		s.Images = make([]SandboxImageInfo, 0, len(infos))
		for _, info := range infos {
			created := ""
			if !info.Created.IsZero() {
				created = info.Created.UTC().Format("2006-01-02T15:04:05Z")
			}
			s.Images = append(s.Images, SandboxImageInfo{
				Tag:       info.Tag,
				Created:   created,
				SizeBytes: info.SizeBytes,
				Active:    info.Tag == s.ActiveTag,
			})
		}
	}

	if s.ActiveTag != "" {
		if ready, err := eng.ImageReady(b.ctx, s.ActiveTag); err == nil && ready {
			s.ActiveReady = true
		}
	}
	return s
}

// sandboxConfigForProbe builds a transient sandbox.Config used
// only to detect the engine binary. The work-dir / network /
// limits don't matter for ImageReady.
func (b *Bindings) sandboxConfigForProbe() sandbox.Config {
	rs := b.cfg.ResolvedSandbox()
	return sandbox.Config{
		Engine:      rs.Engine,
		Image:       rs.Image,
		SessionsDir: filepath.Join(config.DataDir(), "sessions"),
	}
}

// BuildSandboxImage starts a build using the user's
// Dockerfile (cfg.Sandbox.Dockerfile, falling back to
// imagebuild.RecommendedDockerfile). Returns immediately;
// progress streams via Wails events:
//
//	"sandbox:build:line"  payload {"line": <stdout|stderr line>}
//	"sandbox:build:done"  payload {"tag": <tag>, "error": <"" on success>}
//
// Concurrent calls return ErrBuildInProgress.
//
// On success the new tag is recorded as cfg.Sandbox.Image
// (becomes Active), the config is saved, and the agent's
// sandbox is restarted so the readiness gate re-evaluates
// without waiting for the next SaveSettings.
func (b *Bindings) BuildSandboxImage() error {
	b.buildMu.Lock()
	if b.buildInFlight {
		b.buildMu.Unlock()
		return ErrBuildInProgress
	}
	b.buildInFlight = true
	b.buildMu.Unlock()

	eng, err := sandbox.NewCLI(b.sandboxConfigForProbe())
	if err != nil {
		b.buildMu.Lock()
		b.buildInFlight = false
		b.buildMu.Unlock()
		return err
	}

	dockerfile := ""
	if b.cfg != nil {
		dockerfile = b.cfg.Sandbox.Dockerfile
	}
	if dockerfile == "" {
		dockerfile = imagebuild.RecommendedDockerfile
	}

	go func() {
		ctx := b.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		tag, buildErr := eng.BuildImage(ctx, dockerfile, func(line string) {
			wailsRuntime.EventsEmit(b.ctx, "sandbox:build:line", map[string]any{"line": line})
		})

		// Persist cfg + clear buildInFlight BEFORE emitting
		// :done so the frontend's refreshImageStatus reads
		// the new cfg.Sandbox.Image. Otherwise the dialog
		// re-fetches with the old (empty) Image and the
		// "Built images" list looks unchanged.
		if buildErr == nil && tag != "" && b.cfg != nil {
			b.cfg.Sandbox.Dockerfile = dockerfile
			b.cfg.Sandbox.Image = tag
			_ = b.cfg.Save()
			if b.agent != nil {
				b.agent.RestartSandbox()
			}
		}

		b.buildMu.Lock()
		b.buildInFlight = false
		b.buildMu.Unlock()

		errStr := ""
		if buildErr != nil {
			errStr = buildErr.Error()
		}
		wailsRuntime.EventsEmit(b.ctx, "sandbox:build:done", map[string]any{
			"tag":   tag,
			"error": errStr,
		})
	}()
	return nil
}

// ListSandboxImages returns the current built-images
// library snapshot, mirroring what's inside
// GetSandboxImageStatus().Images. Provided as its own
// binding so the dialog can re-fetch the list cheaply
// after a Delete without re-running the heavier readiness
// probe.
func (b *Bindings) ListSandboxImages() []SandboxImageInfo {
	return b.GetSandboxImageStatus().Images
}

// SetActiveSandboxImage sets cfg.Sandbox.Image to tag and
// triggers RestartSandbox so the readiness gate
// re-evaluates immediately. Returns an error if tag isn't
// present on the engine — the dialog should keep its
// current selection if so.
func (b *Bindings) SetActiveSandboxImage(tag string) error {
	if b.cfg == nil {
		return fmt.Errorf("no config")
	}
	if tag != "" {
		eng, err := sandbox.NewCLI(b.sandboxConfigForProbe())
		if err != nil {
			return err
		}
		ready, err := eng.ImageReady(b.ctx, tag)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("image %q not present on engine", tag)
		}
	}
	b.cfg.Sandbox.Image = tag
	if err := b.cfg.Save(); err != nil {
		return err
	}
	if b.agent != nil {
		b.agent.RestartSandbox()
	}
	return nil
}

// RemoveSandboxImage deletes the given tag from the engine.
// If the tag was Active, cfg.Sandbox.Image is cleared and
// the agent's sandbox is restarted (which causes the tools
// to unregister, since the gate now fails the empty check).
func (b *Bindings) RemoveSandboxImage(tag string) error {
	logger.Info("sandbox: RemoveSandboxImage tag=%q", tag)
	eng, err := sandbox.NewCLI(b.sandboxConfigForProbe())
	if err != nil {
		logger.Error("sandbox: RemoveSandboxImage NewCLI: %v", err)
		return err
	}
	if err := eng.RemoveImage(b.ctx, tag); err != nil {
		logger.Error("sandbox: RemoveSandboxImage engine.RemoveImage: %v", err)
		return err
	}
	logger.Info("sandbox: RemoveSandboxImage tag=%q removed", tag)
	if b.cfg != nil && b.cfg.Sandbox.Image == tag {
		b.cfg.Sandbox.Image = ""
		_ = b.cfg.Save()
		if b.agent != nil {
			b.agent.RestartSandbox()
		}
	}
	return nil
}

// GetMCPStatus returns the status of all MCP guardian profiles.
func (b *Bindings) GetMCPStatus() []agent.MCPStatus {
	if b.agent == nil {
		return nil
	}
	return b.agent.MCPStatuses()
}

// --- Image bindings ---

// SaveImage stores a data URL image and returns its ID.
func (b *Bindings) SaveImage(dataURL string) (string, error) {
	meta, err := b.objects.SaveDataURL(dataURL, "")
	if err != nil {
		return "", err
	}
	return meta.ID, nil
}

// GetImageDataURL loads an image by ID and returns a data URL.
func (b *Bindings) GetImageDataURL(id string) (string, error) {
	return b.objects.LoadAsDataURL(id)
}

// --- Object repository bindings ---

// ObjectInfo is the JSON-serializable view of an object's metadata.
// Lines / Tokens (v0.5) are populated only for text-bearing types
// (markdown / report); other types omit them via omitempty.
type ObjectInfo struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	MimeType  string `json:"mime_type"`
	OrigName  string `json:"orig_name"`
	CreatedAt string `json:"created_at"`
	SessionID string `json:"session_id"`
	Size      int64  `json:"size"`
	Lines     int    `json:"lines,omitempty"`
	Tokens    int    `json:"tokens,omitempty"`
}

// ListObjects returns metadata for every object in the central repository,
// newest-first.
func (b *Bindings) ListObjects() []ObjectInfo {
	all := b.objects.All()
	result := make([]ObjectInfo, 0, len(all))
	for _, m := range all {
		result = append(result, ObjectInfo{
			ID:        m.ID,
			Type:      string(m.Type),
			MimeType:  m.MimeType,
			OrigName:  m.OrigName,
			CreatedAt: m.CreatedAt.Format("2006-01-02 15:04:05"),
			SessionID: m.SessionID,
			Size:      m.Size,
			Lines:     m.Lines,
			Tokens:    m.Tokens,
		})
	}
	// Newest first.
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result
}

// GetSessionObjects returns the objects belonging to the given
// session, newest-first. Backs the per-session Data → Objects
// sub-section in the new chat-pane disclosure (information-display
// redesign Phase 1).
func (b *Bindings) GetSessionObjects(sessionID string) []ObjectInfo {
	if b.objects == nil || sessionID == "" {
		return []ObjectInfo{}
	}
	objs := b.objects.ListBySession(sessionID)
	result := make([]ObjectInfo, 0, len(objs))
	for _, m := range objs {
		result = append(result, ObjectInfo{
			ID:        m.ID,
			Type:      string(m.Type),
			MimeType:  m.MimeType,
			OrigName:  m.OrigName,
			CreatedAt: m.CreatedAt.Format("2006-01-02 15:04:05"),
			SessionID: m.SessionID,
			Size:      m.Size,
			Lines:     m.Lines,
			Tokens:    m.Tokens,
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result
}

// TableInfoData is the JSON-serializable summary of one DuckDB
// table loaded into a session's analysis engine.
type TableInfoData struct {
	Name        string   `json:"name"`
	RowCount    int64    `json:"row_count"`
	Columns     []string `json:"columns"`
	Description string   `json:"description,omitempty"`
}

// GetSessionTables returns the analysis-engine tables for the
// given session. Returns an empty slice (not error) when no
// engine is wired up or the session has never loaded data —
// callers render "no tables" themselves.
func (b *Bindings) GetSessionTables(sessionID string) []TableInfoData {
	if b.analysis == nil || sessionID == "" {
		return []TableInfoData{}
	}
	tables := b.analysis.Tables()
	result := make([]TableInfoData, 0, len(tables))
	for _, t := range tables {
		result = append(result, TableInfoData{
			Name:        t.Name,
			RowCount:    t.RowCount,
			Columns:     t.Columns,
			Description: t.Description,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// TablePreviewData is what the UI receives when the user clicks a
// table to preview rows.
type TablePreviewData struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Total     int64    `json:"total"`
	Truncated bool     `json:"truncated"`
}

// PreviewTable returns the first `limit` rows of the named table
// for UI display. limit ≤ 0 falls back to the engine default
// (currently 20). Returns an error string in the wrapper if the
// engine is missing or the table is unknown.
func (b *Bindings) PreviewTable(tableName string, limit int) (TablePreviewData, error) {
	if b.analysis == nil {
		return TablePreviewData{}, fmt.Errorf("no analysis engine for current session")
	}
	p, err := b.analysis.PreviewTable(tableName, limit)
	if err != nil {
		return TablePreviewData{}, err
	}
	return TablePreviewData{
		Columns:   p.Columns,
		Rows:      p.Rows,
		Total:     p.Total,
		Truncated: p.Truncated,
	}, nil
}

// WorkFileData is one /work entry surfaced to the UI.
type WorkFileData struct {
	Path  string `json:"path"`  // relative to /work
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix milli
}

// GetWorkFiles lists the contents of the session's sandbox /work
// directory regardless of whether the engine is currently running.
// The directory is just a host-side mount, so reading it is
// always safe and stays consistent with what `sandbox-info` shows
// the LLM. Caps at 200 entries to avoid sending huge payloads.
func (b *Bindings) GetWorkFiles(sessionID string) []WorkFileData {
	if sessionID == "" {
		return []WorkFileData{}
	}
	workDir := filepath.Join(memory.SessionDir(sessionID), "work")
	if _, err := os.Stat(workDir); err != nil {
		return []WorkFileData{}
	}
	files := sandbox.ListWorkFiles(workDir, 200)
	result := make([]WorkFileData, 0, len(files))
	for _, f := range files {
		result = append(result, WorkFileData{
			Path:  filepath.ToSlash(f.Path),
			Size:  f.Size,
			MTime: f.MTime.UnixMilli(),
		})
	}
	return result
}

// DeleteObject removes an object by ID. Returns nil if the ID didn't exist.
func (b *Bindings) DeleteObject(id string) error {
	return b.objects.Delete(id)
}

// DeleteObjects bulk-removes objects. Returns the count actually deleted
// (entries that didn't exist are not counted as deletions).
func (b *Bindings) DeleteObjects(ids []string) (int, error) {
	deleted := 0
	for _, id := range ids {
		if _, ok := b.objects.Get(id); !ok {
			continue
		}
		if err := b.objects.Delete(id); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// GetObjectText returns the object's stored bytes as a string. Intended
// for previewing TypeReport (markdown) and other text-like types in the
// UI; the caller is responsible for size sanity.
func (b *Bindings) GetObjectText(id string) (string, error) {
	if _, ok := b.objects.Get(id); !ok {
		return "", fmt.Errorf("object %s not found", id)
	}
	data, err := os.ReadFile(b.objects.DataPath(id))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ExportObject opens a save dialog and writes the object's bytes to disk.
// The default filename uses the object's OrigName when set, otherwise
// the ID with an extension derived from MimeType.
//
// For TypeReport objects, object:ID image references in the markdown
// are inlined as data URLs so the exported file is self-contained —
// matches SaveReport's behaviour.
func (b *Bindings) ExportObject(id string) error {
	meta, ok := b.objects.Get(id)
	if !ok {
		return fmt.Errorf("object %s not found", id)
	}
	defaultName := meta.OrigName
	if defaultName == "" {
		defaultName = id + extensionForMime(meta.MimeType)
	}
	path, err := wailsRuntime.SaveFileDialog(b.ctx, wailsRuntime.SaveDialogOptions{
		DefaultFilename: defaultName,
	})
	if err != nil || path == "" {
		return err
	}
	src := b.objects.DataPath(id)
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if meta.Type == objstore.TypeReport {
		data = []byte(b.resolveObjectRefsForExport(string(data)))
	}
	return os.WriteFile(path, data, 0644)
}

func extensionForMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "text/markdown":
		return ".md"
	case "application/json":
		return ".json"
	case "text/plain":
		return ".txt"
	}
	return ""
}

// --- Report bindings ---

// SaveReport saves markdown content to a file via save dialog.
// object:ID references in images are resolved to base64 data URLs
// so the exported file is self-contained.
func (b *Bindings) SaveReport(content, filename string) error {
	path, err := wailsRuntime.SaveFileDialog(b.ctx, wailsRuntime.SaveDialogOptions{
		DefaultFilename: filename,
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "Markdown", Pattern: "*.md"},
		},
	})
	if err != nil || path == "" {
		return err
	}

	// Resolve object:ID references to data URLs for self-contained export
	resolved := b.resolveObjectRefsForExport(content)
	return os.WriteFile(path, []byte(resolved), 0644)
}

// resolveObjectRefsForExport replaces object:ID image refs with data URLs.
func (b *Bindings) resolveObjectRefsForExport(content string) string {
	if b.objects == nil || !strings.Contains(content, "object:") {
		return content
	}
	result := content
	for {
		idx := strings.Index(result, "(object:")
		if idx < 0 {
			break
		}
		end := strings.Index(result[idx:], ")")
		if end < 0 {
			break
		}
		id := result[idx+8 : idx+end]
		du, err := b.objects.LoadAsDataURL(id)
		if err == nil && du != "" {
			result = result[:idx+1] + du + result[idx+end:]
		} else {
			// Skip unresolvable reference to avoid infinite loop
			result = result[:idx] + "(missing-object:" + result[idx+8:]
		}
	}
	return result
}

// --- Tools bindings ---

// ToolInfo describes a tool for the frontend.
//
// MITLDefault is the gate's default for this tool ignoring any
// MITLOverrides entry. The Settings UI uses it so the toggle's
// "default" state matches the dispatcher's actual default — see
// security-hardening-2.md follow-up: prior to wiring this through,
// the UI computed the default locally from category/source and
// went out of sync after the analysisToolMITLDefault map was
// introduced.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Source      string `json:"source"` // "builtin", "analysis", "shell", "mcp"
	MITLDefault bool   `json:"mitl_default"`
}

// GetTools returns all available tools.
func (b *Bindings) GetTools() []ToolInfo {
	items := b.agent.ListTools()
	result := make([]ToolInfo, len(items))
	for i, item := range items {
		result[i] = ToolInfo{Name: item.Name, Description: item.Description, Category: item.Category, Source: item.Source, MITLDefault: item.MITLDefault}
	}
	return result
}

// --- Global / Session Memory bindings (v0.2.0) ---

// GlobalMemoryData is a cross-session memory entry for the frontend.
//
// Renamed from PinnedMemoryData in v0.2.0; only preference / decision
// categories belong here. Source / SessionID / ToolOriginated /
// CreatedAt drive the trust badge and learned-date in the sidebar.
type GlobalMemoryData struct {
	Fact           string `json:"fact"`
	NativeFact     string `json:"native_fact"`
	Category       string `json:"category"`
	Source         string `json:"source"`
	SessionID      string `json:"session_id"`
	ToolOriginated bool   `json:"tool_originated"`
	CreatedAt      string `json:"created_at"` // RFC3339 if present, empty otherwise
}

// SessionMemoryData is a per-session memory entry for the frontend.
// Only fact / context categories belong here; entries die with the
// session unless promoted to Global Memory.
type SessionMemoryData struct {
	Fact           string `json:"fact"`
	NativeFact     string `json:"native_fact"`
	Category       string `json:"category"`
	Source         string `json:"source"`
	ToolOriginated bool   `json:"tool_originated"`
	CreatedAt      string `json:"created_at"`
}

// GetGlobalMemories returns all Global Memory entries.
func (b *Bindings) GetGlobalMemories() []GlobalMemoryData {
	all := b.agent.GlobalMemoryAll()
	result := make([]GlobalMemoryData, len(all))
	for i, f := range all {
		createdAt := ""
		if !f.CreatedAt.IsZero() {
			createdAt = f.CreatedAt.Format(time.RFC3339)
		}
		result[i] = GlobalMemoryData{
			Fact:           f.Fact,
			NativeFact:     f.NativeFact,
			Category:       f.Category,
			Source:         f.Source,
			SessionID:      f.SessionID,
			ToolOriginated: f.ToolOriginated,
			CreatedAt:      createdAt,
		}
	}
	return result
}

// UpdateGlobalMemory creates or updates a Global Memory entry.
// Signature: (fact, native, category) — direct edit path from the
// settings UI.
func (b *Bindings) UpdateGlobalMemory(fact, native, category string) error {
	return b.agent.GlobalMemorySet(fact, native, category)
}

// DeleteGlobalMemory removes a Global Memory entry by fact text.
func (b *Bindings) DeleteGlobalMemory(fact string) error {
	return b.agent.GlobalMemoryDelete(fact)
}

// DeleteGlobalMemories bulk-removes Global Memory entries by fact
// text. Returns count actually deleted.
func (b *Bindings) DeleteGlobalMemories(facts []string) (int, error) {
	return b.agent.GlobalMemoryDeleteByFacts(facts)
}

// GetSessionMemories returns the active session's Session Memory
// entries, or empty when no session is loaded.
func (b *Bindings) GetSessionMemories() []SessionMemoryData {
	all := b.agent.SessionMemoryAll()
	result := make([]SessionMemoryData, len(all))
	for i, f := range all {
		createdAt := ""
		if !f.CreatedAt.IsZero() {
			createdAt = f.CreatedAt.Format(time.RFC3339)
		}
		result[i] = SessionMemoryData{
			Fact:           f.Fact,
			NativeFact:     f.NativeFact,
			Category:       f.Category,
			Source:         f.Source,
			ToolOriginated: f.ToolOriginated,
			CreatedAt:      createdAt,
		}
	}
	return result
}

// DeleteSessionMemories bulk-removes Session Memory entries from
// the active session. Returns count actually deleted.
func (b *Bindings) DeleteSessionMemories(facts []string) (int, error) {
	return b.agent.SessionMemoryDeleteByFacts(facts)
}

// PinSessionMemory promotes a Session Memory entry into Global
// Memory under the chosen category (preference|decision). Source
// is stamped as promoted_from_session_memory; the original Session
// Memory entry stays in place.
func (b *Bindings) PinSessionMemory(fact, category string) error {
	return b.agent.PromoteSessionMemoryToGlobal(fact, category)
}

// PinFinding promotes a Findings entry into Global Memory under
// the chosen category. Source is stamped as promoted_from_finding.
func (b *Bindings) PinFinding(id, category string) error {
	return b.agent.PromoteFindingToGlobal(id, category)
}

// DeleteFindings bulk-removes findings by ID. Returns count actually deleted.
func (b *Bindings) DeleteFindings(ids []string) (int, error) {
	return b.agent.FindingsDeleteByIDs(ids)
}

// --- LLM Status bindings ---

// LLMStatusData is the LLM status for the frontend.
type LLMStatusData struct {
	Backend       string `json:"backend"`
	HotMessages   int    `json:"hot_messages"`
	WarmSummaries int    `json:"warm_summaries"`
	SessionID     string `json:"session_id"`
	PromptTokens  int    `json:"prompt_tokens"`
	OutputTokens  int    `json:"output_tokens"`
}

// GetLLMStatus returns the current LLM and memory status.
func (b *Bindings) GetLLMStatus() LLMStatusData {
	if b.agent == nil {
		// Frontend polls this from useEffect; if startup is still
		// in flight, return zero values rather than panicking.
		return LLMStatusData{}
	}
	s := b.agent.LLMStatus()
	return LLMStatusData{
		Backend:       s.Backend,
		HotMessages:   s.HotMessages,
		WarmSummaries: s.WarmSummaries,
		SessionID:     s.SessionID,
		PromptTokens:  s.PromptTokens,
		OutputTokens:  s.OutputTokens,
	}
}

// --- Info ---

// Version returns the application version.
func (b *Bindings) Version() string {
	return version
}

// --- internal ---

func (b *Bindings) switchAnalysis(sessionID string) {
	if b.analysis != nil {
		b.analysis.Close()
	}
	b.analysis = analysis.New(sessionID)
	// Eagerly reopen if a DuckDB file already exists for this
	// session — otherwise the engine's tables map stays empty
	// until the first analysis tool call, and HasData() (used to
	// gate which tools are advertised to the LLM) keeps returning
	// false even though the data is sitting on disk.
	if err := b.analysis.OpenIfExists(); err != nil {
		logger.Error("switchAnalysis: OpenIfExists failed: %v", err)
	}
	b.agent.SetAnalysis(b.analysis)
}

func nowUnixMilli() int64 {
	return time.Now().UnixMilli()
}
