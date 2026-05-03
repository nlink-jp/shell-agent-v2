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

// Source values for Finding.Source. See
// docs/en/memory-injection-hardening.md §5 Phase A.
const (
	SourceLLMPromoted = "llm_promoted" // promoted by an LLM tool call (promote-finding)
	SourceManual      = "manual"       // promoted manually via Settings UI / API
)

// Finding is an analysis insight with origin provenance.
//
// Source/ToolOriginated were added in v0.1.26 for memory-injection
// provenance. See docs/en/memory-injection-hardening.md §5 Phase A.
// Existing entries from earlier versions have empty Source — those
// are rendered with the lower-trust [derived] tag in FormatForPrompt.
type Finding struct {
	ID                 string   `json:"id"`
	Content            string   `json:"content"`
	OriginSessionID    string   `json:"origin_session_id"`
	OriginSessionTitle string   `json:"origin_session_title"`
	Tags               []string `json:"tags"`
	CreatedAt          string   `json:"created_at"`
	CreatedLabel       string   `json:"created_label"`

	// Provenance (v0.1.26+).
	Source         string `json:"source,omitempty"`          // Source* constants
	ToolOriginated bool   `json:"tool_originated,omitempty"` // surrounding turn included tool output
}

// DefaultMaxFindings is the soft cap on Store.findings when no
// explicit MaxFindings override is set. The oldest entry is evicted
// on overflow. See docs/en/memory-injection-hardening.md §5 Phase C.
const DefaultMaxFindings = 200

// FindingsFormatBudget caps the total size of FormatForPrompt output.
// When rendered findings exceed this budget the OLDEST entries are
// elided (most-recent are most likely to be relevant).
const FindingsFormatBudget = 16 * 1024 // 16 KiB

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

	// MaxFindings is the soft cap on findings. Zero falls back to
	// DefaultMaxFindings. FIFO eviction.
	MaxFindings int
}

// NewStore creates a store backed by the default findings file.
func NewStore() *Store {
	return &Store{
		path:        filepath.Join(config.DataDir(), "findings.json"),
		MaxFindings: DefaultMaxFindings,
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
//
// source/toolOriginated record the provenance of the finding's
// content. Pass SourceLLMPromoted for promote-finding tool calls
// (potentially attacker-influenced content) and SourceManual for
// user-initiated promotion via the UI.
func (s *Store) Add(content, sessionID, sessionTitle string, tags []string, source string, toolOriginated bool) *Finding {
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
		Source:             source,
		ToolOriginated:     toolOriginated,
	}
	s.findings = append(s.findings, f)

	cap := s.MaxFindings
	if cap <= 0 {
		cap = DefaultMaxFindings
	}
	if len(s.findings) > cap {
		s.findings = s.findings[1:]
	}
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
//
// Each line carries a trust tag derived from Source:
//
//   - [user-stated]: SourceManual — promoted by the user via UI.
//   - [derived]: SourceLLMPromoted or empty (legacy) — content traces
//     back through the LLM and may be attacker-influenced. See
//     docs/en/memory-injection-hardening.md.
func (s *Store) FormatForPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.findings) == 0 {
		return ""
	}
	lines := make([]string, len(s.findings))
	for i, f := range s.findings {
		content := sanitizeForPrompt(f.Content, 500)
		title := sanitizeForPrompt(f.OriginSessionTitle, 100)
		lines[i] = fmt.Sprintf("- %s [%s] %s (from: %s, session: %s)\n",
			trustTag(f.Source), f.CreatedLabel, content, title, f.OriginSessionID)
	}
	// Walk newest → oldest, including lines until the budget runs
	// out. Most-recent findings are most likely to be relevant.
	included := make([]string, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if used+len(lines[i]) > FindingsFormatBudget {
			break
		}
		included = append([]string{lines[i]}, included...)
		used += len(lines[i])
	}
	elided := len(lines) - len(included)

	var sb strings.Builder
	if elided > 0 {
		sb.WriteString(fmt.Sprintf("(%d earlier findings elided to fit budget)\n", elided))
	}
	for _, l := range included {
		sb.WriteString(l)
	}
	return sb.String()
}

// trustTag maps a Finding.Source to the leading bracketed token in
// FormatForPrompt. Anything unknown (including legacy empty Source
// and SourceLLMPromoted) gets [derived] — the lower-trust default,
// since LLM-promoted findings may carry content originally derived
// from attacker-controlled tool output.
func trustTag(source string) string {
	if source == SourceManual {
		return "[user-stated]"
	}
	return "[derived]"
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
