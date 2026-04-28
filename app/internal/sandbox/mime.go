package sandbox

import (
	"path/filepath"
	"strings"
)

// extToMIME is the mini-table used by MimeFromPath. Kept small and
// deterministic on purpose: we cover the formats the LLM is realistic
// to register from /work (charts, generated CSV/JSON, scripts, simple
// reports). Anything else falls back to application/octet-stream and
// the user can specify the type explicitly when calling
// sandbox-register-object.
var extToMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",

	".csv":  "text/csv",
	".json": "application/json",
	".md":   "text/markdown",
	".txt":  "text/plain",
	".html": "text/html",
	".htm":  "text/html",
	".yaml": "application/x-yaml",
	".yml":  "application/x-yaml",

	".py":  "text/x-python",
	".sh":  "text/x-shellscript",
	".log": "text/plain",

	".pdf": "application/pdf",
}

// MimeFromPath returns the inferred MIME type for a path based on its
// extension. Returns "application/octet-stream" for unknown extensions.
func MimeFromPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if m, ok := extToMIME[ext]; ok {
		return m
	}
	return "application/octet-stream"
}

// ObjectTypeForMIME maps a MIME type to one of the objstore type
// strings used when registering a sandbox-produced file:
//
//	image/*  → "image"
//	text/markdown → "report"
//	other text/*, application/json, application/x-yaml → "blob"
//	other → "blob"
//
// Phase 2 will pass the result to objstore.ObjectType().
func ObjectTypeForMIME(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case mime == "text/markdown":
		return "report"
	default:
		return "blob"
	}
}
