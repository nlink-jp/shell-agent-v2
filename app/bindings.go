package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"path/filepath"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/bundled"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Bindings is the thin Wails binding layer.
// All business logic is delegated to the agent and analysis packages.
type Bindings struct {
	ctx        context.Context
	agent      *agent.Agent
	cfg        *config.Config
	analysis   *analysis.Engine
	mitlChan   chan agent.MITLResponse
	objects    *objstore.Store
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
	logger.Info("shell-agent-v2 starting, config=%s", config.ConfigPath())

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
	b.agent.SetPinnedHandler(func() {
		wailsRuntime.EventsEmit(b.ctx, "pinned:updated", nil)
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
		wailsRuntime.EventsEmit(b.ctx, "agent:activity", payload)
	})
	b.mitlChan = make(chan agent.MITLResponse, 1)
	b.agent.SetMITLHandler(func(req agent.MITLRequest) agent.MITLResponse {
		wailsRuntime.EventsEmit(b.ctx, "mitl:request", map[string]any{
			"tool_name": req.ToolName,
			"arguments": req.Arguments,
			"category":  req.Category,
		})
		return <-b.mitlChan
	})

	// Restore window position and size
	w := cfg.UI.Window
	if w.Width > 0 && w.Height > 0 {
		wailsRuntime.WindowSetSize(ctx, w.Width, w.Height)
		wailsRuntime.WindowSetPosition(ctx, w.X, w.Y)
	}
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
	return b.agent.Send(b.ctx, message)
}

// SendWithImages sends a user message with images to the agent.
// Images are saved to objstore first; ObjectIDs are passed to the agent.
func (b *Bindings) SendWithImages(message string, imageDataURLs []string) (string, error) {
	// Save images to objstore, collect IDs and data URLs for LLM
	var objectIDs []string
	var dataURLs []string
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
		objectIDs = append(objectIDs, meta.ID)
		dataURLs = append(dataURLs, du) // keep data URL for LLM context
	}
	return b.agent.SendWithImages(b.ctx, message, objectIDs, dataURLs)
}

// Abort cancels the current agent task.
func (b *Bindings) Abort() {
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
	if b.IsBusy() {
		return "", fmt.Errorf("agent is busy")
	}

	session := &memory.Session{
		ID:      fmt.Sprintf("sess-%d", nowUnixMilli()),
		Title:   "New Session",
		Records: []memory.Record{},
	}
	if err := session.Save(); err != nil {
		return "", err
	}

	b.switchAnalysis(session.ID)
	if err := b.agent.LoadSession(session); err != nil {
		return "", err
	}

	return session.ID, nil
}

// LoadSession switches to an existing session and returns its messages.
func (b *Bindings) LoadSession(sessionID string) ([]MessageData, error) {
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
	for _, r := range session.Records {
		switch r.Role {
		case "tool":
			continue
		case "assistant":
			if strings.HasPrefix(r.Content, "[Calling:") {
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
		default:
			msgs = append(msgs, MessageData{
				Role: r.Role, Content: r.Content,
				Timestamp: r.Timestamp.Format("15:04:05"),
			})
		}
	}
	return msgs, nil
}

// ListSessions returns all sessions.
func (b *Bindings) ListSessions() ([]memory.SessionInfo, error) {
	return memory.ListSessions()
}

// RenameSession updates a session title.
func (b *Bindings) RenameSession(sessionID, title string) error {
	return memory.RenameSession(sessionID, title)
}

// DeleteSession removes a session and its associated objects.
func (b *Bindings) DeleteSession(sessionID string) error {
	if b.IsBusy() {
		return fmt.Errorf("agent is busy")
	}
	// Clean up objstore objects for this session
	if b.objects != nil {
		_ = b.objects.DeleteBySession(sessionID)
	}
	// Clean up findings originating from this session
	if b.agent != nil {
		b.agent.DeleteFindingsBySession(sessionID)
		// Tear down the session's sandbox container, if any.
		_ = b.agent.SandboxStop(b.ctx, sessionID)
	}
	return memory.DeleteSessionDir(sessionID)
}

// MessageData is a message for the frontend.
type MessageData struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// --- MITL bindings ---

// ApproveMITL approves the pending tool execution.
func (b *Bindings) ApproveMITL() {
	select {
	case b.mitlChan <- agent.MITLResponse{Approved: true}:
	default:
	}
}

// RejectMITL rejects the pending tool execution without feedback.
func (b *Bindings) RejectMITL() {
	select {
	case b.mitlChan <- agent.MITLResponse{Approved: false}:
	default:
	}
}

// RejectMITLWithFeedback rejects with a reason for LLM revision.
func (b *Bindings) RejectMITLWithFeedback(feedback string) {
	select {
	case b.mitlChan <- agent.MITLResponse{Approved: false, Feedback: feedback}:
	default:
	}
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
type FindingsResult struct {
	ID            string   `json:"id"`
	Content       string   `json:"content"`
	SessionID     string   `json:"session_id"`
	SessionTitle  string   `json:"session_title"`
	Tags          []string `json:"tags"`
	CreatedLabel  string   `json:"created_label"`
}

// GetFindings returns all global findings.
func (b *Bindings) GetFindings() []FindingsResult {
	all := b.agent.Findings()
	results := make([]FindingsResult, len(all))
	for i, f := range all {
		results[i] = FindingsResult{
			ID:           f.ID,
			Content:      f.Content,
			SessionID:    f.OriginSessionID,
			SessionTitle: f.OriginSessionTitle,
			Tags:         f.Tags,
			CreatedLabel: f.CreatedLabel,
		}
	}
	return results
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
type BackendBudgetData struct {
	HotTokenLimit       int `json:"hot_token_limit"`
	MaxContextTokens    int `json:"max_context_tokens"`
	MaxWarmTokens       int `json:"max_warm_tokens"`
	MaxToolResultTokens int `json:"max_tool_result_tokens"`
}

// SandboxData mirrors config.SandboxConfig for the frontend.
type SandboxData struct {
	Enabled        bool   `json:"enabled"`
	Engine         string `json:"engine"`
	Image          string `json:"image"`
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
	VertexProject        string            `json:"vertex_project"`
	VertexRegion         string            `json:"vertex_region"`
	VertexModel          string            `json:"vertex_model"`
	VertexBudget         BackendBudgetData `json:"vertex_budget"`
	VertexTimeoutSeconds int               `json:"vertex_timeout_seconds"`
	Theme          string            `json:"theme"`
	Location       string            `json:"location"`
	MCPProfiles    []MCPProfileData  `json:"mcp_profiles"`
	DisabledTools  []string          `json:"disabled_tools"`
	MITLOverrides  map[string]bool   `json:"mitl_overrides"`
	MemoryUseV2    bool              `json:"memory_use_v2"`
	Sandbox        SandboxData       `json:"sandbox"`
}

// GetSettings returns current settings.
func (b *Bindings) GetSettings() SettingsData {
	profiles := make([]MCPProfileData, len(b.cfg.Tools.MCPProfiles))
	for i, p := range b.cfg.Tools.MCPProfiles {
		profiles[i] = MCPProfileData{Name: p.Name, Binary: p.Binary, ProfilePath: p.ProfilePath, Enabled: p.Enabled}
	}
	toBudget := func(hot int, b config.ContextBudgetConfig) BackendBudgetData {
		return BackendBudgetData{
			HotTokenLimit:       hot,
			MaxContextTokens:    b.MaxContextTokens,
			MaxWarmTokens:       b.MaxWarmTokens,
			MaxToolResultTokens: b.MaxToolResultTokens,
		}
	}
	return SettingsData{
		DefaultBackend:       string(b.cfg.LLM.DefaultBackend),
		LocalEndpoint:        b.cfg.LLM.Local.Endpoint,
		LocalModel:           b.cfg.LLM.Local.Model,
		LocalBudget:          toBudget(b.cfg.LLM.Local.HotTokenLimit, b.cfg.LLM.Local.ContextBudget),
		LocalTimeoutSeconds:  b.cfg.LLM.Local.LocalRequestTimeout(),
		VertexProject:        b.cfg.LLM.VertexAI.ProjectID,
		VertexRegion:         b.cfg.LLM.VertexAI.Region,
		VertexModel:          b.cfg.LLM.VertexAI.Model,
		VertexBudget:         toBudget(b.cfg.LLM.VertexAI.HotTokenLimit, b.cfg.LLM.VertexAI.ContextBudget),
		VertexTimeoutSeconds: b.cfg.LLM.VertexAI.VertexRequestTimeout(),
		Theme:          b.cfg.UI.Theme,
		Location:       b.cfg.Location,
		MCPProfiles:    profiles,
		DisabledTools:  b.cfg.Tools.DisabledTools,
		MITLOverrides:  b.cfg.Tools.MITLOverrides,
		MemoryUseV2:    b.cfg.Memory.UseV2,
		Sandbox: SandboxData{
			Enabled:        b.cfg.Sandbox.Enabled,
			Engine:         b.cfg.Sandbox.Engine,
			Image:          b.cfg.Sandbox.Image,
			Network:        b.cfg.Sandbox.Network,
			CPULimit:       b.cfg.Sandbox.CPULimit,
			MemoryLimit:    b.cfg.Sandbox.MemoryLimit,
			TimeoutSeconds: b.cfg.Sandbox.TimeoutSeconds,
		},
	}
}

// SaveSettings persists updated settings.
func (b *Bindings) SaveSettings(s SettingsData) error {
	b.cfg.LLM.DefaultBackend = config.LLMBackend(s.DefaultBackend)
	b.cfg.LLM.Local.Endpoint = s.LocalEndpoint
	b.cfg.LLM.Local.Model = s.LocalModel
	b.cfg.LLM.Local.HotTokenLimit = s.LocalBudget.HotTokenLimit
	b.cfg.LLM.Local.ContextBudget = config.ContextBudgetConfig{
		MaxContextTokens:    s.LocalBudget.MaxContextTokens,
		MaxWarmTokens:       s.LocalBudget.MaxWarmTokens,
		MaxToolResultTokens: s.LocalBudget.MaxToolResultTokens,
	}
	b.cfg.LLM.Local.RequestTimeoutSeconds = s.LocalTimeoutSeconds
	b.cfg.LLM.VertexAI.ProjectID = s.VertexProject
	b.cfg.LLM.VertexAI.Region = s.VertexRegion
	b.cfg.LLM.VertexAI.Model = s.VertexModel
	b.cfg.LLM.VertexAI.HotTokenLimit = s.VertexBudget.HotTokenLimit
	b.cfg.LLM.VertexAI.ContextBudget = config.ContextBudgetConfig{
		MaxContextTokens:    s.VertexBudget.MaxContextTokens,
		MaxWarmTokens:       s.VertexBudget.MaxWarmTokens,
		MaxToolResultTokens: s.VertexBudget.MaxToolResultTokens,
	}
	b.cfg.LLM.VertexAI.RequestTimeoutSeconds = s.VertexTimeoutSeconds
	b.cfg.UI.Theme = s.Theme
	b.cfg.Location = s.Location
	b.cfg.Memory.UseV2 = s.MemoryUseV2

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
		Network:        s.Sandbox.Network,
		CPULimit:       s.Sandbox.CPULimit,
		MemoryLimit:    s.Sandbox.MemoryLimit,
		TimeoutSeconds: s.Sandbox.TimeoutSeconds,
	}

	return b.cfg.Save()
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

// RestartSandbox tears down all sandbox containers and re-reads
// cfg.Sandbox so Settings changes (Enabled / Engine / Image / Network
// / limits) take effect without an app restart.
func (b *Bindings) RestartSandbox() {
	if b.agent != nil {
		b.agent.RestartSandbox()
	}
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
type ObjectInfo struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	MimeType  string `json:"mime_type"`
	OrigName  string `json:"orig_name"`
	CreatedAt string `json:"created_at"`
	SessionID string `json:"session_id"`
	Size      int64  `json:"size"`
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

// ObjectReferences scans all sessions and returns, for each given ID, the
// number of distinct sessions that still reference it. A reference is a
// record in the session whose ObjectIDs list contains the ID, or whose
// Content contains the textual marker "object:<ID>" (markdown image refs
// in reports).
//
// Used to warn the user before deleting an object that is still in use.
func (b *Bindings) ObjectReferences(ids []string) (map[string]int, error) {
	out := make(map[string]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = 0
		wanted[id] = struct{}{}
	}

	sessions, err := memory.ListSessions()
	if err != nil {
		return out, err
	}
	for _, si := range sessions {
		s, err := memory.LoadSession(si.ID)
		if err != nil {
			continue
		}
		seen := make(map[string]bool)
		for _, r := range s.Records {
			for _, oid := range r.ObjectIDs {
				if _, ok := wanted[oid]; ok {
					seen[oid] = true
				}
			}
			if r.Content != "" {
				for id := range wanted {
					if strings.Contains(r.Content, "object:"+id) {
						seen[id] = true
					}
				}
			}
		}
		for id := range seen {
			out[id]++
		}
	}
	return out, nil
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
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Source      string `json:"source"` // "builtin", "analysis", "shell", "mcp"
}

// GetTools returns all available tools.
func (b *Bindings) GetTools() []ToolInfo {
	items := b.agent.ListTools()
	result := make([]ToolInfo, len(items))
	for i, item := range items {
		result[i] = ToolInfo{Name: item.Name, Description: item.Description, Category: item.Category, Source: item.Source}
	}
	return result
}

// --- Pinned Memory bindings ---

// PinnedMemoryData is a pinned fact for the frontend.
type PinnedMemoryData struct {
	Fact       string `json:"fact"`
	NativeFact string `json:"native_fact"`
	Category   string `json:"category"`
}

// GetPinnedMemories returns all pinned facts.
func (b *Bindings) GetPinnedMemories() []PinnedMemoryData {
	all := b.agent.PinnedAll()
	result := make([]PinnedMemoryData, len(all))
	for i, f := range all {
		result[i] = PinnedMemoryData{
			Fact:       f.Fact,
			NativeFact: f.NativeFact,
			Category:   f.Category,
		}
	}
	return result
}

// UpdatePinnedMemory creates or updates a pinned fact.
func (b *Bindings) UpdatePinnedMemory(key, content string) error {
	return b.agent.PinnedSet(key, content)
}

// DeletePinnedMemory removes a pinned fact.
func (b *Bindings) DeletePinnedMemory(key string) error {
	return b.agent.PinnedDelete(key)
}

// DeletePinnedMemories bulk-removes pinned facts. Returns count actually deleted.
func (b *Bindings) DeletePinnedMemories(keys []string) (int, error) {
	return b.agent.PinnedDeleteByKeys(keys)
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
