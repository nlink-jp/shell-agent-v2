package agent

import (
	"context"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestRenameActiveSession_SurvivesSubsequentSave is the Mode A
// regression. Pre-fix:
//   1. user renames active session
//   2. memory.RenameSession writes the new title to chat.json
//   3. agent's in-memory a.session.Title still holds the OLD value
//   4. any subsequent a.session.Save() (after a Send, after a
//      tool, etc.) silently overwrites the disk copy with the
//      stale in-memory title
//   5. on next launch the user sees the original title
//
// The fix updates a.session.Title in-memory under a.mu before
// the disk write, so subsequent saves observe the new value.
// We verify by simulating step (4) directly: append a record
// and call a.session.Save(), then reload chat.json and confirm
// the rename stuck.
func TestRenameActiveSession_SurvivesSubsequentSave(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-mode-a", "Original Title", false)

	if err := a.RenameSession("sess-mode-a", "Renamed Title"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if a.session.Title != "Renamed Title" {
		t.Errorf("in-memory Title = %q, want %q", a.session.Title, "Renamed Title")
	}

	// Simulate any of the post-action a.session.Save() call sites
	// (agent.go:1367 / :1470 / :1538). Pre-fix this would write
	// the stale "Original Title" back to disk.
	a.session.Records = append(a.session.Records, memory.Record{
		Role: "user", Content: "follow-up message",
	})
	if err := a.session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}

	got, err := memory.LoadSession("sess-mode-a")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got.Title != "Renamed Title" {
		t.Errorf("on-disk Title after subsequent Save = %q, want %q", got.Title, "Renamed Title")
	}
}

// TestRenameActiveSession_GuardsAutoTitleGen is the Mode B
// regression. Pre-fix:
//   1. fresh session has Title="New Session"
//   2. user renames to something meaningful
//   3. memory.RenameSession writes new title to disk; in-memory
//      a.session.Title still says "New Session"
//   4. user sends first message; generateTitleIfNeeded checks
//      a.session.Title != "New Session" — false (in-memory is
//      stale) — proceeds, asks LLM for an auto-title, writes
//      that to disk
//   5. user-set title obliterated
//
// The fix updates a.session.Title in-memory, so the guard
// observes the new value and returns early. We verify the
// guard observation directly without firing the LLM call.
func TestRenameActiveSession_GuardsAutoTitleGen(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-mode-b", "New Session", false)
	// Add a user message so generateTitleIfNeeded would
	// otherwise have something to title from.
	a.session.Records = []memory.Record{
		{Role: "user", Content: "investigate the spike"},
	}

	if err := a.RenameSession("sess-mode-b", "私の調査"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if a.session.Title != "私の調査" {
		t.Fatalf("in-memory Title = %q, want %q", a.session.Title, "私の調査")
	}

	// Pre-fix: in-memory Title is still "New Session" so this
	// would proceed past the guard and call a.backend (nil in
	// the test fixture, panicking — which is itself a useful
	// "did the guard fire?" signal). Post-fix: returns nil
	// immediately because Title is now "私の調査".
	if err := a.generateTitleIfNeeded(context.Background()); err != nil {
		t.Errorf("generateTitleIfNeeded should return nil when title was renamed; got %v", err)
	}

	// Disk title must still be the user's choice, not regen'd.
	got, err := memory.LoadSession("sess-mode-b")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got.Title != "私の調査" {
		t.Errorf("on-disk Title = %q, want %q", got.Title, "私の調査")
	}
}

// TestRenameNonActiveSession_StillWorks ensures the
// pass-through to memory.RenameSession is intact for sessions
// the agent has not loaded — this is the no-regression direction.
func TestRenameNonActiveSession_StillWorks(t *testing.T) {
	a := newTestAgentWithSession(t, "sess-active", "Active", false)

	// Persist a SECOND session that the agent never loads.
	other := &memory.Session{
		ID:    "sess-other",
		Title: "Other Original",
		Records: []memory.Record{
			{Role: "user", Content: "elsewhere"},
		},
	}
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	if err := a.RenameSession("sess-other", "Other Renamed"); err != nil {
		t.Fatalf("RenameSession non-active: %v", err)
	}

	got, err := memory.LoadSession("sess-other")
	if err != nil {
		t.Fatalf("LoadSession sess-other: %v", err)
	}
	if got.Title != "Other Renamed" {
		t.Errorf("non-active Title = %q, want %q", got.Title, "Other Renamed")
	}
	// Active session must be undisturbed by a rename targeting
	// a different session ID.
	if a.session.Title != "Active" {
		t.Errorf("active session Title disturbed by non-active rename: now %q", a.session.Title)
	}
}
