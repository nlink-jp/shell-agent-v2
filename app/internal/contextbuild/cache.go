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

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// SummaryEntry is a single cached summary covering a contiguous range
// of records, scoped to a particular summarizer (backend+model).
type SummaryEntry struct {
	RangeKey      string    `json:"range_key"`
	SummarizerID  string    `json:"summarizer_id"`
	FromTimestamp time.Time `json:"from_timestamp"`
	ToTimestamp   time.Time `json:"to_timestamp"`
	RecordCount   int       `json:"record_count"`
	Summary       string    `json:"summary"`
	CreatedAt     time.Time `json:"created_at"`
}

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
// scoped to a summarizer. Used as the cache key.
//
// Including a content hash means a record edit (redaction, manual fix-up)
// invalidates the cache automatically.
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

// Get returns the cached entry for the given key, or nil.
func (c *SummaryCache) Get(key string) *SummaryEntry {
	if c == nil {
		return nil
	}
	for i := range c.Entries {
		if c.Entries[i].RangeKey == key {
			return &c.Entries[i]
		}
	}
	return nil
}

// Put inserts (or replaces) an entry. FIFO eviction when over MaxItems.
func (c *SummaryCache) Put(e SummaryEntry) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	for i := range c.Entries {
		if c.Entries[i].RangeKey == e.RangeKey {
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

// Save writes the cache to disk. No-op if there are no entries (avoids
// littering empty files).
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
	return os.WriteFile(cachePath(sessionID), data, 0600)
}

func cachePath(sessionID string) string {
	return filepath.Join(memory.SessionDir(sessionID), "summaries.json")
}
