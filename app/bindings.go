package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"

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
	b.agent.SetActivityHandler(func(actType, detail string) {
		wailsRuntime.EventsEmit(b.ctx, "agent:activity", map[string]any{
			"type":   actType,
			"detail": detail,
		})
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
		case "summary", "tool":
			continue
		case "assistant":
			if strings.HasPrefix(r.Content, "[Calling:") {
				continue
			}
			msgs = append(msgs, MessageData{
				Role: r.Role, Content: r.Content,
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

// SettingsData is the JSON-serializable settings for the frontend.
type SettingsData struct {
	DefaultBackend string            `json:"default_backend"`
	LocalEndpoint  string            `json:"local_endpoint"`
	LocalModel     string            `json:"local_model"`
	VertexProject  string            `json:"vertex_project"`
	VertexRegion   string            `json:"vertex_region"`
	VertexModel    string            `json:"vertex_model"`
	Theme          string            `json:"theme"`
	Location       string            `json:"location"`
	MCPProfiles    []MCPProfileData  `json:"mcp_profiles"`
}

// GetSettings returns current settings.
func (b *Bindings) GetSettings() SettingsData {
	profiles := make([]MCPProfileData, len(b.cfg.Tools.MCPProfiles))
	for i, p := range b.cfg.Tools.MCPProfiles {
		profiles[i] = MCPProfileData{Name: p.Name, Binary: p.Binary, ProfilePath: p.ProfilePath, Enabled: p.Enabled}
	}
	return SettingsData{
		DefaultBackend: string(b.cfg.LLM.DefaultBackend),
		LocalEndpoint:  b.cfg.LLM.Local.Endpoint,
		LocalModel:     b.cfg.LLM.Local.Model,
		VertexProject:  b.cfg.LLM.VertexAI.ProjectID,
		VertexRegion:   b.cfg.LLM.VertexAI.Region,
		VertexModel:    b.cfg.LLM.VertexAI.Model,
		Theme:          b.cfg.UI.Theme,
		Location:       b.cfg.Location,
		MCPProfiles:    profiles,
	}
}

// SaveSettings persists updated settings.
func (b *Bindings) SaveSettings(s SettingsData) error {
	b.cfg.LLM.DefaultBackend = config.LLMBackend(s.DefaultBackend)
	b.cfg.LLM.Local.Endpoint = s.LocalEndpoint
	b.cfg.LLM.Local.Model = s.LocalModel
	b.cfg.LLM.VertexAI.ProjectID = s.VertexProject
	b.cfg.LLM.VertexAI.Region = s.VertexRegion
	b.cfg.LLM.VertexAI.Model = s.VertexModel
	b.cfg.UI.Theme = s.Theme
	b.cfg.Location = s.Location

	// Update MCP profiles
	profiles := make([]config.MCPProfileConfig, len(s.MCPProfiles))
	for i, p := range s.MCPProfiles {
		profiles[i] = config.MCPProfileConfig{Name: p.Name, Binary: p.Binary, ProfilePath: p.ProfilePath, Enabled: p.Enabled}
	}
	b.cfg.Tools.MCPProfiles = profiles

	return b.cfg.Save()
}

// RestartMCP restarts all MCP guardian processes from current config.
func (b *Bindings) RestartMCP() {
	if b.agent != nil {
		b.agent.RestartGuardians()
	}
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
	b.agent.SetAnalysis(b.analysis)
}

func nowUnixMilli() int64 {
	return time.Now().UnixMilli()
}
