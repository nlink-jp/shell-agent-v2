// Package findings manages the global findings store.
// Findings are analysis-derived insights promoted from sessions.
package findings

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
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
//
// Concurrency: every entry point that mutates s.findings or reads it
// to derive a derived value (ID generation in particular) takes
// s.mu. Add originally derived ID from len(s.findings) without any
// lock — racing Add calls would generate duplicate IDs and a later
// DeleteByIDs would remove the wrong record
// (security-hardening-2.md H9). Save and Load also take the lock so
// the on-disk file matches the in-memory state at a single point in
// time.
type Store struct {
	mu       sync.Mutex
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
	s.mu.Lock()
	defer s.mu.Unlock()
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

// Save writes findings to disk atomically (tmp+rename) so a reader
// always sees either the previous or new file, never partial.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.findings, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(s.path, data, 0600)
}

// Add promotes a new finding to the global store.
//
// ID format: f-YYYYMMDD-NNN for the first 999 findings of any
// calendar day; if a busy day exceeds that, fall back to
// f-YYYYMMDD-NNNNNN-<6 hex> so the ID stays unique without colliding
// with the legacy fixed-width format.
func (s *Store) Add(content, sessionID, sessionTitle string, tags []string) *Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	day := now.Format("20060102")
	count := s.countForDayLocked(day) + 1

	id := fmt.Sprintf("f-%s-%03d", day, count)
	if count > 999 {
		id = fmt.Sprintf("f-%s-%06d-%s", day, count, randomHex(3))
	}

	f := Finding{
		ID:                 id,
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

// countForDayLocked returns how many existing findings already use
// the f-<day>- prefix. Caller must hold s.mu.
func (s *Store) countForDayLocked(day string) int {
	prefix := "f-" + day + "-"
	count := 0
	for _, f := range s.findings {
		if strings.HasPrefix(f.ID, prefix) {
			count++
		}
	}
	return count
}

// randomHex returns 2*n hex chars from crypto/rand. Used as a
// uniqueness suffix on overflow IDs; if rand fails (essentially
// never) we fall back to a timestamp-derived value so ID generation
// stays infallible.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano()&0xFFFF)
	}
	return hex.EncodeToString(b)
}

// All returns all findings.
func (s *Store) All() []Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Finding, len(s.findings))
	copy(out, s.findings)
	return out
}

// DeleteBySession removes all findings originating from the given session.
func (s *Store) DeleteBySession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []Finding
	for _, f := range s.findings {
		if f.OriginSessionID != sessionID {
			kept = append(kept, f)
		}
	}
	s.findings = kept
}

// DeleteByIDs removes findings whose ID is in the given set. Returns the
// number actually deleted.
func (s *Store) DeleteByIDs(ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var kept []Finding
	deleted := 0
	for _, f := range s.findings {
		if _, hit := wanted[f.ID]; hit {
			deleted++
			continue
		}
		kept = append(kept, f)
	}
	s.findings = kept
	return deleted
}

// FormatForPrompt returns findings formatted for system prompt injection.
// Content is sanitized: newlines collapsed, length capped per finding,
// to prevent prompt injection via user-influenced finding content.
func (s *Store) FormatForPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
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
