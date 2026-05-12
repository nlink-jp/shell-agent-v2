// Package sessionio packages a session's on-disk state into a single
// portable .shellagent bundle and reads such bundles back. The
// bundle format and import/export semantics are specified in
// docs/en/adr/0001-session-import-export.md (and the JA mirror).
//
// This file defines the manifest format. The manifest is the first
// file read on import and gates compatibility checks before any
// other data is touched.
package sessionio

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is the bundle schema version this build writes and
// is the only version it accepts on import. Bumping this is a
// breaking change for older builds and requires migration logic
// that v0.4.0 explicitly does not ship (see design §9).
const SchemaVersion = 1

// Manifest is the JSON structure stored as `manifest.json` at the
// root of every .shellagent bundle. Fields mirror design §3.3.
type Manifest struct {
	SchemaVersion        int         `json:"schema_version"`
	ExportedAt           time.Time   `json:"exported_at"`
	ExportedByAppVersion string      `json:"exported_by_app_version"`
	Session              SessionMeta `json:"session"`
}

// SessionMeta carries the bundle's session-level identity: the
// original ID (preserved for traceability), the title (basis for
// the imported session's title with collision suffixing), and the
// privacy flag (carried verbatim into the imported session).
type SessionMeta struct {
	OriginalID string `json:"original_id"`
	Title      string `json:"title"`
	Private    bool   `json:"private"`
}

// ErrUnsupportedSchemaVersion is returned by UnmarshalManifest when
// the bundle's schema_version is not the value this build expects.
// Importers should surface this directly to the user rather than
// trying to coerce — design §3.3 rule 3.
var ErrUnsupportedSchemaVersion = errors.New("sessionio: unsupported bundle schema version")

// MarshalManifest serialises the manifest as pretty-printed JSON.
// Pretty-printing matters because users may inspect bundles with
// `unzip -p` to diagnose issues; a one-liner blob is hostile.
func MarshalManifest(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("sessionio: nil manifest")
	}
	return json.MarshalIndent(m, "", "  ")
}

// UnmarshalManifest parses the manifest bytes and validates the
// schema version. Returns ErrUnsupportedSchemaVersion (wrapped with
// the actual value) if the bundle was written by an incompatible
// build.
func UnmarshalManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("sessionio: parse manifest: %w", err)
	}
	if m.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedSchemaVersion, m.SchemaVersion, SchemaVersion)
	}
	return &m, nil
}
