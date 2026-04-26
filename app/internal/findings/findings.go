// Package findings manages the global findings store.
// Findings are analysis-derived insights promoted from sessions.
package findings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// Content is sanitized: newlines collapsed, length capped per finding,
// to prevent prompt injection via user-influenced finding content.
func (s *Store) FormatForPrompt() string {
	if len(s.findings) == 0 {
		return ""
	}
	result := ""
	for _, f := range s.findings {
		content := sanitizeForPrompt(f.Content, 500)
		title := sanitizeForPrompt(f.OriginSessionTitle, 100)
		result += fmt.Sprintf("- [%s] %s (from: %s, session: %s)\n",
			f.CreatedLabel, content, title, f.OriginSessionID)
	}
	return result
}

// sanitizeForPrompt removes control chars and newlines, caps length.
// Used when user-influenced content is embedded in system prompt.
func sanitizeForPrompt(s string, maxLen int) string {
	var b []rune
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			b = append(b, ' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b = append(b, r)
		if len(b) >= maxLen {
			break
		}
	}
	return strings.TrimSpace(string(b))
}
