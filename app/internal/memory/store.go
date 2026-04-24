package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// SessionInfo is a lightweight session descriptor for listing.
type SessionInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

// ListSessions returns all sessions sorted by most recent first.
func ListSessions() ([]SessionInfo, error) {
	sessionsDir := filepath.Join(config.DataDir(), "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var infos []SessionInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		chatPath := filepath.Join(sessionsDir, entry.Name(), "chat.json")
		data, err := os.ReadFile(chatPath)
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}

		updatedAt := ""
		if info, err := entry.Info(); err == nil {
			updatedAt = info.ModTime().Format("2006-01-02 15:04")
		}

		infos = append(infos, SessionInfo{
			ID:        s.ID,
			Title:     s.Title,
			UpdatedAt: updatedAt,
		})
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].UpdatedAt > infos[j].UpdatedAt
	})

	return infos, nil
}

// DeleteSessionDir removes a session directory entirely.
func DeleteSessionDir(sessionID string) error {
	return os.RemoveAll(SessionDir(sessionID))
}

// RenameSession updates the title of a session on disk.
func RenameSession(sessionID, title string) error {
	s, err := LoadSession(sessionID)
	if err != nil {
		return err
	}
	s.Title = title
	return s.Save()
}
