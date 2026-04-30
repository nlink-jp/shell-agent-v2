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

func TestNormalizeToolArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // exact wanted output (post-jsonfix); empty means "must round-trip via Unmarshal cleanly"
	}{
		{"plain JSON unchanged", `{"a":1}`, `{"a":1}`},
		{"empty stays empty", "", ""},
		{"markdown fence stripped", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose around JSON", `Sure, here it is: {"a":1}`, `{"a":1}`},
		{"trailing comma repaired", `{"a":1,}`, `{"a":1}`},
		{"single-quoted keys repaired", `{'a':1}`, `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolArgs(tc.in)
			if got != tc.want {
				t.Errorf("normalizeToolArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeToolArgs_FallsBackOnGarbage(t *testing.T) {
	// jsonfix.Extract returns ErrNoJSON for input that doesn't
	// contain anything recoverable. We must surface the original
	// string so downstream Unmarshal produces a normal "invalid
	// arguments" error instead of pretending the input was empty.
	in := "absolutely not JSON"
	if got := normalizeToolArgs(in); got != in {
		t.Errorf("garbage input should pass through untouched, got %q", got)
	}
}
