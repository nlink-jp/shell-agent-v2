// Package objstore provides a central repository for images and blobs.
// Objects are stored as files with 12-char hex IDs.
package objstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// ObjectMeta holds metadata for a stored object.
type ObjectMeta struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	OrigName string `json:"orig_name"`
	Size     int64  `json:"size"`
}

// Store manages binary objects on disk.
type Store struct {
	baseDir  string
	dataDir  string
	index    map[string]*ObjectMeta
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
func (s *Store) Store(reader io.Reader, mimeType, origName string) (*ObjectMeta, error) {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return nil, err
	}

	id := generateID()
	path := filepath.Join(s.dataDir, id)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create object: %w", err)
	}

	size, err := io.Copy(f, reader)
	f.Close()
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("write object: %w", err)
	}

	meta := &ObjectMeta{
		ID:       id,
		MimeType: mimeType,
		OrigName: origName,
		Size:     size,
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

func generateID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
