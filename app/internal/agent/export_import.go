// export_import.go — agent-level wrappers around internal/sessionio.
//
// The state-machine integration lives here so that agent.ExportSession
// and agent.ImportSession behave identically to other busy operations
// (Send / LoadSession): they wait for in-flight post-tasks to drain,
// reject overlapping calls with ErrBusy, and release state on return.
//
// Active-session export additionally flushes the per-session stores
// (chat.json / session_memory.json / findings.json) and closes the
// analysis Engine so the on-disk DuckDB file is consistent for the
// bundle copy. The Engine is left closed; the binding layer
// re-creates it via switchAnalysis after this returns. See
// docs/en/adr/0001-session-import-export.md §4.2.

package agent

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sessionio"
)

// ExportSession packages the named session into a .shellagent
// bundle at destPath.
//
// The agent state is held Busy for the duration so concurrent
// Send / Load / Export / Import calls return ErrBusy. For the
// active session the analysis Engine is closed before the copy;
// the caller (typically the binding layer) is responsible for
// re-opening it after this returns successfully.
//
// appVersion is stamped into the bundle manifest (informational —
// import compatibility is gated by SchemaVersion alone).
//
// Returns the bundle byte size and the count of objstore objects
// included, both useful for audit logging at the binding layer.
func (a *Agent) ExportSession(sessionID, destPath, appVersion string) (size int64, objectCount int, err error) {
	// Drain in-flight post-response tasks (memory extraction, title
	// generation, summarisation) before transitioning state. Mirrors
	// LoadSession's pattern (agent.go:778). Without this, a background
	// goroutine could still be writing to a per-session store while
	// we copy it.
	a.postTasksWg.Wait()

	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return 0, 0, ErrBusy
	}
	a.state = StateBusy
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.state = StateIdle
		a.mu.Unlock()
	}()

	// Read the session metadata from disk so we don't depend on the
	// in-memory copy (which only exists when sessionID is active).
	diskSession, err := memory.LoadSession(sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("export: load session %s: %w", sessionID, err)
	}

	isActive := a.session != nil && a.session.ID == sessionID
	if isActive {
		// Flush in-memory state to disk. These should already be
		// current (every mutator calls Save) but flushing is
		// idempotent and cheap. If a flush fails we still continue;
		// the on-disk copy remains valid even if newer in-memory
		// changes are lost.
		if a.session != nil {
			_ = a.session.Save()
		}
		if a.sessionMemory != nil {
			_ = a.sessionMemory.Save()
		}
		if a.findings != nil {
			_ = a.findings.Save()
		}
		// Close the analysis Engine so its on-disk DuckDB file is
		// flushed and unlocked for the binary copy. The engine field
		// stays non-nil but its underlying *sql.DB is nil; callers
		// that re-enter the engine after this point will need to
		// have it re-created (the binding layer does this via
		// switchAnalysis on return).
		if a.analysis != nil {
			_ = a.analysis.Close()
		}
	}

	// Gather every object the session owns. Snapshot is taken under
	// objstore's RLock; concurrent Store/Delete cannot mutate this
	// slice mid-iteration.
	objects := a.collectObjectsForExport(sessionID)

	manifest := &sessionio.Manifest{
		SchemaVersion:        sessionio.SchemaVersion,
		ExportedAt:           time.Now().UTC(),
		ExportedByAppVersion: appVersion,
		Session: sessionio.SessionMeta{
			OriginalID: diskSession.ID,
			Title:      diskSession.Title,
			Private:    diskSession.Private,
		},
	}

	srcDir := memory.SessionDir(sessionID)
	size, err = sessionio.ExportSession(srcDir, destPath, manifest, objects)
	if err != nil {
		return 0, 0, fmt.Errorf("export: write bundle: %w", err)
	}

	logger.Info("session exported: id=%s private=%v bytes=%d objects=%d dest=%s",
		sessionID, diskSession.Private, size, len(objects), destPath)
	return size, len(objects), nil
}

// collectObjectsForExport snapshots every object the session owns
// in the global objstore and wraps each in an ObjectExport whose
// Open lazily streams the blob via objstore.ReadData. Returns nil
// when the agent has no objstore wired up (test paths).
func (a *Agent) collectObjectsForExport(sessionID string) []sessionio.ObjectExport {
	if a.objects == nil {
		return nil
	}
	metas := a.objects.ListBySession(sessionID)
	if len(metas) == 0 {
		return nil
	}
	out := make([]sessionio.ObjectExport, 0, len(metas))
	store := a.objects
	for _, m := range metas {
		m := m
		out = append(out, sessionio.ObjectExport{
			Meta: m,
			Open: func() (io.ReadCloser, error) { return store.ReadData(m.ID) },
		})
	}
	return out
}

// ImportSession extracts a .shellagent bundle into a fresh session
// directory, registers any bundled objstore objects under new IDs,
// rewrites references, and returns the new session's ID.
//
// The state machine is held Busy throughout. The new session is
// NOT auto-loaded by this method — callers (the binding layer)
// invoke LoadSession on the returned ID so the binding's analysis
// Engine setup runs in the right order.
func (a *Agent) ImportSession(srcPath string) (newSessionID string, private bool, bytes int64, objectCount int, err error) {
	a.postTasksWg.Wait()

	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return "", false, 0, 0, ErrBusy
	}
	a.state = StateBusy
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.state = StateIdle
		a.mu.Unlock()
	}()

	// Title-collision suffixing reads the live session list. Doing
	// it inside the busy window is enough for the single-user GUI;
	// no titles can be added concurrently.
	infos, err := memory.ListSessions()
	if err != nil {
		return "", false, 0, 0, fmt.Errorf("import: list sessions: %w", err)
	}
	titles := make([]string, 0, len(infos))
	for _, info := range infos {
		titles = append(titles, info.Title)
	}

	if a.objects == nil {
		// objstore must be available to register bundled blobs;
		// otherwise the imported session would have dangling refs.
		return "", false, 0, 0, fmt.Errorf("import: objstore not initialised")
	}

	res, err := sessionio.ImportSession(srcPath, sessionsBaseDir(), &agentObjstoreAdapter{store: a.objects}, titles)
	if err != nil {
		return "", false, 0, 0, err
	}

	logger.Info("session imported: original_id=%s new_id=%s private=%v bytes=%d objects=%d",
		res.Manifest.Session.OriginalID, res.NewSessionID, res.Manifest.Session.Private, res.Bytes, res.ObjectCount)
	return res.NewSessionID, res.Manifest.Session.Private, res.Bytes, res.ObjectCount, nil
}

// sessionsBaseDir returns the directory that holds all per-session
// subdirectories. Derived from a known session's path so we don't
// depend on config.DataDir() directly.
func sessionsBaseDir() string {
	// memory.SessionDir("x") returns "<DataDir>/sessions/x".
	// filepath.Dir gives the parent, platform-portable.
	return filepath.Dir(memory.SessionDir("x"))
}

// agentObjstoreAdapter satisfies sessionio.ObjstoreWriter using the
// agent's *objstore.Store. The interface exists so unit tests can
// fake the store; this adapter is the single non-test implementor.
type agentObjstoreAdapter struct {
	store *objstore.Store
}

func (a *agentObjstoreAdapter) Store(reader io.Reader, t objstore.ObjectType, mime, origName, sessionID string) (*objstore.ObjectMeta, error) {
	return a.store.Store(reader, t, mime, origName, sessionID)
}

func (a *agentObjstoreAdapter) Delete(id string) error {
	return a.store.Delete(id)
}
