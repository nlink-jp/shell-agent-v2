// Package objstore provides a central repository for images, blobs, and reports.
// Objects are stored as files with 12-char hex IDs.
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
	"time"

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
type Store struct {
	baseDir   string
	dataDir   string
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
	if err := os.MkdirAll(s.baseDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath, data, 0600)
}

// Store saves a blob and returns its metadata.
func (s *Store) Store(reader io.Reader, objType ObjectType, mimeType, origName, sessionID string) (*ObjectMeta, error) {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return nil, err
	}

	id := generateID()
	path := filepath.Join(s.dataDir, id)

	// Use 0600 to restrict access to owner only (may contain sensitive content).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, fmt.Errorf("create object: %w", err)
	}

	size, err := io.Copy(f, reader)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
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
	s.index[id] = meta

	if err := s.Save(); err != nil {
		return nil, err
	}

	return meta, nil
}

// Get returns metadata for an object.
func (s *Store) Get(id string) (*ObjectMeta, bool) {
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
	delete(s.index, id)
	return s.Save()
}

// All returns all object metadata.
func (s *Store) All() []*ObjectMeta {
	result := make([]*ObjectMeta, 0, len(s.index))
	for _, m := range s.index {
		result = append(result, m)
	}
	return result
}

// ListByType returns objects matching the given type.
func (s *Store) ListByType(objType ObjectType) []*ObjectMeta {
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
		return s.Save()
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
	if strings.HasPrefix(header, "data:") {
		mimeType = strings.TrimPrefix(header, "data:")
		mimeType = strings.TrimSuffix(mimeType, ";base64")
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

func generateID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
