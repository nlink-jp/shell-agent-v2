// session_rename.go — agent-level session title rename.
//
// memory.RenameSession alone is unsafe: it loads chat.json from
// disk, mutates Title, writes it back. For a session that the
// agent has already loaded into a.session, the in-memory copy
// is untouched and the agent's next a.session.Save() (after a
// user message at agent.go:1367, after a tool at :1538, in the
// agent loop at :1470, or from generateTitleIfNeeded at :2065)
// silently overwrites the disk copy with the in-memory title.
// The user's rename appears to "stick" until the next launch
// reads chat.json again — and finds the old title.
//
// generateTitleIfNeeded compounds the problem: its `if
// a.session.Title != "New Session"` guard reads the in-memory
// title, so a fresh session that the user renames before
// sending a message also gets its rename overwritten by the
// auto-title generator on first send.
//
// Both modes are fixed by routing rename through the agent so
// the in-memory title is updated under a.mu before the disk
// write. No Busy gate — rename should work even during a long
// analyze-data run; a.mu's brief hold is enough because
// the agent loop never holds it across LLM calls.

package agent

import (
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// RenameSession updates a session's title. When sessionID names
// the active session, the in-memory a.session.Title is updated
// under a.mu before the disk save so subsequent agent saves
// (and the auto-title-generation guard) observe the new value.
// For non-active sessions the agent has no in-memory copy, so
// the work degrades to memory.RenameSession's load-mutate-save.
func (a *Agent) RenameSession(sessionID, title string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session != nil && a.session.ID == sessionID {
		a.session.Title = title
		if err := a.session.Save(); err != nil {
			return err
		}
		logger.Info("session renamed: id=%s active=true", sessionID)
		return nil
	}
	if err := memory.RenameSession(sessionID, title); err != nil {
		return err
	}
	logger.Info("session renamed: id=%s active=false", sessionID)
	return nil
}
