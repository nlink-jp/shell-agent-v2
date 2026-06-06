package contextbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// SummaryEntry is a single cached summary covering a contiguous range
// of records, scoped to a particular summarizer (backend+model).
//
// ADR-0032: Kind and Tier were added to distinguish the legacy
// range-keyed entries from the new content-hashed two-tier entries.
// On-disk schema is forward-compatible: legacy entries (Kind=="")
// load fine but are skipped by Get in the v2 flow and eventually
// cleaned up by FIFO eviction.
type SummaryEntry struct {
	RangeKey      string    `json:"range_key"` // legacy field name; v2 stores ComputeContentKey output
	Kind          string    `json:"kind,omitempty"`
	Tier          string    `json:"tier,omitempty"`
	SummarizerID  string    `json:"summarizer_id"`
	FromTimestamp time.Time `json:"from_timestamp,omitzero"`
	ToTimestamp   time.Time `json:"to_timestamp,omitzero"`
	RecordCount   int       `json:"record_count"`
	Summary       string    `json:"summary"`
	CreatedAt     time.Time `json:"created_at,omitzero"`
}

// SummaryEntryKindContentV2 marks an entry as written under the
// ADR-0032 content-hash schema. Get only matches entries with this
// kind in the v2 flow.
const SummaryEntryKindContentV2 = "content_v2"

// SummaryCache is a per-session cache of summaries.
//
// Stored as JSON at sessions/<id>/summaries.json — separate from chat.json
// to keep the active conversation file lean.
type SummaryCache struct {
	Entries  []SummaryEntry `json:"entries"`
	MaxItems int            `json:"-"` // 0 -> defaultMaxItems
}

const defaultMaxItems = 64

// ComputeRangeKey produces a content-stable hash for a range of records
// scoped to a summarizer. Used as the cache key in the pre-ADR-0032
// single-tier flow.
//
// Including a content hash means a record edit (redaction, manual fix-up)
// invalidates the cache automatically.
//
// Deprecated: ADR-0032 replaces the range-based summarisation with
// content-hashed tiered summaries (ComputeContentKey). This function
// is kept temporarily to support backward-compat tests during the
// transition; new code should use ComputeContentKey.
func ComputeRangeKey(records []memory.Record, summarizerID string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|", summarizerID, len(records))
	if len(records) > 0 {
		fmt.Fprintf(h, "%d|%d|", records[0].Timestamp.UnixNano(), records[len(records)-1].Timestamp.UnixNano())
	}
	inner := sha256.New()
	for _, r := range records {
		fmt.Fprintf(inner, "%s\x00%s\x00%s\x00", r.Role, r.ToolName, r.Content)
	}
	h.Write(inner.Sum(nil))
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeContentKey returns a content-hash cache key for ADR-0032
// two-tier summarisation. The key is stable across turn additions
// that don't change the tier's input but invalidates when any of
// the listed inputs change.
//
// Inputs (all sorted internally for determinism so the caller does
// not need to pre-sort):
//   - records: the tier's input records, after dead-topic drop and
//     anchor lift. Their Role + ToolName + Content (timestamps
//     excluded — they would force a miss on every turn) contribute
//     to the inner hash.
//   - summarizerID: backend / model identifier.
//   - deadFingerprints: stable identifiers for the dropped session-
//     memory facts; when the dead set changes (e.g. a topic goes
//     dormant or revives), affected tiers' keys change and the
//     summary regenerates.
//   - anchorIndices: 0-based positions of anchor records relative
//     to the pre-lift candidate slice; changing the anchor set
//     invalidates a tier's cache.
//   - tier: "near" or "far". Independent cache slots per tier.
func ComputeContentKey(records []memory.Record, summarizerID string, deadFingerprints []string, anchorIndices []int, tier string) string {
	recH := sha256.New()
	for _, r := range records {
		fmt.Fprintf(recH, "%s\x00%s\x00%s\x00", r.Role, r.ToolName, r.Content)
	}

	deadSorted := append([]string(nil), deadFingerprints...)
	sort.Strings(deadSorted)
	deadH := sha256.New()
	for _, d := range deadSorted {
		fmt.Fprintf(deadH, "%s\x00", d)
	}

	anchorSorted := append([]int(nil), anchorIndices...)
	sort.Ints(anchorSorted)
	anchorH := sha256.New()
	for _, a := range anchorSorted {
		fmt.Fprintf(anchorH, "%d\x00", a)
	}

	outer := sha256.New()
	fmt.Fprintf(outer, "%s|", summarizerID)
	outer.Write(recH.Sum(nil))
	outer.Write(deadH.Sum(nil))
	outer.Write(anchorH.Sum(nil))
	fmt.Fprintf(outer, "|%s", tier)
	return hex.EncodeToString(outer.Sum(nil))
}

// Get returns the cached entry for the given key, or nil.
//
// ADR-0032: in the v2 flow, only entries marked with
// Kind=="content_v2" are eligible for matching. Legacy entries
// (Kind=="") are kept on disk for the FIFO eviction story but are
// never returned, so a downgrade-then-upgrade does not surface a
// stale summary.
func (c *SummaryCache) Get(key string) *SummaryEntry {
	if c == nil {
		return nil
	}
	for i := range c.Entries {
		e := &c.Entries[i]
		if e.Kind != SummaryEntryKindContentV2 {
			continue
		}
		if e.RangeKey == key {
			return e
		}
	}
	return nil
}

// Put inserts (or replaces) an entry. FIFO eviction when over MaxItems.
// ADR-0032: stamps Kind="content_v2" on every entry — the v2 flow is
// the only writer and we want legacy entries (Kind=="") to remain
// distinguishable for the Get-side skip logic.
func (c *SummaryCache) Put(e SummaryEntry) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.Kind == "" {
		e.Kind = SummaryEntryKindContentV2
	}
	for i := range c.Entries {
		if c.Entries[i].RangeKey == e.RangeKey && c.Entries[i].Kind == e.Kind {
			c.Entries[i] = e
			return
		}
	}
	c.Entries = append(c.Entries, e)
	limit := c.MaxItems
	if limit <= 0 {
		limit = defaultMaxItems
	}
	if len(c.Entries) > limit {
		sort.SliceStable(c.Entries, func(i, j int) bool {
			return c.Entries[i].CreatedAt.Before(c.Entries[j].CreatedAt)
		})
		c.Entries = c.Entries[len(c.Entries)-limit:]
	}
}

// LoadCache reads a session's summary cache from disk. A missing file
// yields an empty cache.
func LoadCache(sessionID string) (*SummaryCache, error) {
	path := cachePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &SummaryCache{}, nil
		}
		return nil, err
	}
	var c SummaryCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the cache to disk atomically (tmp+rename) so a torn
// summaries.json never reaches the next contextbuild call. No-op if
// there are no entries (avoids littering empty files).
// Security-hardening-2.md C4.
func (c *SummaryCache) Save(sessionID string) error {
	if c == nil || len(c.Entries) == 0 {
		return nil
	}
	dir := memory.SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(cachePath(sessionID), data, 0600)
}

func cachePath(sessionID string) string {
	return filepath.Join(memory.SessionDir(sessionID), "summaries.json")
}
