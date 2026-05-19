package main

import (
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// --- ListProfiles / GetProfile -------------------------------------

func TestBindings_ListProfiles_DefaultFirst(t *testing.T) {
	b, _ := newTestBindings(t)
	// Add a Production profile so the sort actually has work to do.
	if _, err := b.CreateProfile(CreateProfileRequest{Name: "Production"}); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	got := b.ListProfiles()
	if len(got) < 2 {
		t.Fatalf("expected ≥2 profiles, got %d", len(got))
	}
	if !got[0].IsDefault {
		t.Errorf("ListProfiles[0].IsDefault = false, want true (default first)")
	}
}

func TestBindings_GetProfile_KnownAndUnknown(t *testing.T) {
	b, _ := newTestBindings(t)
	id := b.cfg.LLM.DefaultProfileID

	d, err := b.GetProfile(id)
	if err != nil {
		t.Fatalf("GetProfile(known): %v", err)
	}
	if d.ID != id {
		t.Errorf("ID = %q, want %q", d.ID, id)
	}
	if !d.IsDefault {
		t.Error("IsDefault = false for the actual default profile")
	}

	if _, err := b.GetProfile("no-such-id"); err == nil {
		t.Error("GetProfile(unknown) returned no error")
	}
}

// --- CreateProfile --------------------------------------------------

func TestBindings_CreateProfile_AutoDisambiguatesName(t *testing.T) {
	b, _ := newTestBindings(t)
	// The Default profile is named "Default"; ask for another one
	// also called "Default" and expect "Default (2)".
	res, err := b.CreateProfile(CreateProfileRequest{Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if !res.NameAdjusted {
		t.Error("NameAdjusted = false on duplicate-name create")
	}
	if res.Profile.Name != "Default (2)" {
		t.Errorf("Profile.Name = %q, want %q", res.Profile.Name, "Default (2)")
	}
	if res.OriginalName != "Default" {
		t.Errorf("OriginalName = %q, want %q", res.OriginalName, "Default")
	}
}

func TestBindings_CreateProfile_Clone_CopiesSourceFields(t *testing.T) {
	b, _ := newTestBindings(t)
	srcID := b.cfg.LLM.DefaultProfileID
	// Customise the source so cloning is observable.
	src := b.cfg.LLM.DefaultProfile()
	src.VertexAI.ProjectID = "src-project"
	src.Local.Endpoint = "http://src:1234"
	if err := b.cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
	}

	res, err := b.CreateProfile(CreateProfileRequest{Name: "Clone", CloneFromID: srcID})
	if err != nil {
		t.Fatalf("CreateProfile clone: %v", err)
	}
	cloned, err := b.GetProfile(res.Profile.ID)
	if err != nil {
		t.Fatalf("GetProfile clone: %v", err)
	}
	if cloned.Vertex.ProjectID != "src-project" {
		t.Errorf("clone.Vertex.ProjectID = %q, want %q", cloned.Vertex.ProjectID, "src-project")
	}
	if cloned.Local.Endpoint != "http://src:1234" {
		t.Errorf("clone.Local.Endpoint = %q, want %q", cloned.Local.Endpoint, "http://src:1234")
	}
}

// --- UpdateProfile --------------------------------------------------

func TestBindings_UpdateProfile_FullRoundTrip(t *testing.T) {
	b, _ := newTestBindings(t)
	id := b.cfg.LLM.DefaultProfileID

	req := UpdateProfileRequest{
		Name:           "Renamed",
		DefaultBackend: string(config.BackendVertexAI),
		Local: LocalProfileFields{
			Endpoint:              "http://new:9999",
			Model:                 "new-local",
			APIKeyEnv:             "NEW_KEY",
			ContextBudget:         BackendBudgetData{MaxContextTokens: 12345},
			RequestTimeoutSeconds: 99,
			RetryMaxAttempts:      5,
		},
		Vertex: VertexProfileFields{
			ProjectID:             "new-proj",
			Region:                "asia-northeast1",
			Model:                 "new-flash",
			ContextBudget:         BackendBudgetData{MaxContextTokens: 99999},
			RequestTimeoutSeconds: 60,
			RetryMaxAttempts:      4,
		},
	}
	if _, err := b.UpdateProfile(id, req); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	got, err := b.GetProfile(id)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if got.Name != "Renamed" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.DefaultBackend != string(config.BackendVertexAI) {
		t.Errorf("DefaultBackend = %q", got.DefaultBackend)
	}
	if got.Local.Endpoint != "http://new:9999" {
		t.Errorf("Local.Endpoint = %q", got.Local.Endpoint)
	}
	if got.Vertex.ProjectID != "new-proj" {
		t.Errorf("Vertex.ProjectID = %q", got.Vertex.ProjectID)
	}
}

func TestBindings_UpdateProfile_AutoDisambiguatesNewName(t *testing.T) {
	b, _ := newTestBindings(t)
	// Create a second profile to collide with on rename.
	if _, err := b.CreateProfile(CreateProfileRequest{Name: "Production"}); err != nil {
		t.Fatalf("seed CreateProfile: %v", err)
	}
	// Now try to rename the default profile to "Production" — should
	// auto-suffix.
	res, err := b.UpdateProfile(b.cfg.LLM.DefaultProfileID, UpdateProfileRequest{Name: "Production"})
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if !res.NameAdjusted {
		t.Error("NameAdjusted = false on rename-to-existing-name")
	}
	if res.Profile.Name != "Production (2)" {
		t.Errorf("Profile.Name = %q, want %q", res.Profile.Name, "Production (2)")
	}
}

// --- DeleteProfile / SetDefaultProfile -----------------------------

func TestBindings_DeleteProfile_RefusesDefault(t *testing.T) {
	b, _ := newTestBindings(t)
	_, err := b.DeleteProfile(b.cfg.LLM.DefaultProfileID)
	if err == nil {
		t.Fatal("DeleteProfile(default) returned no error")
	}
	if !strings.Contains(err.Error(), "default profile") {
		t.Errorf("error message %q does not mention default profile", err)
	}
}

func TestBindings_DeleteProfile_NonDefault(t *testing.T) {
	b, _ := newTestBindings(t)
	res, err := b.CreateProfile(CreateProfileRequest{Name: "Throwaway"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	delRes, err := b.DeleteProfile(res.Profile.ID)
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if delRes.DeletedID != res.Profile.ID {
		t.Errorf("DeletedID = %q, want %q", delRes.DeletedID, res.Profile.ID)
	}
	// Verify it's gone from the list.
	for _, p := range b.ListProfiles() {
		if p.ID == res.Profile.ID {
			t.Error("profile still present after DeleteProfile")
		}
	}
}

func TestBindings_SetDefaultProfile(t *testing.T) {
	b, _ := newTestBindings(t)
	created, err := b.CreateProfile(CreateProfileRequest{Name: "NewDefault"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := b.SetDefaultProfile(created.Profile.ID); err != nil {
		t.Fatalf("SetDefaultProfile: %v", err)
	}
	if b.cfg.LLM.DefaultProfileID != created.Profile.ID {
		t.Errorf("DefaultProfileID = %q, want %q", b.cfg.LLM.DefaultProfileID, created.Profile.ID)
	}
}

func TestBindings_SetDefaultProfile_UnknownID(t *testing.T) {
	b, _ := newTestBindings(t)
	if err := b.SetDefaultProfile("no-such-id"); err == nil {
		t.Error("SetDefaultProfile(unknown) returned no error")
	}
}

// --- SwitchSessionProfile / SwitchSessionBackend -------------------

func TestBindings_SwitchSessionProfile(t *testing.T) {
	b, _ := newTestBindings(t)
	// Create a second profile.
	created, err := b.CreateProfile(CreateProfileRequest{Name: "Production"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	// Construct a session bound to the default profile.
	defaultID := b.cfg.LLM.DefaultProfileID
	session := &memory.Session{ID: "switch-test", ProfileID: defaultID, Records: []memory.Record{}}
	if err := session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	if err := memory.SaveSessionConfig(session.ID, memory.SessionConfig{ProfileID: defaultID}); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}
	if err := b.agent.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	if err := b.SwitchSessionProfile(created.Profile.ID); err != nil {
		t.Fatalf("SwitchSessionProfile: %v", err)
	}
	got := b.CurrentSessionProfile()
	if got.ID != created.Profile.ID {
		t.Errorf("CurrentSessionProfile.ID = %q, want %q", got.ID, created.Profile.ID)
	}
}

func TestBindings_SwitchSessionBackend(t *testing.T) {
	b, _ := newTestBindings(t)
	session := &memory.Session{ID: "be-switch", ProfileID: b.cfg.LLM.DefaultProfileID, Records: []memory.Record{}}
	if err := session.Save(); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	if err := b.agent.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if err := b.SwitchSessionBackend(string(config.BackendVertexAI)); err != nil {
		t.Fatalf("SwitchSessionBackend: %v", err)
	}
	if got := b.CurrentSessionBackend(); got != string(config.BackendVertexAI) {
		t.Errorf("CurrentSessionBackend = %q, want %q", got, config.BackendVertexAI)
	}
}
