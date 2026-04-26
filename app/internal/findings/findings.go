// Package findings manages the global findings store.
// Findings are analysis-derived insights promoted from sessions.
package findings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Finding is an analysis insight with origin provenance.
type Finding struct {
	ID                 string   `json:"id"`
	Content            string   `json:"content"`
	OriginSessionID    string   `json:"origin_session_id"`
	OriginSessionTitle string   `json:"origin_session_title"`
	Tags               []string `json:"tags"`
	CreatedAt          string   `json:"created_at"`
	CreatedLabel       string   `json:"created_label"`
}

// Store manages the global findings collection.
type Store struct {
	path     string
	findings []Finding
}

// NewStore creates a store backed by the default findings file.
func NewStore() *Store {
	return &Store{
		path: filepath.Join(config.DataDir(), "findings.json"),
	}
}

// Load reads findings from disk.
func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.findings = []Finding{}
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.findings)
}

// Save writes findings to disk.
func (s *Store) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.findings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Add promotes a new finding to the global store.
func (s *Store) Add(content, sessionID, sessionTitle string, tags []string) *Finding {
	now := time.Now()
	f := Finding{
		ID:                 fmt.Sprintf("f-%s-%03d", now.Format("20060102"), len(s.findings)+1),
		Content:            content,
		OriginSessionID:    sessionID,
		OriginSessionTitle: sessionTitle,
		Tags:               tags,
		CreatedAt:          now.Format(time.RFC3339),
		CreatedLabel:       fmt.Sprintf("%s (%s)", now.Format("2006-01-02"), now.Format("Monday")),
	}
	s.findings = append(s.findings, f)
	return &f
}

// All returns all findings.
func (s *Store) All() []Finding {
	return s.findings
}

// DeleteBySession removes all findings originating from the given session.
func (s *Store) DeleteBySession(sessionID string) {
	var kept []Finding
	for _, f := range s.findings {
		if f.OriginSessionID != sessionID {
			kept = append(kept, f)
		}
	}
	s.findings = kept
}

// FormatForPrompt returns findings formatted for system prompt injection.
func (s *Store) FormatForPrompt() string {
	if len(s.findings) == 0 {
		return ""
	}
	result := ""
	for _, f := range s.findings {
		result += fmt.Sprintf("- [%s] %s (from: %s, session: %s)\n",
			f.CreatedLabel, f.Content, f.OriginSessionTitle, f.OriginSessionID)
	}
	return result
}
