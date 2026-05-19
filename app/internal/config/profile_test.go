package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// --- Migration (UnmarshalJSON old shape → new shape) ----------------

func TestMigrate_V011LegacyShape_CreatesDefaultProfile(t *testing.T) {
	legacy := `{
		"default_backend": "vertex_ai",
		"local": {"endpoint": "http://legacy:1234", "model": "old-local"},
		"vertex_ai": {"project_id": "legacy-proj", "model": "legacy-flash"}
	}`
	var llm LLMConfig
	if err := json.Unmarshal([]byte(legacy), &llm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(llm.Profiles) != 1 {
		t.Fatalf("got %d profiles, want 1", len(llm.Profiles))
	}
	p := llm.Profiles[0]
	if p.Name != DefaultProfileName {
		t.Errorf("Name = %q, want %q", p.Name, DefaultProfileName)
	}
	if p.ID == "" {
		t.Error("Name has no synthesised ID")
	}
	if llm.DefaultProfileID != p.ID {
		t.Errorf("DefaultProfileID = %q, want %q", llm.DefaultProfileID, p.ID)
	}
}

func TestMigrate_V011LegacyShape_PreservesAllFields(t *testing.T) {
	legacy := `{
		"default_backend": "vertex_ai",
		"local": {
			"endpoint": "http://legacy:1234",
			"model": "old-local",
			"api_key_env": "OLD_KEY",
			"request_timeout_seconds": 222
		},
		"vertex_ai": {
			"project_id": "legacy-proj",
			"region": "asia-northeast1",
			"model": "legacy-flash",
			"retry_max_attempts": 7
		}
	}`
	var llm LLMConfig
	if err := json.Unmarshal([]byte(legacy), &llm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := llm.Profiles[0]
	if p.DefaultBackend != BackendVertexAI {
		t.Errorf("DefaultBackend = %v, want vertex_ai", p.DefaultBackend)
	}
	if p.Local.Endpoint != "http://legacy:1234" {
		t.Errorf("Local.Endpoint = %q", p.Local.Endpoint)
	}
	if p.Local.Model != "old-local" {
		t.Errorf("Local.Model = %q", p.Local.Model)
	}
	if p.Local.APIKeyEnv != "OLD_KEY" {
		t.Errorf("Local.APIKeyEnv = %q", p.Local.APIKeyEnv)
	}
	if p.Local.RequestTimeoutSeconds != 222 {
		t.Errorf("Local.RequestTimeoutSeconds = %d", p.Local.RequestTimeoutSeconds)
	}
	if p.VertexAI.ProjectID != "legacy-proj" {
		t.Errorf("VertexAI.ProjectID = %q", p.VertexAI.ProjectID)
	}
	if p.VertexAI.Region != "asia-northeast1" {
		t.Errorf("VertexAI.Region = %q", p.VertexAI.Region)
	}
	if p.VertexAI.Model != "legacy-flash" {
		t.Errorf("VertexAI.Model = %q", p.VertexAI.Model)
	}
	if p.VertexAI.RetryMaxAttempts != 7 {
		t.Errorf("VertexAI.RetryMaxAttempts = %d", p.VertexAI.RetryMaxAttempts)
	}
}

func TestMigrate_AlreadyMigrated_NoChange(t *testing.T) {
	already := `{
		"default_profile_id": "fixed-id-123",
		"profiles": [{
			"id": "fixed-id-123",
			"name": "Production",
			"default_backend": "vertex_ai",
			"local": {"endpoint": "http://x:1"},
			"vertex_ai": {"project_id": "p", "model": "m"}
		}]
	}`
	var llm LLMConfig
	if err := json.Unmarshal([]byte(already), &llm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(llm.Profiles) != 1 {
		t.Fatalf("got %d profiles, want 1", len(llm.Profiles))
	}
	if llm.Profiles[0].ID != "fixed-id-123" {
		t.Errorf("ID changed: %q", llm.Profiles[0].ID)
	}
	if llm.Profiles[0].Name != "Production" {
		t.Errorf("Name changed: %q", llm.Profiles[0].Name)
	}
	if llm.DefaultProfileID != "fixed-id-123" {
		t.Errorf("DefaultProfileID changed: %q", llm.DefaultProfileID)
	}
}

func TestMigrate_DanglingDefaultProfileID_RepairsToFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(DataDir(), 0700); err != nil {
		t.Fatal(err)
	}
	dangling := `{
		"llm": {
			"default_profile_id": "does-not-exist",
			"profiles": [{
				"id": "real-id",
				"name": "Real",
				"default_backend": "local",
				"local": {},
				"vertex_ai": {}
			}]
		}
	}`
	if err := os.WriteFile(ConfigPath(), []byte(dangling), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.DefaultProfileID != "real-id" {
		t.Errorf("DefaultProfileID = %q, want repaired to %q", cfg.LLM.DefaultProfileID, "real-id")
	}
}

// --- ResolveProfile / DefaultProfile / HasProfile ------------------

func TestResolveProfile_KnownID_ReturnsProfile(t *testing.T) {
	llm := LLMConfig{
		DefaultProfileID: "p1",
		Profiles: []LLMProfile{
			{ID: "p1", Name: "Alpha"},
			{ID: "p2", Name: "Beta"},
		},
	}
	got := llm.ResolveProfile("p2")
	if got == nil || got.Name != "Beta" {
		t.Errorf("ResolveProfile(p2) returned %v, want Beta", got)
	}
}

func TestResolveProfile_UnknownID_ReturnsDefault(t *testing.T) {
	llm := LLMConfig{
		DefaultProfileID: "p1",
		Profiles: []LLMProfile{
			{ID: "p1", Name: "Alpha"},
			{ID: "p2", Name: "Beta"},
		},
	}
	got := llm.ResolveProfile("no-such-id")
	if got == nil || got.Name != "Alpha" {
		t.Errorf("ResolveProfile(unknown) returned %v, want Alpha (default)", got)
	}
}

func TestResolveProfile_EmptyID_ReturnsDefault(t *testing.T) {
	llm := LLMConfig{
		DefaultProfileID: "p1",
		Profiles: []LLMProfile{
			{ID: "p1", Name: "Alpha"},
			{ID: "p2", Name: "Beta"},
		},
	}
	got := llm.ResolveProfile("")
	if got == nil || got.Name != "Alpha" {
		t.Errorf("ResolveProfile(empty) returned %v, want Alpha (default)", got)
	}
}

func TestResolveProfile_NoProfiles_ReturnsNil(t *testing.T) {
	llm := LLMConfig{}
	if got := llm.ResolveProfile("anything"); got != nil {
		t.Errorf("ResolveProfile on empty Profiles = %v, want nil", got)
	}
}

func TestHasProfile(t *testing.T) {
	llm := LLMConfig{Profiles: []LLMProfile{{ID: "abc"}, {ID: "def"}}}
	if !llm.HasProfile("abc") {
		t.Error("HasProfile(abc) = false, want true")
	}
	if llm.HasProfile("xyz") {
		t.Error("HasProfile(xyz) = true, want false")
	}
}

// --- ProfileByName --------------------------------------------------

func TestProfileByName_UniqueMatch(t *testing.T) {
	llm := LLMConfig{Profiles: []LLMProfile{
		{ID: "p1", Name: "Alpha"},
		{ID: "p2", Name: "Beta"},
	}}
	p, ok, amb := llm.ProfileByName("beta") // case-insensitive
	if !ok || amb || p == nil || p.ID != "p2" {
		t.Errorf("ProfileByName(beta) = (%v, %v, ambig=%v), want unique match for p2", p, ok, amb)
	}
}

func TestProfileByName_NoMatch(t *testing.T) {
	llm := LLMConfig{Profiles: []LLMProfile{{ID: "p1", Name: "Alpha"}}}
	p, ok, amb := llm.ProfileByName("zeta")
	if ok || amb || p != nil {
		t.Errorf("ProfileByName(zeta) = (%v, %v, ambig=%v), want no match", p, ok, amb)
	}
}

func TestProfileByName_Ambiguous(t *testing.T) {
	// Reachable only through hand-edited config.json; Settings Save
	// auto-disambiguates duplicates. Defensive code still flags.
	llm := LLMConfig{Profiles: []LLMProfile{
		{ID: "p1", Name: "Alpha"},
		{ID: "p2", Name: "alpha"}, // case-insensitive duplicate
	}}
	_, ok, amb := llm.ProfileByName("Alpha")
	if ok || !amb {
		t.Errorf("ProfileByName(Alpha) ok=%v ambig=%v, want ambiguous", ok, amb)
	}
}

// --- DisambiguateName ----------------------------------------------

func TestDisambiguateName_NoCollision_ReturnsDesired(t *testing.T) {
	profiles := []LLMProfile{{ID: "p1", Name: "Existing"}}
	if got := DisambiguateName(profiles, "Fresh", ""); got != "Fresh" {
		t.Errorf("DisambiguateName(Fresh) = %q, want Fresh", got)
	}
}

func TestDisambiguateName_OneCollision_AppendsSuffix2(t *testing.T) {
	profiles := []LLMProfile{{ID: "p1", Name: "Foo"}}
	if got := DisambiguateName(profiles, "Foo", ""); got != "Foo (2)" {
		t.Errorf("DisambiguateName(Foo) = %q, want Foo (2)", got)
	}
}

func TestDisambiguateName_ChainOfCollisions_FindsLowestFree(t *testing.T) {
	profiles := []LLMProfile{
		{ID: "p1", Name: "Foo"},
		{ID: "p2", Name: "Foo (2)"},
		{ID: "p3", Name: "Foo (3)"},
	}
	if got := DisambiguateName(profiles, "Foo", ""); got != "Foo (4)" {
		t.Errorf("DisambiguateName(Foo) = %q, want Foo (4)", got)
	}
}

func TestDisambiguateName_GapInChain_FillsLowestFree(t *testing.T) {
	// Finder behaviour: gap in numbering means we backfill the
	// lowest free slot, not always go to max+1.
	profiles := []LLMProfile{
		{ID: "p1", Name: "Foo"},
		{ID: "p3", Name: "Foo (3)"},
	}
	if got := DisambiguateName(profiles, "Foo", ""); got != "Foo (2)" {
		t.Errorf("DisambiguateName(Foo) = %q, want Foo (2) (fill gap)", got)
	}
}

func TestDisambiguateName_CaseInsensitive(t *testing.T) {
	profiles := []LLMProfile{{ID: "p1", Name: "Foo"}}
	got := DisambiguateName(profiles, "foo", "")
	if !strings.EqualFold(got, "foo (2)") {
		t.Errorf("DisambiguateName(foo) = %q, want foo (2) case-insensitive collision", got)
	}
}

func TestDisambiguateName_SelfIDExcluded(t *testing.T) {
	profiles := []LLMProfile{
		{ID: "p1", Name: "Foo"},
		{ID: "p2", Name: "Bar"},
	}
	// Rename of p1 (still keeping its name "Foo") should not
	// auto-suffix — the only profile with Name=Foo is itself.
	if got := DisambiguateName(profiles, "Foo", "p1"); got != "Foo" {
		t.Errorf("DisambiguateName(Foo, self=p1) = %q, want Foo", got)
	}
	// But renaming p2 to "Foo" must auto-suffix (p1 owns "Foo").
	if got := DisambiguateName(profiles, "Foo", "p2"); got != "Foo (2)" {
		t.Errorf("DisambiguateName(Foo, self=p2) = %q, want Foo (2)", got)
	}
}

// --- repairProfiles -------------------------------------------------

func TestRepairProfiles_EmptyProfilesGetsCanonicalDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(DataDir(), 0700); err != nil {
		t.Fatal(err)
	}
	// Config file present but llm.profiles is empty.
	if err := os.WriteFile(ConfigPath(), []byte(`{"llm": {"profiles": []}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.LLM.Profiles) == 0 {
		t.Error("expected repairProfiles to install the canonical default profile")
	}
}
