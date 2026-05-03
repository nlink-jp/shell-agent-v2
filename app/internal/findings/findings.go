// Package findings manages per-session data-analysis discoveries.
//
// v0.2.0 redesign: findings are session-scoped, not cross-session.
// Each session owns its own `sessions/<id>/findings.json`. Cross-
// session promotion is handled by the user explicitly via the
// "Pin to Global Memory" UI action, which creates a corresponding
// entry in GlobalMemoryStore. See docs/en/memory-model.md §4.
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
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// Source values for Finding.Source. All findings now originate
// from analysis tools — there is no manual entry path
// (the v0.1.x `/finding` slash command was removed in v0.2.0).
const (
	SourceLLMPromoted = "llm_promoted" // promoted by the promote-finding tool
	SourceAnalyzeData = "analyze_data" // emitted by analyze-data sliding-window
)

// Finding is a session-scoped data-analysis discovery.
//
// Per-session storage means OriginSessionID / OriginSessionTitle
// are no longer needed (the file location implies the session).
type Finding struct {
	ID           string   `json:"id"`
	Content      string   `json:"content"`
	Tags         []string `json:"tags"`
	CreatedAt    string   `json:"created_at"`
	CreatedLabel string   `json:"created_label"`

	// Provenance.
	Source         string `json:"source,omitempty"`
	ToolOriginated bool   `json:"tool_originated,omitempty"`
}

// DefaultMaxFindings is the soft cap on per-session findings.
// FIFO eviction past this. Lower than v0.1.x's global 200
// because it's now per session.
const DefaultMaxFindings = 100

// FindingsFormatBudget caps the rendered output size when
// injected into the system prompt.
const FindingsFormatBudget = 16 * 1024 // 16 KiB

// Store manages per-session findings.
//
// Concurrency: Add / DeleteByIDs / All / Save / Load / FormatForPrompt
// all take s.mu so concurrent callers see consistent state and
// the on-disk file matches the in-memory snapshot at a single
// point in time.
type Store struct {
	mu       sync.Mutex
	path     string
	findings []Finding

	// MaxFindings is the soft cap. Zero falls back to
	// DefaultMaxFindings.
	MaxFindings int
}

// NewStore creates a per-session store backed by
// `sessions/<sessionID>/findings.json`.
func NewStore(sessionID string) *Store {
	return &Store{
		path:        filepath.Join(memory.SessionDir(sessionID), "findings.json"),
		MaxFindings: DefaultMaxFindings,
	}
}

// Load reads findings from disk. Missing file = empty store.
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

// Save writes findings to disk atomically (tmp+rename).
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

// Add appends a new finding.
//
// ID format: f-YYYYMMDD-NNN for the first 999 findings of any
// calendar day; if exceeded, fall back to f-YYYYMMDD-NNNNNN-<6 hex>
// so the ID stays unique without colliding with the legacy
// fixed-width format.
func (s *Store) Add(content string, tags []string, source string, toolOriginated bool) *Finding {
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
		ID:             id,
		Content:        content,
		Tags:           tags,
		CreatedAt:      now.Format(time.RFC3339),
		CreatedLabel:   fmt.Sprintf("%s (%s)", now.Format("2006-01-02"), now.Format("Monday")),
		Source:         source,
		ToolOriginated: toolOriginated,
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

// countForDayLocked returns how many existing findings already
// use the f-<day>- prefix. Caller must hold s.mu.
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

// randomHex returns 2*n hex chars from crypto/rand.
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

// Get retrieves a finding by ID. Used by the Pin to Global
// Memory flow.
func (s *Store) Get(id string) (Finding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.findings {
		if f.ID == id {
			return f, true
		}
	}
	return Finding{}, false
}

// DeleteByIDs removes findings whose ID is in the given set.
// Returns the number actually deleted.
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

// FormatForPrompt returns findings formatted for system prompt
// injection. All findings are LLM-derived (no manual source) so
// every entry renders with the lower-trust [derived] tag.
//
// Per-session storage means we no longer emit "from: ... session: ..."
// suffixes — the calling session is the only context.
func (s *Store) FormatForPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.findings) == 0 {
		return ""
	}
	lines := make([]string, len(s.findings))
	for i, f := range s.findings {
		content := sanitizeForPrompt(f.Content, 500)
		lines[i] = fmt.Sprintf("- [derived] [%s] %s\n", f.CreatedLabel, content)
	}
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

// sanitizeForPrompt removes control chars and newlines, caps
// length. Used when LLM-influenced content is embedded in the
// system prompt.
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
