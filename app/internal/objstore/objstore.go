// Package objstore provides a central repository for images, blobs, and reports.
// Objects are stored as files with hex IDs (16 bytes / 32 hex chars
// for new objects; legacy 12-hex IDs continue to load via the
// length-tolerant read path).
// Design: docs/en/object-storage.md
package objstore

import (
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
)

// ObjectType identifies the kind of stored object.
type ObjectType string

const (
	TypeImage  ObjectType = "image"
	TypeBlob   ObjectType = "blob"
	TypeReport ObjectType = "report"
)

// ObjectMeta holds metadata for a stored object.
type ObjectMeta struct {
	ID        string     `json:"id"`
	Type      ObjectType `json:"type"`
	MimeType  string     `json:"mime_type"`
	OrigName  string     `json:"orig_name"`
	CreatedAt time.Time  `json:"created_at"`
	SessionID string     `json:"session_id,omitempty"`
	Size      int64      `json:"size"`
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
	return json.Unmarshal(data, &s.index)
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
func (s *Store) Store(reader io.Reader, objType ObjectType, mimeType, origName, sessionID string) (*ObjectMeta, error) {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return nil, err
	}

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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		s.mu.Lock()
		delete(s.index, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("create object: %w", err)
	}

	size, err := io.Copy(f, reader)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
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

// SaveDataURL parses a data URL and stores the binary data.
func (s *Store) SaveDataURL(dataURL, sessionID string) (*ObjectMeta, error) {
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

	// Determine type from MIME
	objType := TypeBlob
	if strings.HasPrefix(mimeType, "image/") {
		objType = TypeImage
	}

	return s.Store(strings.NewReader(string(decoded)), objType, mimeType, "", sessionID)
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
