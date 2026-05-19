package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// newTwoProfileAgent returns an agent whose config has two
// profiles ("Default" + "Production") so /profile has somewhere
// non-trivial to switch to.
func newTwoProfileAgent(t *testing.T) (*Agent, *config.LLMProfile, *config.LLMProfile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	// The Default() profile is "Default"; clone it as "Production"
	// so we have a second profile to switch to.
	prod := config.LLMProfile{
		ID:             config.NewProfileID(),
		Name:           "Production",
		DefaultBackend: config.BackendVertexAI,
		Local:          cfg.LLM.Profiles[0].Local,
		VertexAI:       cfg.LLM.Profiles[0].VertexAI,
	}
	prod.VertexAI.ProjectID = "prod-project"
	cfg.LLM.Profiles = append(cfg.LLM.Profiles, prod)

	a := New(cfg)
	a.backend = llm.NewMockBackend()
	a.session = &memory.Session{
		ID:        "profile-cmd-test",
		Records:   []memory.Record{},
		ProfileID: cfg.LLM.DefaultProfileID,
	}
	a.findings = findings.NewStore(a.session.ID)
	return a, &cfg.LLM.Profiles[0], &cfg.LLM.Profiles[1]
}

func TestAgent_ProfileCommand_List_MarksActive(t *testing.T) {
	a, def, _ := newTwoProfileAgent(t)
	out, err := a.handleProfileCommand(nil)
	if err != nil {
		t.Fatalf("handleProfileCommand: %v", err)
	}
	// Active profile gets ● marker.
	wantActiveLine := "● " + def.Name
	if !strings.Contains(out, wantActiveLine) {
		t.Errorf("output missing active marker line %q:\n%s", wantActiveLine, out)
	}
	// Production should be ○.
	if !strings.Contains(out, "○ Production") {
		t.Errorf("output missing inactive Production line:\n%s", out)
	}
}

func TestAgent_ProfileCommand_Switch_PersistsAndRebindsBackend(t *testing.T) {
	a, _, prod := newTwoProfileAgent(t)
	prevBackend := a.backend

	out, err := a.handleProfileCommand([]string{"Production"})
	if err != nil {
		t.Fatalf("handleProfileCommand: %v", err)
	}
	if !strings.Contains(out, "Switched to profile \"Production\"") {
		t.Errorf("unexpected output: %q", out)
	}
	if a.session.ProfileID != prod.ID {
		t.Errorf("session.ProfileID = %q, want %q", a.session.ProfileID, prod.ID)
	}
	if a.activeProfileID != prod.ID {
		t.Errorf("activeProfileID = %q, want %q", a.activeProfileID, prod.ID)
	}
	// Backend must have been rebuilt — different instance from the
	// pre-switch stub.
	if a.backend == prevBackend {
		t.Error("backend not rebuilt after /profile switch")
	}
	// And persisted to session.json.
	got, exists, err := memory.LoadSessionConfig(a.session.ID)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if !exists || got.ProfileID != prod.ID {
		t.Errorf("session.json profile_id = %q (exists=%v), want %q", got.ProfileID, exists, prod.ID)
	}
}

func TestAgent_ProfileCommand_SwitchCaseInsensitive(t *testing.T) {
	a, _, prod := newTwoProfileAgent(t)
	_, err := a.handleProfileCommand([]string{"production"}) // lowercase
	if err != nil {
		t.Fatalf("handleProfileCommand: %v", err)
	}
	if a.session.ProfileID != prod.ID {
		t.Errorf("case-insensitive switch failed: got %q want %q", a.session.ProfileID, prod.ID)
	}
}

func TestAgent_ProfileCommand_SwitchSameProfile_NoOp(t *testing.T) {
	a, def, _ := newTwoProfileAgent(t)
	prevBackend := a.backend

	out, err := a.handleProfileCommand([]string{def.Name})
	if err != nil {
		t.Fatalf("handleProfileCommand: %v", err)
	}
	if !strings.Contains(out, "Already on profile") {
		t.Errorf("expected 'Already on' message, got: %q", out)
	}
	if a.backend != prevBackend {
		t.Error("backend rebuilt on no-op switch")
	}
}

func TestAgent_ProfileCommand_UnknownName(t *testing.T) {
	a, _, _ := newTwoProfileAgent(t)
	out, err := a.handleProfileCommand([]string{"NoSuchProfile"})
	if err != nil {
		t.Fatalf("handleProfileCommand: %v", err)
	}
	if !strings.Contains(out, "Unknown profile") {
		t.Errorf("expected 'Unknown profile' message, got: %q", out)
	}
}

func TestAgent_ProfileCommand_AmbiguousName(t *testing.T) {
	// Reachable only by hand-editing config.json — Settings
	// auto-disambiguates duplicates. Defensive code must still
	// surface the ambiguity rather than silently picking one.
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	cfg.LLM.Profiles = append(cfg.LLM.Profiles, config.LLMProfile{
		ID:   config.NewProfileID(),
		Name: cfg.LLM.Profiles[0].Name, // duplicate name
	})

	a := New(cfg)
	a.backend = llm.NewMockBackend()
	a.session = &memory.Session{ID: "amb-test", Records: []memory.Record{}}
	a.findings = findings.NewStore(a.session.ID)

	out, err := a.handleProfileCommand([]string{cfg.LLM.Profiles[0].Name})
	if err != nil {
		t.Fatalf("handleProfileCommand: %v", err)
	}
	if !strings.Contains(out, "ambiguous") {
		t.Errorf("expected 'ambiguous' message, got: %q", out)
	}
}

func TestAgent_ProfileCommand_DispatchThroughSlashSwitch(t *testing.T) {
	// /profile must be in the slash-command dispatch switch so the
	// chat input path actually runs handleProfileCommand rather
	// than falling through to agentLoop and confusing the LLM.
	a, _, prod := newTwoProfileAgent(t)
	res, err := a.Send(context.Background(), "/profile Production")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(res, "[CMD]") {
		t.Errorf("Send returned %q, want a [CMD] prefix (slash-command path)", res)
	}
	if a.session.ProfileID != prod.ID {
		t.Errorf("ProfileID = %q after Send, want %q", a.session.ProfileID, prod.ID)
	}
}

func TestAgent_HelpCommand_MentionsProfile(t *testing.T) {
	// Help text must advertise /profile so users discover it.
	a, _, _ := newTwoProfileAgent(t)
	out, err := a.handleHelpCommand()
	if err != nil {
		t.Fatalf("handleHelpCommand: %v", err)
	}
	if !strings.Contains(out, "/profile") {
		t.Errorf("/help does not mention /profile:\n%s", out)
	}
}
