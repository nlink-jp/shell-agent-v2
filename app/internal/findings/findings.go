// Package findings manages per-session data-analysis discoveries.
//
// v0.2.0 redesign: findings are session-scoped, not cross-session.
// Each session owns its own `sessions/<id>/findings.json`. Cross-
// session promotion is handled by the user explicitly via the
// "Pin to Global Memory" UI action, which creates a corresponding
// entry in GlobalMemoryStore. See docs/en/reference/memory-model.md §4.
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
	"unicode"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// DedupJaccardThreshold is the word-set Jaccard ratio above which
// two finding contents are treated as the same observation.
//
// Real-world LLM duplicates ("Tokyo Widget sales spiked to 99999
// on 2026-02-16" vs "On 2026-02-16 the Tokyo Widget sales hit an
// outlier value of 99999") share the load-bearing nouns and
// numbers but diverge in connectives and verbs, landing around
// 0.5–0.65 Jaccard. 0.5 was chosen empirically: it catches the
// common rewording-but-same-insight case without eating
// genuinely distinct observations on the same table (those
// usually share at most ~3 nouns and land below 0.4).
const DedupJaccardThreshold = 0.5

// Source values for Finding.Source. All findings now originate
// from analysis tools — there is no manual entry path
// (the v0.1.x `/finding` slash command was removed in v0.2.0).
const (
	SourceLLMPromoted = "llm_promoted" // promoted by the promote-finding tool
	SourceAnalyzeData = "analyze_data" // emitted by analyze-data sliding-window
	SourceAnalyzeText = "analyze_text" // emitted by analyze-text sliding-window (v0.5)
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

// Add appends a new finding. Returns nil when the content matches
// (or is too similar to) an existing finding so the caller can
// surface a meaningful "already recorded" message.
//
// ID format: f-YYYYMMDD-NNN for the first 999 findings of any
// calendar day; if exceeded, fall back to f-YYYYMMDD-NNNNNN-<6 hex>
// so the ID stays unique without colliding with the legacy
// fixed-width format.
func (s *Store) Add(content string, tags []string, source string, toolOriginated bool) *Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isDuplicateLocked(content) {
		return nil
	}
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

// isDuplicateLocked reports whether the given content describes
// the same observation as an existing finding. Three layers:
//   1. exact equality
//   2. normalised equality (lowercased, whitespace collapsed,
//      punctuation stripped) — catches whitespace / case noise
//   3. word-set Jaccard ≥ DedupJaccardThreshold — catches the
//      "same observation, slightly different wording" case the
//      LLM produces when the user asks promote-finding after
//      analyze-data already auto-promoted the same insight
//
// Caller must hold s.mu.
func (s *Store) isDuplicateLocked(content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	norm := normaliseFindingText(content)
	tokens := tokeniseFinding(content)
	for _, existing := range s.findings {
		if existing.Content == content {
			return true
		}
		if normaliseFindingText(existing.Content) == norm {
			return true
		}
		if jaccard(tokens, tokeniseFinding(existing.Content)) >= DedupJaccardThreshold {
			return true
		}
	}
	return false
}

// normaliseFindingText lowercases the input, replaces any
// non-letter / non-digit run with a single space, and trims.
// Used by the layer-2 dedup check.
func normaliseFindingText(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevSpace := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevSpace = false
		} else if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// tokeniseFinding turns the input into a set of comparable
// tokens for the layer-3 dedup check. ASCII letter/digit runs
// of length ≥3 become a single token; CJK runs are windowed
// into 3-character n-grams (mirrors extractCJKNgrams in agent
// but kept independent so findings doesn't depend on agent).
func tokeniseFinding(s string) map[string]struct{} {
	out := map[string]struct{}{}
	s = strings.ToLower(s)
	var ascii strings.Builder
	flushAscii := func() {
		if ascii.Len() >= 3 {
			out[ascii.String()] = struct{}{}
		}
		ascii.Reset()
	}
	var cjk []rune
	flushCJK := func() {
		if len(cjk) >= 3 {
			for i := 0; i+3 <= len(cjk); i++ {
				out[string(cjk[i:i+3])] = struct{}{}
			}
		}
		cjk = nil
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushAscii()
			cjk = append(cjk, r)
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			flushCJK()
			ascii.WriteRune(r)
		default:
			flushAscii()
			flushCJK()
		}
	}
	flushAscii()
	flushCJK()
	return out
}

func isCJK(r rune) bool {
	// Hiragana, Katakana, CJK Unified Ideographs (basic + ext A),
	// CJK Symbols/Punctuation, full-width forms.
	return (r >= 0x3000 && r <= 0x303F) || // CJK Symbols
		(r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Ext A
		(r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified
		(r >= 0xFF00 && r <= 0xFFEF) // Halfwidth/Fullwidth
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
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
