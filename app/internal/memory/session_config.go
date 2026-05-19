// Package memory — session.json: per-session configuration state.
//
// session.json sits alongside chat.json in each session directory.
// Where chat.json holds the conversation transcript (records, title,
// private flag), session.json holds the orthogonal configuration
// state — currently just the profile_id binding (ADR-0016 §3.2).
// Separating the two keeps chat.json untouched by configuration
// changes and gives a clean extension point for future session-level
// state (UI prefs, pinned tools, …) without growing chat.json.
//
// Schema (v1):
//
//	{
//	  "schema_version": 1,
//	  "profile_id": "<uuid>"
//	}
//
// A missing session.json is NOT an error — it means this session
// was created under v0.11.x (or hasn't been saved yet) and the
// caller should treat it as referencing the default profile. The
// caller is also responsible for lazy-writing a fresh session.json
// on first load to migrate v0.11.x sessions in place (§3.3 step 2a).
package memory

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
)

// SessionConfigSchemaVersion is the on-disk schema version for
// session.json. v1 is the only version this codebase writes; readers
// must accept higher versions only if all required fields parse.
const SessionConfigSchemaVersion = 1

// SessionConfig holds per-session configuration state persisted to
// session.json. Today it carries the profile binding only; future
// ADRs may add fields (bumping SchemaVersion).
type SessionConfig struct {
	SchemaVersion int    `json:"schema_version"`
	ProfileID     string `json:"profile_id"`
}

// SessionConfigPath returns the path to a session's session.json.
func SessionConfigPath(sessionID string) string {
	return filepath.Join(SessionDir(sessionID), "session.json")
}

// LoadSessionConfig reads session.json for the given session.
//
// Returns (cfg, true, nil) on a successful read.
// Returns (zero, false, nil) when the file does not exist — this is
// the normal path for v0.11.x sessions and for sessions whose
// initial save has not happened yet. Callers fall back to the
// default profile and should call SaveSessionConfig once they've
// resolved the profile_id.
// Returns (zero, false, err) for any other I/O or parse error.
func LoadSessionConfig(sessionID string) (SessionConfig, bool, error) {
	data, err := os.ReadFile(SessionConfigPath(sessionID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SessionConfig{}, false, nil
		}
		return SessionConfig{}, false, err
	}
	var cfg SessionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return SessionConfig{}, false, err
	}
	return cfg, true, nil
}

// SaveSessionConfig writes session.json atomically (tmp+rename) so a
// crash mid-save can't leave a torn file. The schema_version is
// stamped to SessionConfigSchemaVersion regardless of the caller's
// input so we always persist a self-describing record.
func SaveSessionConfig(sessionID string, cfg SessionConfig) error {
	dir := SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	cfg.SchemaVersion = SessionConfigSchemaVersion
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(SessionConfigPath(sessionID), data, 0600)
}
