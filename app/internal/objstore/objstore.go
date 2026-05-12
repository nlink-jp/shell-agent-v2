// Package objstore provides a central repository for images, blobs, and reports.
// Objects are stored as files with hex IDs (16 bytes / 32 hex chars
// for new objects; legacy 12-hex IDs continue to load via the
// length-tolerant read path).
// Design: docs/en/history/object-storage.md
package objstore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// ObjectType identifies the kind of stored object.
type ObjectType string

const (
	TypeImage    ObjectType = "image"
	TypeBlob     ObjectType = "blob"
	TypeReport   ObjectType = "report"
	TypeMarkdown ObjectType = "markdown" // v0.5: user-attached markdown / plain text
)

// ObjectMeta holds metadata for a stored object.
//
// Lines and Tokens are populated by Store() for text/* MIME types
// (and backfilled by Load() for pre-v0.5 records). They allow
// list-objects to surface document size without forcing the LLM
// to copy the content into the sandbox just to run `wc -l`.
// omitempty keeps the JSON forward/backward compatible.
type ObjectMeta struct {
	ID        string     `json:"id"`
	Type      ObjectType `json:"type"`
	MimeType  string     `json:"mime_type"`
	OrigName  string     `json:"orig_name"`
	CreatedAt time.Time  `json:"created_at"`
	SessionID string     `json:"session_id,omitempty"`
	Size      int64      `json:"size"`
	Lines     int        `json:"lines,omitempty"`  // newline count + 1, text/* only
	Tokens    int        `json:"tokens,omitempty"` // memory.EstimateTokens cache, text/* only
}

// isTextMIME reports whether the MIME type is text-shaped and
// thus eligible for Lines/Tokens auto-fill. Mirrors the
// SaveDataURL MIME→TypeMarkdown inference rule: text/markdown
// and text/plain are the headline cases, but anything under
// text/* (text/csv, text/html, ...) is also worth measuring.
func isTextMIME(mime string) bool {
	return strings.HasPrefix(mime, "text/")
}

// Store manages binary objects on disk.
//
// Concurrency: the index map is guarded by mu. Read methods
// (Get, All, ListBySession, ListByType) take RLock; mutating
// methods (Store, Delete, DeleteBySession, Save, Load) take
// Lock. ReadData / DataPath access the filesystem directly by
// id and don't need the lock — a file removed mid-Open
// surfaces as a normal os error.
type Store struct {
	baseDir   string
	dataDir   string
	mu        sync.RWMutex
	index     map[string]*ObjectMeta
	indexPath string
	// dirty is set when Load() back-fills missing Lines/Tokens
	// for pre-v0.5 text objects; Load flushes the updated index
	// via saveLocked() before returning. Subsequent reads see
	// dirty=false because the persisted index already has the
	// computed values.
	dirty bool
}

// NewStore creates a store at the default location.
func NewStore() *Store {
	base := filepath.Join(config.DataDir(), "objects")
	return NewStoreAt(base)
}

// NewStoreAt creates a store at the given directory.
func NewStoreAt(baseDir string) *Store {
	return &Store{
		baseDir:   baseDir,
		dataDir:   filepath.Join(baseDir, "data"),
		indexPath: filepath.Join(baseDir, "index.json"),
		index:     make(map[string]*ObjectMeta),
	}
}

// Load reads the index from disk.
//
// On first launch after v0.5, any pre-existing text-MIME object
// whose `Lines` field is zero gets its `Lines` / `Tokens`
// computed from the data file (lazy backfill). The updated index
// is persisted before Load returns so subsequent reads pay no
// cost. Missing data files are tolerated — the entry stays at
// Lines=0 and the rest of the app keeps working.
//
// This makes the v0.5 upgrade self-healing for the
// `create-report` objects users already have on disk: no
// migration UI, no user action, no permanent metadata asymmetry
// between legacy reports and new TypeMarkdown attachments.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, &s.index); err != nil {
		return err
	}
	for _, meta := range s.index {
		if meta.Lines > 0 {
			continue // already populated
		}
		if !isTextMIME(meta.MimeType) {
			continue // image/blob: nothing to compute
		}
		content, rerr := os.ReadFile(filepath.Join(s.dataDir, meta.ID))
		if rerr != nil || len(content) == 0 {
			continue // tolerate missing/empty data file
		}
		meta.Lines = bytes.Count(content, []byte{'\n'}) + 1
		meta.Tokens = memory.EstimateTokens(string(content))
		s.dirty = true
	}
	if s.dirty {
		_ = s.saveLocked()
		s.dirty = false
	}
	return nil
}

// Save writes the index to disk.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveLocked writes the index without acquiring the lock.
// Caller must already hold s.mu (Lock, not RLock — we are
// reading the map but a concurrent writer would still race).
//
// Atomic write: tmp+rename so a reader never sees a partial
// index.json after a crash mid-save (security-hardening-2.md C4 / H10).
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.baseDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.index, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteFileAtomic(s.indexPath, data, 0600)
}

// Store saves a blob and returns its metadata.
//
// Concurrency / collision: with 16-byte (128-bit) IDs the birthday
// bound makes accidental collisions astronomically improbable, but
// we still pick a fresh ID under the index lock and verify it isn't
// already present — up to 3 attempts before bailing out. This both
// future-proofs against ID-space shrinks and guards against a buggy
// crypto/rand returning all zeros (security-hardening-2.md H11).
//
// As of v0.5, Store buffers the reader's content fully into memory
// before writing so it can compute Lines/Tokens for text/* MIME
// types in a single pass. Callers must keep their input within
// reasonable bounds (SaveDataURL enforces a 50 MB cap; other
// callers (toolCreateReport, sandbox register/import) all pass
// already-in-memory data). This contract narrowing is benign
// because every existing caller already had the content in memory.
func (s *Store) Store(reader io.Reader, objType ObjectType, mimeType, origName, sessionID string) (*ObjectMeta, error) {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return nil, err
	}

	// Buffer the reader so Lines/Tokens can be computed in the
	// same pass as the disk write (avoids a second os.ReadFile
	// after the file is closed). At 50 MB cap this is at most
	// a single 50 MB allocation, freed when this function returns.
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}
	size := int64(len(content))

	s.mu.Lock()
	var id string
	for range 3 {
		candidate := generateID()
		if _, exists := s.index[candidate]; !exists {
			id = candidate
			break
		}
	}
	if id == "" {
		s.mu.Unlock()
		return nil, fmt.Errorf("objstore: could not generate unique ID after 3 attempts")
	}
	// Reserve the slot under lock so a concurrent Store can't pick
	// the same ID while we're writing the file. Set a placeholder
	// that we'll overwrite with the real meta below.
	s.index[id] = &ObjectMeta{ID: id}
	s.mu.Unlock()

	path := filepath.Join(s.dataDir, id)

	// Use 0600 to restrict access to owner only (may contain sensitive content).
	if err := os.WriteFile(path, content, 0600); err != nil {
		s.mu.Lock()
		delete(s.index, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("write object: %w", err)
	}

	meta := &ObjectMeta{
		ID:        id,
		Type:      objType,
		MimeType:  mimeType,
		OrigName:  origName,
		CreatedAt: time.Now(),
		SessionID: sessionID,
		Size:      size,
	}

	// v0.5: auto-fill Lines/Tokens for text content so list-objects
	// can surface document size to the LLM without a sandbox detour.
	// Mirrors the Load() lazy-backfill path so new writes and legacy
	// reads converge on the same metadata shape.
	if isTextMIME(mimeType) && len(content) > 0 {
		meta.Lines = bytes.Count(content, []byte{'\n'}) + 1
		meta.Tokens = memory.EstimateTokens(string(content))
	}

	s.mu.Lock()
	s.index[id] = meta
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// Get returns metadata for an object.
func (s *Store) Get(id string) (*ObjectMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.index[id]
	return meta, ok
}

// ReadData returns a reader for an object's data.
func (s *Store) ReadData(id string) (io.ReadCloser, error) {
	path := filepath.Join(s.dataDir, id)
	return os.Open(path)
}

// DataPath returns the file path for an object.
func (s *Store) DataPath(id string) string {
	return filepath.Join(s.dataDir, id)
}

// Delete removes an object and its data.
func (s *Store) Delete(id string) error {
	path := filepath.Join(s.dataDir, id)
	os.Remove(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.index, id)
	return s.saveLocked()
}

// All returns all object metadata.
func (s *Store) All() []*ObjectMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ObjectMeta, 0, len(s.index))
	for _, m := range s.index {
		result = append(result, m)
	}
	return result
}

// ListByType returns objects matching the given type.
func (s *Store) ListByType(objType ObjectType) []*ObjectMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*ObjectMeta
	for _, m := range s.index {
		if m.Type == objType {
			result = append(result, m)
		}
	}
	return result
}

// ListBySession returns objects created by the given session.
func (s *Store) ListBySession(sessionID string) []*ObjectMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*ObjectMeta
	for _, m := range s.index {
		if m.SessionID == sessionID {
			result = append(result, m)
		}
	}
	return result
}

// DeleteBySession removes all objects created by the given session.
func (s *Store) DeleteBySession(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var toDelete []string
	for id, m := range s.index {
		if m.SessionID == sessionID {
			toDelete = append(toDelete, id)
		}
	}
	for _, id := range toDelete {
		path := filepath.Join(s.dataDir, id)
		os.Remove(path)
		delete(s.index, id)
	}
	if len(toDelete) > 0 {
		return s.saveLocked()
	}
	return nil
}

// MaxAttachmentBytes caps a single SaveDataURL ingest. Larger
// payloads are rejected before the base64 decode result is handed
// to Store(). 50 MB is generous for markdown / images and well
// below the size at which the data-URL round-trip through Wails
// becomes painful. Larger content should go via sandbox-register
// -object after the user manually drops the file into /work.
const MaxAttachmentBytes = 50 * 1024 * 1024

// SaveDataURL parses a data URL and stores the binary data.
//
// origName is the user-visible filename to record on the stored
// ObjectMeta (orig_name). Pass "" when the data URL has no
// associated filename (e.g. paste-from-clipboard images). For
// drag-drop / file-picker attachments the chat input passes the
// actual filename here so the data panel and chat bubbles can
// show "audit.md" instead of the 32-hex object ID.
//
// Type inference from MIME:
//   - image/*                         → TypeImage
//   - text/markdown, text/plain       → TypeMarkdown   (v0.5)
//   - anything else                   → TypeBlob
//
// application/json deliberately stays as TypeBlob: tabular JSON
// has its own DuckDB path via load-data; non-tabular JSON as a
// document is deferred to a later release (user can wrap in a
// .md code fence today).
func (s *Store) SaveDataURL(dataURL, origName, sessionID string) (*ObjectMeta, error) {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid data URL format")
	}

	header := parts[0]
	mimeType := ""
	if rest, ok := strings.CutPrefix(header, "data:"); ok {
		mimeType = strings.TrimSuffix(rest, ";base64")
	}

	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(decoded) > MaxAttachmentBytes {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", len(decoded), MaxAttachmentBytes)
	}

	// Determine type from MIME.
	objType := TypeBlob
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		objType = TypeImage
	case mimeType == "text/markdown" || mimeType == "text/plain":
		objType = TypeMarkdown
	}

	return s.Store(strings.NewReader(string(decoded)), objType, mimeType, origName, sessionID)
}

// LoadAsDataURL reads an object and returns it as a data URL.
func (s *Store) LoadAsDataURL(id string) (string, error) {
	meta, ok := s.Get(id)
	if !ok {
		return "", fmt.Errorf("object %s not found", id)
	}

	data, err := os.ReadFile(s.DataPath(id))
	if err != nil {
		return "", err
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("data:%s;base64,%s", meta.MimeType, encoded), nil
}

// IDByteLen is the entropy width of newly-generated object IDs.
// 16 bytes → 32 hex chars → 128 bits, which makes accidental
// collisions astronomically improbable even at >1 M objects per
// store. Read-side code is length-tolerant so legacy 12-hex IDs
// continue to load (security-hardening-2.md H11).
const IDByteLen = 16

func generateID() string {
	b := make([]byte, IDByteLen)
	rand.Read(b)
	return hex.EncodeToString(b)
}
