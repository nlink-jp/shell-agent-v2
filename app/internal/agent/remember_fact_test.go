package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestRememberFact_RoutesPreferenceToGlobal: a preference-category
// fact must land in GlobalMemory, with Source = ToolCall so the
// audit trail distinguishes it from auto-extracted records.
func TestRememberFact_RoutesPreferenceToGlobal(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)
	// setupAgentWithAnalysis doesn't wire memory stores; do it
	// directly so toolRememberFact has somewhere to write.
	a.globalMemory = &memory.GlobalMemoryStore{}
	a.sessionMemory = &memory.SessionMemoryStore{}

	out, err := a.toolRememberFact(`{"fact":"User prefers concise responses","category":"preference"}`)
	if err != nil {
		t.Fatalf("toolRememberFact: %v", err)
	}
	if !strings.Contains(out, "global memory") {
		t.Errorf("success message should mention global memory: %q", out)
	}
	if len(a.globalMemory.Entries) != 1 {
		t.Fatalf("globalMemory count = %d, want 1", len(a.globalMemory.Entries))
	}
	got := a.globalMemory.Entries[0]
	if got.Fact != "User prefers concise responses" {
		t.Errorf("Fact = %q", got.Fact)
	}
	if got.Category != "preference" {
		t.Errorf("Category = %q", got.Category)
	}
	if got.Source != memory.GlobalSourceToolCall {
		t.Errorf("Source = %q, want %q", got.Source, memory.GlobalSourceToolCall)
	}
	if len(a.sessionMemory.Entries) != 0 {
		t.Errorf("sessionMemory should be empty for preference-route fact")
	}
}

// TestRememberFact_RoutesFactToSession: a fact-category record must
// land in SessionMemory (per-session, not cross-session).
func TestRememberFact_RoutesFactToSession(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)
	a.globalMemory = &memory.GlobalMemoryStore{}
	a.sessionMemory = &memory.SessionMemoryStore{}

	out, err := a.toolRememberFact(`{"fact":"Working on the Q2 sales analysis","category":"context"}`)
	if err != nil {
		t.Fatalf("toolRememberFact: %v", err)
	}
	if !strings.Contains(out, "session memory") {
		t.Errorf("success message should mention session memory: %q", out)
	}
	if len(a.sessionMemory.Entries) != 1 {
		t.Fatalf("sessionMemory count = %d, want 1", len(a.sessionMemory.Entries))
	}
	if a.sessionMemory.Entries[0].Source != memory.SessionSourceToolCall {
		t.Errorf("Source = %q, want %q",
			a.sessionMemory.Entries[0].Source, memory.SessionSourceToolCall)
	}
	if len(a.globalMemory.Entries) != 0 {
		t.Errorf("globalMemory should be empty for context-route fact")
	}
}

// TestRememberFact_RejectsSelfReferential: the IsSelfReferential
// filter must keep THINK-leakage-class facts out of either store.
// This is the same defence extractMemories uses; the tool path
// must not be a bypass.
func TestRememberFact_RejectsSelfReferential(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)
	a.globalMemory = &memory.GlobalMemoryStore{}
	a.sessionMemory = &memory.SessionMemoryStore{}

	_, err := a.toolRememberFact(`{"fact":"The assistant should always use bullet points","category":"preference"}`)
	if err == nil {
		t.Fatal("expected error for self-referential fact, got nil")
	}
	if !strings.Contains(err.Error(), "assistant") {
		t.Errorf("error should mention assistant/self-referential: %v", err)
	}
	if len(a.globalMemory.Entries) != 0 || len(a.sessionMemory.Entries) != 0 {
		t.Errorf("self-referential fact should not be stored")
	}
}

// TestRememberFact_RejectsInvalidCategory: only the four allowlist
// categories are accepted. Anything else returns a tool error so the
// LLM gets feedback.
func TestRememberFact_RejectsInvalidCategory(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)
	a.globalMemory = &memory.GlobalMemoryStore{}
	a.sessionMemory = &memory.SessionMemoryStore{}

	_, err := a.toolRememberFact(`{"fact":"x","category":"system_rule"}`)
	if err == nil {
		t.Fatal("expected error for invalid category")
	}
}

// TestRememberFact_EmptyFactRejected: empty / whitespace-only fact
// strings must be rejected before any storage call.
func TestRememberFact_EmptyFactRejected(t *testing.T) {
	a, _ := setupAgentWithAnalysis(t)
	a.globalMemory = &memory.GlobalMemoryStore{}
	a.sessionMemory = &memory.SessionMemoryStore{}

	if _, err := a.toolRememberFact(`{"fact":"   ","category":"fact"}`); err == nil {
		t.Fatal("expected error for whitespace-only fact")
	}
}

// TestBuildToolDefs_RememberFactExclusivity: when the active
// backend has AutoExtract on, the LLM-presented tool list omits
// remember-fact. When off, it's present. The descriptor itself is
// always registered (verified by descriptorToolDefs count) — the
// filter is a presentation-time gate in buildToolDefs.
func TestBuildToolDefs_RememberFactExclusivity(t *testing.T) {
	a := agentForToolDefs(t)

	// Default-of-default: Local profile, Local backend, extract=off
	// (ADR-0019 default) → remember-fact should be PRESENT.
	tools := a.buildToolDefs()
	if !containsTool(tools, "remember_fact") {
		t.Error("remember-fact should be present when AutoExtract is off (local default)")
	}

	// Flip Local to extract=on → remember-fact should be hidden.
	on := true
	prof := &a.cfg.LLM.Profiles[0]
	prof.Local.AutoExtractEnabled = &on
	tools = a.buildToolDefs()
	if containsTool(tools, "remember_fact") {
		t.Error("remember-fact should be hidden when AutoExtract is on")
	}

	// Flip back to off → present again. Confirms the gate is live
	// (re-evaluated per call), not cached at New() time.
	off := false
	prof.Local.AutoExtractEnabled = &off
	tools = a.buildToolDefs()
	if !containsTool(tools, "remember_fact") {
		t.Error("remember-fact should be present after toggling AutoExtract off again")
	}
}

// TestAutoTitle_DefaultConfigConfigured guards the ADR-0020 wiring:
// fresh local profile must resolve to AutoTitle=off (so the title-
// gen LLM call doesn't fire between turns and trash llama.cpp's
// prefix cache). Vertex must resolve to on.
func TestAutoTitle_DefaultConfigConfigured(t *testing.T) {
	a := agentForToolDefs(t)
	if a.autoTitleEnabled() {
		t.Fatal("autoTitleEnabled() = true for fresh local profile, want false (ADR-0020)")
	}
	if got := config.LocalAutoTitleDefault; got != false {
		t.Errorf("LocalAutoTitleDefault drifted: %v", got)
	}
	if got := config.VertexAutoTitleDefault; got != true {
		t.Errorf("VertexAutoTitleDefault drifted: %v", got)
	}
}

// TestGenerateTitleIfNeeded_GatedOff: when AutoTitle is off, the
// function must early-return without invoking the backend. Verified
// by leaving a.backend nil — a real LLM call would deref nil and
// panic; gating short-circuits before that.
func TestGenerateTitleIfNeeded_GatedOff(t *testing.T) {
	a := New(config.Default()) // local default → AutoTitle=off
	a.session = &memory.Session{
		ID:    "test",
		Title: "New Session",
		Records: []memory.Record{
			{Role: "user", Content: "hello there"},
		},
	}
	if err := a.generateTitleIfNeeded(context.Background()); err != nil {
		t.Fatalf("generateTitleIfNeeded: %v", err)
	}
	if a.session.Title != "New Session" {
		t.Errorf("Title = %q, want unchanged 'New Session' (AutoTitle off should not run)", a.session.Title)
	}
}

// TestRememberFact_DefaultConfigConfigured guards the wiring from
// config.Default() through the agent's autoExtractEnabled resolver
// — the ADR-0019 invariant that fresh-install local users get
// remember-fact and Vertex users get the auto-extractor.
func TestRememberFact_DefaultConfigConfigured(t *testing.T) {
	a := agentForToolDefs(t)
	// Fresh agent, default config → autoExtract resolves to local
	// default (off), so the tool must be visible.
	if a.autoExtractEnabled() {
		t.Fatal("autoExtractEnabled() = true for fresh local profile, want false (ADR-0019 §3.1)")
	}
	if got := config.LocalAutoExtractDefault; got != false {
		t.Errorf("LocalAutoExtractDefault drifted: %v", got)
	}
	if got := config.VertexAutoExtractDefault; got != true {
		t.Errorf("VertexAutoExtractDefault drifted: %v", got)
	}
}
