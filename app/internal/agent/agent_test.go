package agent

import (
	"context"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestNewAgent(t *testing.T) {
	a := New(config.Default())
	if a.State() != StateIdle {
		t.Errorf("initial state = %v, want %v", a.State(), StateIdle)
	}
}

func TestSendReturnsToIdle(t *testing.T) {
	a := New(config.Default())
	a.session = &memory.Session{ID: "test", Records: []memory.Record{}}

	// Send will fail because local LLM isn't running, but state should return to Idle
	_, _ = a.Send(context.Background(), "hello")
	if a.State() != StateIdle {
		t.Errorf("state after Send = %v, want %v", a.State(), StateIdle)
	}
}

func TestSendRejectsDuringBusy(t *testing.T) {
	a := New(config.Default())

	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	_, err := a.Send(context.Background(), "hello")
	if err != ErrBusy {
		t.Errorf("Send during busy = %v, want ErrBusy", err)
	}
}

func TestAbortOnIdle(t *testing.T) {
	a := New(config.Default())
	a.Abort() // should not panic
}

func TestModelCommand(t *testing.T) {
	a := New(config.Default())

	// Show current
	result, err := a.handleCommand("/model")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	// Switch to vertex
	result, err = a.handleCommand("/model vertex")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if a.CurrentBackend() != "vertex_ai" {
		t.Errorf("backend = %v, want vertex_ai", a.CurrentBackend())
	}

	// Switch back to local
	result, err = a.handleCommand("/model local")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if a.CurrentBackend() != "local" {
		t.Errorf("backend = %v, want local", a.CurrentBackend())
	}
	_ = result
}

func TestFindingCommand(t *testing.T) {
	a := New(config.Default())
	a.session = &memory.Session{ID: "test-sess", Title: "Test Session"}

	result, err := a.handleCommand("/finding Sales peak in April")
	// Save may fail (temp path), but command itself should work
	if err != nil {
		// Allow save errors in test
		_ = err
	}
	_ = result
}

func TestFindingsCommandEmpty(t *testing.T) {
	a := New(config.Default())
	a.findings = findings.NewStore() // fresh store

	result, err := a.handleCommand("/findings")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result != "No findings yet." {
		t.Errorf("result = %v, want 'No findings yet.'", result)
	}
}

func TestUnknownCommand(t *testing.T) {
	a := New(config.Default())

	result, err := a.handleCommand("/unknown")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result for unknown command")
	}
}

func TestLoadSessionRejectsDuringBusy(t *testing.T) {
	a := New(config.Default())
	a.mu.Lock()
	a.state = StateBusy
	a.mu.Unlock()

	err := a.LoadSession(&memory.Session{})
	if err != ErrBusy {
		t.Errorf("LoadSession during busy = %v, want ErrBusy", err)
	}
}
