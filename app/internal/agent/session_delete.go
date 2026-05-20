// session_delete.go — agent-level session deletion with state-
// machine integration.
//
// Mirrors ExportSession / ImportSession (export_import.go) and
// LoadSession / SendWithImages: drains in-flight post-tasks,
// gates on Idle, holds Busy for the duration so concurrent
// Send / Load / Export / Import calls return ErrBusy. Deleting
// the active session nil-clears the per-session pointers and
// closes the analysis Engine before the directory is removed
// so a stray Save / Engine call can't fight a half-deleted
// session dir.
//
// Design: docs/en/adr/0003-session-delete-ux.md.

package agent

import (
	"context"

	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// DeleteSession removes a session's per-session files
// (sessions/<id>/*), its objstore objects (snapshot via
// objstore.DeleteBySession), and its sandbox container.
//
// Held under the agent state-machine slot for the duration so
// concurrent Send / Load / Export / Import calls return
// ErrBusy. If sessionID names the active session, the agent's
// per-session pointers (session, sessionMemory, findings) are
// nil-cleared and the analysis Engine is closed before
// RemoveAll runs; the binding layer is responsible for
// switching to a different session (or creating a fresh one)
// after this returns.
//
// Returns the error from memory.DeleteSessionDir; the others
// (objstore, sandbox) are best-effort, mirroring the previous
// bindings.DeleteSession behaviour.
func (a *Agent) DeleteSession(ctx context.Context, sessionID string) error {
	a.postTasksWg.Wait()

	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return ErrBusy
	}
	// ADR-0021 §2.4: defensive reset of FSM fields after Wait
	// returns. Audit V7: lifecycle paths previously relied on
	// extraction's normal cleanup having cleared the flags, but
	// a panicking extraction (audit V4) could leave them stranded.
	// This is a no-op on the happy path; load-bearing on the
	// recovery path.
	a.resetStateMachine()
	a.state = StateBusy
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.state = StateIdle
		a.mu.Unlock()
	}()

	isActive := a.session != nil && a.session.ID == sessionID
	if isActive {
		// Close the DuckDB engine before the dir disappears so
		// RemoveAll doesn't fight an open *sql.DB. The binding
		// layer's switchAnalysis (called when the user lands on
		// the next session) will allocate a fresh engine.
		if a.analysis != nil {
			_ = a.analysis.Close()
		}
		// Drop pointers so any code path that somehow re-enters
		// after this method (none should — state is Busy) finds
		// nil and fails safely instead of resurrecting the dir
		// via a trailing Save.
		a.session = nil
		a.sessionMemory = nil
		a.findings = nil
	}

	if a.objects != nil {
		_ = a.objects.DeleteBySession(sessionID)
	}
	// SandboxStop is a no-op when the sandbox is disabled or no
	// container exists for this session, so calling it
	// unconditionally is safe.
	_ = a.SandboxStop(ctx, sessionID)

	if err := memory.DeleteSessionDir(sessionID); err != nil {
		return err
	}
	logger.Info("session deleted: id=%s active=%v", sessionID, isActive)
	return nil
}
