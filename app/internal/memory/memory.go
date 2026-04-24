// Package memory manages Hot/Warm/Cold message tiers, sessions, and pinned memory.
package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Tier identifies a memory tier.
type Tier string

const (
	TierHot  Tier = "hot"
	TierWarm Tier = "warm"
	TierCold Tier = "cold"
)

// Record is a single memory entry in a session.
type Record struct {
	Timestamp    time.Time  `json:"timestamp"`
	Role         string     `json:"role"`
	Content      string     `json:"content"`
	Tier         Tier       `json:"tier"`
	SummaryRange *TimeRange `json:"summary_range,omitempty"`
}

// TimeRange represents a time span for Warm/Cold summaries.
type TimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Session holds the conversation state for a single chat session.
type Session struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Records []Record `json:"records"`
}

// SessionDir returns the directory for a given session.
func SessionDir(sessionID string) string {
	return filepath.Join(config.DataDir(), "sessions", sessionID)
}

// ChatPath returns the path to a session's chat file.
func ChatPath(sessionID string) string {
	return filepath.Join(SessionDir(sessionID), "chat.json")
}

// LoadSession reads a session from disk.
func LoadSession(sessionID string) (*Session, error) {
	data, err := os.ReadFile(ChatPath(sessionID))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save writes the session to disk.
func (s *Session) Save() error {
	dir := SessionDir(s.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ChatPath(s.ID), data, 0600)
}
