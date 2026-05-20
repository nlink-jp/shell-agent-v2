package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
)

// TestAgentLoop_EmptyResponseTriggersWrapUpRetry verifies the
// one-shot retry kicks in when the LLM returns content="" with no
// tool calls — the situation where Vertex sometimes drops 0 output
// tokens after a tool result, leaving the user with no wrap-up.
func TestAgentLoop_EmptyResponseTriggersWrapUpRetry(t *testing.T) {
	mock := llm.NewMockBackend(
		llm.MockResponse{},                          // round 0: empty
		llm.MockResponse{Content: "wrap-up text"},   // round 1: retry succeeds
	)
	a := newTestAgent(t, mock)

	result, err := a.Send(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	a.postTasksWg.Wait() // drain background tasks before reading mock state

	if result.Content != "wrap-up text" {
		t.Errorf("result = %q, want wrap-up text", result.Content)
	}

	calls := mock.Calls()
	if len(calls) < 2 {
		t.Fatalf("calls = %d, want at least 2 (initial + retry)", len(calls))
	}

	// The retry call must contain the wrap-up nudge as a system message.
	var found bool
	for _, m := range calls[1].Messages {
		if m.Role == "system" && strings.Contains(m.Content, "previous response was empty") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("retry call missing wrap-up nudge system message")
	}
}

// TestAgentLoop_EmptyAfterRetryEndsCleanly verifies that when the
// retry also returns empty, the loop exits without spinning. Uses
// a timeout to fail loud if we accidentally infinite-loop.
func TestAgentLoop_EmptyAfterRetryEndsCleanly(t *testing.T) {
	mock := llm.NewMockBackend(llm.MockResponse{}) // always empty (cycles to last)
	a := newTestAgent(t, mock)

	done := make(chan struct{})
	var result SendResult
	var err error
	go func() {
		result, err = a.Send(context.Background(), "hello")
		close(done)
	}()

	select {
	case <-done:
		// Good — Send returned without hanging.
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return within 5s; empty-retry may be looping")
	}

	a.postTasksWg.Wait()

	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Content != "" {
		t.Errorf("result = %q, want empty (both attempts returned empty)", result.Content)
	}
}

// TestAgentLoop_NonEmptyResponseDoesNotTriggerRetry verifies the
// happy path is unchanged — a normal response on the first try
// does not trigger a second LLM call from the retry mechanism.
func TestAgentLoop_NonEmptyResponseDoesNotTriggerRetry(t *testing.T) {
	mock := llm.NewMockBackend(
		llm.MockResponse{Content: "Hello!"},
		llm.MockResponse{Content: "should not be called by main loop"},
	)
	a := newTestAgent(t, mock)

	result, err := a.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	a.postTasksWg.Wait()

	if result.Content != "Hello!" {
		t.Errorf("result = %q, want Hello!", result.Content)
	}

	// Inspect the first call: it must NOT contain the wrap-up nudge
	// (the retry path should never have run).
	calls := mock.Calls()
	if len(calls) < 1 {
		t.Fatal("no calls to mock")
	}
	for _, m := range calls[0].Messages {
		if m.Role == "system" && strings.Contains(m.Content, "previous response was empty") {
			t.Errorf("wrap-up nudge leaked into first call — retry should not have fired")
		}
	}
}
