package agent

import (
	"os"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestAgent_LoadSession_NoSessionJSON_CreatesDefault — a v0.11.x
// session (no session.json on disk) loaded under v0.12.0 must lazy-
// write session.json pointing at the current default profile.
// ADR-0016 §3.3 step 2a.
func TestAgent_LoadSession_NoSessionJSON_CreatesDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.Default()
	defaultID := cfg.LLM.DefaultProfileID

	a := New(cfg)
	a.backend = llm.NewMockBackend()

	session := &memory.Session{ID: "legacy-session", Records: []memory.Record{}}
	a.findings = findings.NewStore(session.ID)
	if err := session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	// Sanity: no session.json yet.
	if _, exists, _ := memory.LoadSessionConfig(session.ID); exists {
		t.Fatal("precondition failed: session.json already exists")
	}

	if err := a.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	// Lazy migration must have written session.json.
	cfgFromDisk, exists, err := memory.LoadSessionConfig(session.ID)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if !exists {
		t.Fatal("session.json was not lazy-written on LoadSession")
	}
	if cfgFromDisk.ProfileID != defaultID {
		t.Errorf("session.json profile_id = %q, want default %q", cfgFromDisk.ProfileID, defaultID)
	}
	if a.session.ProfileID != defaultID {
		t.Errorf("a.session.ProfileID = %q, want %q", a.session.ProfileID, defaultID)
	}
}

// TestAgent_LoadSession_DeletedProfile_FallsBackToDefault — a session
// whose recorded profile_id no longer exists in config must fall back
// to the default profile and rewrite session.json. ADR-0016 §3.3
// step 3b.
func TestAgent_LoadSession_DeletedProfile_FallsBackToDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.Default()
	defaultID := cfg.LLM.DefaultProfileID

	a := New(cfg)
	a.backend = llm.NewMockBackend()

	const danglingID = "deleted-profile-uuid-no-such-thing"
	session := &memory.Session{
		ID:        "dangling-session",
		Records:   []memory.Record{},
		ProfileID: danglingID,
	}
	a.findings = findings.NewStore(session.ID)
	if err := session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	if err := memory.SaveSessionConfig(session.ID, memory.SessionConfig{ProfileID: danglingID}); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}

	if err := a.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	// Fallback must have rewritten session.json to the default.
	cfgFromDisk, _, err := memory.LoadSessionConfig(session.ID)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if cfgFromDisk.ProfileID != defaultID {
		t.Errorf("session.json profile_id = %q after fallback, want default %q", cfgFromDisk.ProfileID, defaultID)
	}
	if a.session.ProfileID != defaultID {
		t.Errorf("a.session.ProfileID = %q after fallback, want %q", a.session.ProfileID, defaultID)
	}
}

// TestAgent_LoadSession_KnownProfile_NoRewrite — a session pointing
// at an existing profile must NOT trigger session.json rewrites or
// backend rebuilds.
func TestAgent_LoadSession_KnownProfile_NoRewrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.Default()
	defaultID := cfg.LLM.DefaultProfileID

	a := New(cfg)
	mock := llm.NewMockBackend()
	a.backend = mock

	session := &memory.Session{
		ID:        "happy-path",
		Records:   []memory.Record{},
		ProfileID: defaultID,
	}
	a.findings = findings.NewStore(session.ID)
	if err := session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	if err := memory.SaveSessionConfig(session.ID, memory.SessionConfig{ProfileID: defaultID}); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}

	if err := a.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	// Stub must still be in place (not overwritten by setBackend).
	if a.backend != mock {
		t.Error("a.backend was rebuilt on LoadSession of the same profile")
	}
}

// TestAgent_CurrentProfile_FallsBackToDefault — when no session is
// loaded or the session has no recorded profile, currentProfile()
// must return the default profile.
func TestAgent_CurrentProfile_FallsBackToDefault(t *testing.T) {
	cfg := config.Default()
	defaultID := cfg.LLM.DefaultProfileID

	a := New(cfg)
	a.backend = llm.NewMockBackend()

	// No session loaded.
	if got := a.currentProfile(); got == nil || got.ID != defaultID {
		t.Errorf("currentProfile (no session) = %v, want default %q", got, defaultID)
	}

	// Session with empty ProfileID (v0.11.x stub).
	a.session = &memory.Session{ID: "x", Records: []memory.Record{}}
	if got := a.currentProfile(); got == nil || got.ID != defaultID {
		t.Errorf("currentProfile (empty ProfileID) = %v, want default %q", got, defaultID)
	}

	// Session with unknown ProfileID.
	a.session.ProfileID = "no-such-profile"
	if got := a.currentProfile(); got == nil || got.ID != defaultID {
		t.Errorf("currentProfile (unknown ProfileID) = %v, want default %q", got, defaultID)
	}
}

// TestAgent_LoadSession_MalformedSessionJSON_FallsBack — corrupted
// session.json must not crash LoadSession. The session loads with
// the default profile and session.json gets rewritten cleanly.
func TestAgent_LoadSession_MalformedSessionJSON_FallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.Default()
	defaultID := cfg.LLM.DefaultProfileID

	a := New(cfg)
	a.backend = llm.NewMockBackend()

	id := "corrupt-session"
	session := &memory.Session{ID: id, Records: []memory.Record{}}
	a.findings = findings.NewStore(id)
	if err := session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	// Write a malformed session.json.
	if err := os.MkdirAll(memory.SessionDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory.SessionConfigPath(id), []byte(`{ not json`), 0600); err != nil {
		t.Fatal(err)
	}

	// LoadSession reads chat.json; the malformed session.json is
	// silently ignored (ProfileID left empty), and the agent's
	// LoadSession then lazy-rewrites it to the default.
	loadedSession, err := memory.LoadSession(id)
	if err != nil {
		t.Fatalf("memory.LoadSession: %v", err)
	}
	if err := a.LoadSession(loadedSession); err != nil {
		t.Fatalf("agent.LoadSession: %v", err)
	}

	// Verify session.json was rewritten cleanly.
	cfgFromDisk, exists, err := memory.LoadSessionConfig(id)
	if err != nil {
		t.Fatalf("LoadSessionConfig after recovery: %v", err)
	}
	if !exists {
		t.Fatal("session.json missing after recovery write")
	}
	if cfgFromDisk.ProfileID != defaultID {
		t.Errorf("recovered profile_id = %q, want default %q", cfgFromDisk.ProfileID, defaultID)
	}
}
