// Profile-related Wails bindings for ADR-0016. The bindings layer
// stays thin: type marshaling + cfg.Save / agent forwarding. All
// non-trivial logic lives in internal/config (DisambiguateName,
// ResolveProfile) and internal/agent (applyProfileSwitch).
package main

import (
	"fmt"
	"sort"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// --- DTOs --------------------------------------------------------

// ProfileSummary is the lightweight view used by the popover
// dropdown and the Settings tab's profile list. The full per-side
// config lives in ProfileDetail and is fetched on demand.
type ProfileSummary struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DefaultBackend string `json:"default_backend"`
	LocalModel     string `json:"local_model"`
	VertexModel    string `json:"vertex_model"`
	IsDefault      bool   `json:"is_default"`
}

// ProfileDetail is the full editable view of one profile, used by
// the Settings tab's edit form.
type ProfileDetail struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	DefaultBackend string             `json:"default_backend"`
	IsDefault      bool               `json:"is_default"`
	Local          LocalProfileFields `json:"local"`
	Vertex         VertexProfileFields `json:"vertex"`
}

// LocalProfileFields mirrors config.LocalConfig in a form the
// frontend can edit. Mirrors the previous SettingsData.Local* shape
// so v0.11.x UI patterns port cleanly.
type LocalProfileFields struct {
	Endpoint              string     `json:"endpoint"`
	Model                 string     `json:"model"`
	APIKeyEnv             string     `json:"api_key_env"`
	ContextBudget         BackendBudgetData `json:"context_budget"`
	RequestTimeoutSeconds int        `json:"request_timeout_seconds"`
	RetryMaxAttempts      int        `json:"retry_max_attempts"`
}

// VertexProfileFields mirrors config.VertexAIConfig.
type VertexProfileFields struct {
	ProjectID             string     `json:"project_id"`
	Region                string     `json:"region"`
	Model                 string     `json:"model"`
	ContextBudget         BackendBudgetData `json:"context_budget"`
	RequestTimeoutSeconds int        `json:"request_timeout_seconds"`
	RetryMaxAttempts      int        `json:"retry_max_attempts"`
}

// CreateProfileRequest is the input for CreateProfile.
//
// CloneFromID picks a source profile whose Local/Vertex settings
// (except endpoint/project_id) are copied as the starting point —
// matches the "Clone of: X" template in the Settings UI mock.
// Empty CloneFromID = empty Local / Vertex (caller fills via
// UpdateProfile).
//
// DefaultSide is the new profile's default_backend; "" defaults to
// "local".
type CreateProfileRequest struct {
	Name        string `json:"name"`
	CloneFromID string `json:"clone_from_id,omitempty"`
	DefaultSide string `json:"default_side,omitempty"`
}

// CreateProfileResult includes whether the requested Name was
// auto-disambiguated so the frontend can surface a one-time
// "Renamed to X (2)" toast.
type CreateProfileResult struct {
	Profile        ProfileSummary `json:"profile"`
	NameAdjusted   bool           `json:"name_adjusted"`
	OriginalName   string         `json:"original_name"`
}

// UpdateProfileRequest is the input for UpdateProfile.
type UpdateProfileRequest struct {
	Name           string              `json:"name"`
	DefaultBackend string              `json:"default_backend"`
	Local          LocalProfileFields  `json:"local"`
	Vertex         VertexProfileFields `json:"vertex"`
}

// UpdateProfileResult also reports auto-disambiguation.
type UpdateProfileResult struct {
	Profile        ProfileSummary `json:"profile"`
	NameAdjusted   bool           `json:"name_adjusted"`
	OriginalName   string         `json:"original_name"`
}

// DeleteProfileResult reports how many other sessions still
// reference the deleted profile — those sessions will fall back to
// the default profile on next load (lazy, ADR-0016 §3.3 step 3b).
type DeleteProfileResult struct {
	DeletedID          string `json:"deleted_id"`
	NewDefaultProfile  string `json:"new_default_profile,omitempty"` // set when we had to repair DefaultProfileID
	ReassignedSessions int    `json:"reassigned_sessions"`           // only counts the *currently loaded* session for now (commit 5 scope); other sessions migrate lazily
}

// --- Helpers -----------------------------------------------------

func profileToSummary(p *config.LLMProfile, isDefault bool) ProfileSummary {
	return ProfileSummary{
		ID:             p.ID,
		Name:           p.Name,
		DefaultBackend: string(p.DefaultBackend),
		LocalModel:     p.Local.Model,
		VertexModel:    p.VertexAI.Model,
		IsDefault:      isDefault,
	}
}

func profileToDetail(p *config.LLMProfile, isDefault bool) ProfileDetail {
	return ProfileDetail{
		ID:             p.ID,
		Name:           p.Name,
		DefaultBackend: string(p.DefaultBackend),
		IsDefault:      isDefault,
		Local: LocalProfileFields{
			Endpoint:              p.Local.Endpoint,
			Model:                 p.Local.Model,
			APIKeyEnv:             p.Local.APIKeyEnv,
			ContextBudget:         toBudget(p.Local.ContextBudget),
			RequestTimeoutSeconds: p.Local.LocalRequestTimeout(),
			RetryMaxAttempts:      resolveProfileAttempts(p.Local.RetryMaxAttempts),
		},
		Vertex: VertexProfileFields{
			ProjectID:             p.VertexAI.ProjectID,
			Region:                p.VertexAI.Region,
			Model:                 p.VertexAI.Model,
			ContextBudget:         toBudget(p.VertexAI.ContextBudget),
			RequestTimeoutSeconds: p.VertexAI.VertexRequestTimeout(),
			RetryMaxAttempts:      resolveProfileAttempts(p.VertexAI.RetryMaxAttempts),
		},
	}
}

func resolveProfileAttempts(v int) int {
	if v <= 0 {
		return llm.DefaultMaxAttempts
	}
	return v
}

// toBudget converts a config.ContextBudgetConfig into the
// frontend-facing BackendBudgetData. Shared by GetSettings (bindings
// dialog path) and the profile CRUD bindings.
func toBudget(b config.ContextBudgetConfig) BackendBudgetData {
	return BackendBudgetData{
		MaxContextTokens:    b.MaxContextTokens,
		MaxWarmTokens:       b.MaxWarmTokens,
		MaxToolResultTokens: b.MaxToolResultTokens,
		OutputReserve:       b.OutputReserveResolved(),
	}
}

func budgetFromData(b BackendBudgetData) config.ContextBudgetConfig {
	return config.ContextBudgetConfig{
		MaxContextTokens:    b.MaxContextTokens,
		MaxWarmTokens:       b.MaxWarmTokens,
		MaxToolResultTokens: b.MaxToolResultTokens,
		OutputReserve:       b.OutputReserve,
	}
}

func resolveBackendString(s string) config.LLMBackend {
	if s == string(config.BackendVertexAI) {
		return config.BackendVertexAI
	}
	return config.BackendLocal
}

func (b *Bindings) emitProfileListChanged() {
	if b.ctx == nil {
		return
	}
	ids := make([]string, 0, len(b.cfg.LLM.Profiles))
	for i := range b.cfg.LLM.Profiles {
		ids = append(ids, b.cfg.LLM.Profiles[i].ID)
	}
	wailsRuntime.EventsEmit(b.ctx, "config:profile:list_changed", map[string]any{
		"profile_ids": ids,
	})
}

// --- Bindings ----------------------------------------------------

// ListProfiles returns lightweight summaries of every profile.
// Sorted: default profile first, then alphabetical by Name (case-
// insensitive). Used by both the Settings tab list and the popover
// dropdown.
func (b *Bindings) ListProfiles() []ProfileSummary {
	out := make([]ProfileSummary, 0, len(b.cfg.LLM.Profiles))
	for i := range b.cfg.LLM.Profiles {
		p := &b.cfg.LLM.Profiles[i]
		out = append(out, profileToSummary(p, p.ID == b.cfg.LLM.DefaultProfileID))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// GetProfile returns the full editable view of one profile.
func (b *Bindings) GetProfile(id string) (ProfileDetail, error) {
	p := b.cfg.LLM.ResolveProfile(id)
	if p == nil || (id != "" && p.ID != id) {
		return ProfileDetail{}, fmt.Errorf("profile not found: %s", id)
	}
	return profileToDetail(p, p.ID == b.cfg.LLM.DefaultProfileID), nil
}

// CreateProfile adds a new profile. Name auto-disambiguates against
// existing profiles (ADR-0016 §3.5 macOS Finder convention). When
// CloneFromID is non-empty, the new profile's Local/Vertex configs
// are seeded from that source (the caller can edit afterwards via
// UpdateProfile). The new profile is NOT automatically the default
// — SetDefaultProfile is a separate explicit action.
func (b *Bindings) CreateProfile(req CreateProfileRequest) (CreateProfileResult, error) {
	requested := req.Name
	if requested == "" {
		requested = "New Profile"
	}
	adjusted := config.DisambiguateName(b.cfg.LLM.Profiles, requested, "")

	newProf := config.LLMProfile{
		ID:             config.NewProfileID(),
		Name:           adjusted,
		DefaultBackend: resolveBackendString(req.DefaultSide),
	}
	if req.CloneFromID != "" {
		src := b.cfg.LLM.ResolveProfile(req.CloneFromID)
		if src == nil || src.ID != req.CloneFromID {
			return CreateProfileResult{}, fmt.Errorf("clone_from_id not found: %s", req.CloneFromID)
		}
		newProf.Local = src.Local
		newProf.VertexAI = src.VertexAI
	}

	b.cfg.LLM.Profiles = append(b.cfg.LLM.Profiles, newProf)
	if err := b.cfg.Save(); err != nil {
		// Roll back the in-memory change on save failure to keep
		// disk and memory in sync.
		b.cfg.LLM.Profiles = b.cfg.LLM.Profiles[:len(b.cfg.LLM.Profiles)-1]
		return CreateProfileResult{}, err
	}
	logger.Info("profile created: id=%s name=%q (requested=%q)", newProf.ID, newProf.Name, requested)
	b.emitProfileListChanged()

	return CreateProfileResult{
		Profile:      profileToSummary(&newProf, false),
		NameAdjusted: adjusted != requested,
		OriginalName: requested,
	}, nil
}

// UpdateProfile rewrites the named profile's editable fields. The
// Name is auto-disambiguated against other profiles; if no rename
// is needed the original Name is preserved verbatim.
func (b *Bindings) UpdateProfile(id string, req UpdateProfileRequest) (UpdateProfileResult, error) {
	var target *config.LLMProfile
	for i := range b.cfg.LLM.Profiles {
		if b.cfg.LLM.Profiles[i].ID == id {
			target = &b.cfg.LLM.Profiles[i]
			break
		}
	}
	if target == nil {
		return UpdateProfileResult{}, fmt.Errorf("profile not found: %s", id)
	}

	prevProfile := *target

	requested := req.Name
	if requested == "" {
		requested = prevProfile.Name
	}
	adjusted := config.DisambiguateName(b.cfg.LLM.Profiles, requested, id)
	target.Name = adjusted
	target.DefaultBackend = resolveBackendString(req.DefaultBackend)

	target.Local.Endpoint = req.Local.Endpoint
	target.Local.Model = req.Local.Model
	target.Local.APIKeyEnv = req.Local.APIKeyEnv
	target.Local.ContextBudget = budgetFromData(req.Local.ContextBudget)
	target.Local.RequestTimeoutSeconds = req.Local.RequestTimeoutSeconds
	target.Local.RetryMaxAttempts = req.Local.RetryMaxAttempts

	target.VertexAI.ProjectID = req.Vertex.ProjectID
	target.VertexAI.Region = req.Vertex.Region
	target.VertexAI.Model = req.Vertex.Model
	target.VertexAI.ContextBudget = budgetFromData(req.Vertex.ContextBudget)
	target.VertexAI.RequestTimeoutSeconds = req.Vertex.RequestTimeoutSeconds
	target.VertexAI.RetryMaxAttempts = req.Vertex.RetryMaxAttempts

	if err := b.cfg.Save(); err != nil {
		*target = prevProfile
		return UpdateProfileResult{}, err
	}
	logger.Info("profile updated: id=%s name=%q", target.ID, target.Name)
	b.emitProfileListChanged()

	// If the agent is currently running against this profile (it's
	// the active session's profile), rebuild the backend so the new
	// endpoint / project ID / retry policy take effect live.
	if b.agent != nil && id == b.cfg.LLM.DefaultProfileID && *target != prevProfile {
		b.agent.RestartLLMBackend()
	}

	return UpdateProfileResult{
		Profile:      profileToSummary(target, target.ID == b.cfg.LLM.DefaultProfileID),
		NameAdjusted: adjusted != requested,
		OriginalName: requested,
	}, nil
}

// DeleteProfile removes the named profile. Refuses to delete the
// default profile (the UI is expected to gate this too, but the
// binding layer is the source of truth). Sessions referencing the
// deleted profile fall back to the default on their next load
// (lazy, ADR-0016 §3.3 step 3b) — no eager rewrite of every
// session.json on disk.
func (b *Bindings) DeleteProfile(id string) (DeleteProfileResult, error) {
	if id == b.cfg.LLM.DefaultProfileID {
		return DeleteProfileResult{}, fmt.Errorf("cannot delete the default profile; set a different default first")
	}
	idx := -1
	for i := range b.cfg.LLM.Profiles {
		if b.cfg.LLM.Profiles[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return DeleteProfileResult{}, fmt.Errorf("profile not found: %s", id)
	}

	b.cfg.LLM.Profiles = append(b.cfg.LLM.Profiles[:idx], b.cfg.LLM.Profiles[idx+1:]...)
	if err := b.cfg.Save(); err != nil {
		// Reload from disk to recover the in-memory state on failure.
		if reloaded, lerr := config.Load(); lerr == nil {
			b.cfg = reloaded
		}
		return DeleteProfileResult{}, err
	}
	logger.Info("profile deleted: id=%s", id)
	b.emitProfileListChanged()

	result := DeleteProfileResult{DeletedID: id}

	// If the currently loaded session was using the deleted profile,
	// fall back to default and rebuild backend so the user keeps
	// chatting without an app reload.
	if b.agent != nil {
		// Re-run agent.LoadSession against the active session to
		// trigger the §3.3 fallback path (lazy session.json rewrite +
		// backend rebuild) without touching the chat records.
		if active := b.agent.ActiveSession(); active != nil && active.ProfileID == id {
			active.ProfileID = "" // force fallback resolution
			if err := b.agent.ReapplyProfile(); err != nil {
				logger.Error("DeleteProfile reapply: %v", err)
			}
			result.ReassignedSessions = 1
		}
	}

	return result, nil
}

// SetDefaultProfile marks the named profile as the new global
// default. New sessions and deleted-profile fallbacks both
// resolve to this profile. No effect on currently-loaded sessions
// — they keep their own profile binding.
func (b *Bindings) SetDefaultProfile(id string) error {
	if !b.cfg.LLM.HasProfile(id) {
		return fmt.Errorf("profile not found: %s", id)
	}
	if b.cfg.LLM.DefaultProfileID == id {
		return nil
	}
	prev := b.cfg.LLM.DefaultProfileID
	b.cfg.LLM.DefaultProfileID = id
	if err := b.cfg.Save(); err != nil {
		b.cfg.LLM.DefaultProfileID = prev
		return err
	}
	logger.Info("default profile changed: %s → %s", prev, id)
	b.emitProfileListChanged()
	return nil
}

// SwitchSessionProfile changes the currently-loaded session's
// profile (popover endpoint). Shares applyProfileSwitch with
// /profile, so busy-state gating, persistence, and
// agent:profile:changed emission are identical.
func (b *Bindings) SwitchSessionProfile(profileID string) error {
	if b.agent == nil {
		return fmt.Errorf("agent not initialised")
	}
	if b.IsBusy() {
		return fmt.Errorf("agent is busy")
	}
	return b.agent.SwitchProfileByID(profileID)
}

// SwitchSessionBackend toggles the active session between the
// current profile's Local and Vertex sides (popover endpoint).
// Equivalent to /model.
func (b *Bindings) SwitchSessionBackend(backend string) error {
	if b.agent == nil {
		return fmt.Errorf("agent not initialised")
	}
	if b.IsBusy() {
		return fmt.Errorf("agent is busy")
	}
	target := resolveBackendString(backend)
	b.agent.SwitchBackend(target)
	return nil
}

// CurrentSessionProfile returns the active session's bound profile
// summary, or an empty ProfileSummary when no session is loaded.
// Used by the popover on open to highlight the current selection.
func (b *Bindings) CurrentSessionProfile() ProfileSummary {
	if b.agent == nil {
		return ProfileSummary{}
	}
	active := b.agent.ActiveSession()
	if active == nil {
		return ProfileSummary{}
	}
	p := b.cfg.LLM.ResolveProfile(active.ProfileID)
	if p == nil {
		return ProfileSummary{}
	}
	return profileToSummary(p, p.ID == b.cfg.LLM.DefaultProfileID)
}

// CurrentSessionBackend returns the active backend string
// ("local" / "vertex_ai"). Used by the popover radio's initial state.
func (b *Bindings) CurrentSessionBackend() string {
	if b.agent == nil {
		return string(config.BackendLocal)
	}
	return b.agent.CurrentBackendName()
}

// _ keeps memory import alive when no other reference exists in
// this file (used indirectly via agent.ActiveSession returning a
// *memory.Session).
var _ = memory.SessionConfigSchemaVersion
