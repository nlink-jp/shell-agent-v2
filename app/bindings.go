package main

import (
	"context"

	"github.com/nlink-jp/shell-agent-v2/internal/agent"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Bindings is the thin Wails binding layer.
// All business logic is delegated to the agent package.
type Bindings struct {
	ctx   context.Context
	agent *agent.Agent
	cfg   *config.Config
}

// NewBindings creates a new Bindings instance.
func NewBindings() *Bindings {
	return &Bindings{}
}

func (b *Bindings) startup(ctx context.Context) {
	b.ctx = ctx

	cfg, err := config.Load()
	if err != nil {
		println("Warning: using default config:", err.Error())
		cfg = config.Default()
	}
	b.cfg = cfg

	b.agent = agent.New(cfg)
}

func (b *Bindings) shutdown(_ context.Context) {
	if b.agent != nil {
		b.agent.Close()
	}
}

// IsBusy reports whether the agent is currently processing.
func (b *Bindings) IsBusy() bool {
	if b.agent == nil {
		return false
	}
	return b.agent.State() == agent.StateBusy
}

// Send sends a user message to the agent.
// Returns an error if the agent is busy.
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

// Version returns the application version.
func (b *Bindings) Version() string {
	return version
}
