// Package agent implements the core agent state machine and execution loop.
package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// State represents the agent's execution state.
type State string

const (
	StateIdle State = "idle"
	StateBusy State = "busy"
)

// ErrBusy is returned when a message is sent while the agent is busy.
var ErrBusy = errors.New("agent is busy")

// Agent orchestrates chat, analysis, tool execution, and memory.
type Agent struct {
	cfg    *config.Config
	state  State
	mu     sync.Mutex
	cancel context.CancelFunc
}

// New creates a new Agent with the given configuration.
func New(cfg *config.Config) *Agent {
	return &Agent{
		cfg:   cfg,
		state: StateIdle,
	}
}

// State returns the current agent state.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
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

	// TODO: implement agent loop (Phase 1)
	_ = ctx
	_ = message
	return "", nil
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
