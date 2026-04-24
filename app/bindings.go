package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Bindings is the thin Wails binding layer.
// All business logic is delegated to the agent and analysis packages.
type Bindings struct {
	ctx      context.Context
	agent    *agent.Agent
	cfg      *config.Config
	analysis *analysis.Engine
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

	b.agent = agent.New(cfg)
	b.agent.SetStreamHandler(func(token string, done bool) {
		wailsRuntime.EventsEmit(b.ctx, "agent:stream", map[string]any{
			"token": token,
			"done":  done,
		})
	})
}

func (b *Bindings) shutdown(_ context.Context) {
	if b.analysis != nil {
		b.analysis.Close()
	}
	if b.agent != nil {
		b.agent.Close()
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
	return b.agent.Send(b.ctx, message)
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

// LoadSession switches to an existing session.
func (b *Bindings) LoadSession(sessionID string) error {
	if b.IsBusy() {
		return fmt.Errorf("agent is busy")
	}

	session, err := memory.LoadSession(sessionID)
	if err != nil {
		return err
	}

	b.switchAnalysis(sessionID)
	return b.agent.LoadSession(session)
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

// SettingsData is the JSON-serializable settings for the frontend.
type SettingsData struct {
	DefaultBackend string `json:"default_backend"`
	LocalEndpoint  string `json:"local_endpoint"`
	LocalModel     string `json:"local_model"`
	VertexProject  string `json:"vertex_project"`
	VertexRegion   string `json:"vertex_region"`
	VertexModel    string `json:"vertex_model"`
	Theme          string `json:"theme"`
}

// GetSettings returns current settings.
func (b *Bindings) GetSettings() SettingsData {
	return SettingsData{
		DefaultBackend: string(b.cfg.LLM.DefaultBackend),
		LocalEndpoint:  b.cfg.LLM.Local.Endpoint,
		LocalModel:     b.cfg.LLM.Local.Model,
		VertexProject:  b.cfg.LLM.VertexAI.ProjectID,
		VertexRegion:   b.cfg.LLM.VertexAI.Region,
		VertexModel:    b.cfg.LLM.VertexAI.Model,
		Theme:          b.cfg.UI.Theme,
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
	return b.cfg.Save()
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
