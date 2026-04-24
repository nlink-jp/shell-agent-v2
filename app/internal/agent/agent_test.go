package agent

import (
	"context"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

func TestNewAgent(t *testing.T) {
	a := New(config.Default())
	if a.State() != StateIdle {
		t.Errorf("initial state = %v, want %v", a.State(), StateIdle)
	}
}

func TestSendTransitionsToBusy(t *testing.T) {
	a := New(config.Default())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Send(context.Background(), "hello")
	}()

	<-done
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

func TestAbort(t *testing.T) {
	a := New(config.Default())
	// Abort on idle should not panic
	a.Abort()
}
