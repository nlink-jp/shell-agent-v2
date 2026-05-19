// Package config — profile-related types and helpers.
//
// A profile is a named pair of (Local, VertexAI) backend configs plus
// a default_backend flag selecting which side `/model` lands on when
// a session loaded against this profile is fresh. Sessions reference
// a profile by UUID via the per-session session.json file (see
// internal/memory). Multiple profiles let the user attribute Vertex
// AI charges to different GCP projects and point Local at different
// LM Studio endpoints. See ADR-0016.
package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// LLMProfile bundles one (Local, VertexAI) pair plus the
// side that `/model` lands on when a fresh session is loaded against
// this profile. The ID is a UUID v4 — immutable and the only stable
// reference. The Name is a user-facing label, mutable, and uniqueness
// is enforced via auto-suffix on Save (see DisambiguateName).
type LLMProfile struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	DefaultBackend LLMBackend     `json:"default_backend"`
	Local          LocalConfig    `json:"local"`
	VertexAI       VertexAIConfig `json:"vertex_ai"`
}

// NewProfileID returns a fresh UUID v4 for a new profile.
func NewProfileID() string {
	return uuid.NewString()
}

// DefaultProfileName is the name given to the synthesised single
// profile created on first run or when migrating a v0.11.x config.
const DefaultProfileName = "Default"

// DefaultProfile returns a pointer to the profile referenced by
// DefaultProfileID, or the first profile when DefaultProfileID is
// empty or dangling. Returns nil only when Profiles is empty —
// which Load() guarantees never happens.
func (c *LLMConfig) DefaultProfile() *LLMProfile {
	if len(c.Profiles) == 0 {
		return nil
	}
	for i := range c.Profiles {
		if c.Profiles[i].ID == c.DefaultProfileID {
			return &c.Profiles[i]
		}
	}
	return &c.Profiles[0]
}

// ResolveProfile returns the profile with the given ID, falling back
// to DefaultProfile when the ID is empty or unknown. Returns nil only
// when Profiles is empty.
func (c *LLMConfig) ResolveProfile(id string) *LLMProfile {
	if len(c.Profiles) == 0 {
		return nil
	}
	if id != "" {
		for i := range c.Profiles {
			if c.Profiles[i].ID == id {
				return &c.Profiles[i]
			}
		}
	}
	return c.DefaultProfile()
}

// HasProfile reports whether a profile with the given ID exists.
// Used to detect dangling session.json references that need to fall
// back to the default (ADR-0016 §3.3 step 3b).
func (c *LLMConfig) HasProfile(id string) bool {
	for i := range c.Profiles {
		if c.Profiles[i].ID == id {
			return true
		}
	}
	return false
}

// ProfileByName resolves a profile by case-insensitive Name match.
// Returns the profile and true on unique match; returns nil and
// false when no match or when multiple profiles share the name
// (the latter is unreachable through the normal Settings Save path
// thanks to DisambiguateName, but stays as defensive code for users
// who hand-edit config.json — see ADR-0016 §3.4).
func (c *LLMConfig) ProfileByName(name string) (*LLMProfile, bool, bool /*ambiguous*/) {
	target := strings.ToLower(name)
	var match *LLMProfile
	ambiguous := false
	for i := range c.Profiles {
		if strings.ToLower(c.Profiles[i].Name) != target {
			continue
		}
		if match != nil {
			ambiguous = true
			break
		}
		match = &c.Profiles[i]
	}
	if ambiguous {
		return nil, false, true
	}
	if match == nil {
		return nil, false, false
	}
	return match, true, false
}

// DisambiguateName returns desired if no other profile (excluding the
// one identified by selfID, if any) has that Name (case-insensitive).
// Otherwise it returns "<desired> (N)" with the smallest integer
// N ≥ 2 that resolves the collision. Matches the macOS Finder
// duplicate-name suffix convention. See ADR-0016 §3.5.
func DisambiguateName(profiles []LLMProfile, desired, selfID string) string {
	taken := make(map[string]struct{}, len(profiles))
	for _, p := range profiles {
		if p.ID == selfID {
			continue
		}
		taken[strings.ToLower(p.Name)] = struct{}{}
	}
	if _, clash := taken[strings.ToLower(desired)]; !clash {
		return desired
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%d)", desired, n)
		if _, clash := taken[strings.ToLower(candidate)]; !clash {
			return candidate
		}
	}
}

// UnmarshalJSON implements migration from the v0.11.x shape to the
// v0.12.0 multi-profile shape. ADR-0016 §3.6.
//
// Old shape (v0.11.x):
//
//	{"default_backend": "local", "local": {...}, "vertex_ai": {...}}
//
// New shape (v0.12.0):
//
//	{"default_profile_id": "...", "profiles": [...]}
//
// When the JSON has the old shape (no `profiles` array but at least
// one of the legacy fields present), a single "Default" profile is
// synthesised holding the legacy values. After this method returns,
// the in-memory LLMConfig is always in the new shape; the next
// Save() persists it that way and drops the old top-level fields.
func (c *LLMConfig) UnmarshalJSON(data []byte) error {
	// transitional shape — accepts both old and new fields so we can
	// detect which version of the config we're loading.
	var aux struct {
		// new
		DefaultProfileID string       `json:"default_profile_id"`
		Profiles         []LLMProfile `json:"profiles"`
		// old (only present in v0.11.x configs)
		DefaultBackend LLMBackend      `json:"default_backend"`
		Local          *LocalConfig    `json:"local"`
		VertexAI       *VertexAIConfig `json:"vertex_ai"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Profiles) > 0 {
		c.DefaultProfileID = aux.DefaultProfileID
		c.Profiles = aux.Profiles
		return nil
	}
	// Old shape: synthesise a single profile from the legacy fields.
	// At least one of Local / VertexAI / DefaultBackend is non-zero,
	// otherwise this is an empty {"llm": {}} block and we leave the
	// struct empty for Default() to fill.
	if aux.Local == nil && aux.VertexAI == nil && aux.DefaultBackend == "" {
		return nil
	}
	profile := LLMProfile{
		ID:             NewProfileID(),
		Name:           DefaultProfileName,
		DefaultBackend: aux.DefaultBackend,
	}
	if profile.DefaultBackend == "" {
		profile.DefaultBackend = BackendLocal
	}
	if aux.Local != nil {
		profile.Local = *aux.Local
	}
	if aux.VertexAI != nil {
		profile.VertexAI = *aux.VertexAI
	}
	c.DefaultProfileID = profile.ID
	c.Profiles = []LLMProfile{profile}
	return nil
}
