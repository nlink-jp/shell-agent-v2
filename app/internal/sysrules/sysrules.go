// Package sysrules persists the user-authored System Rules
// Markdown document. The document is injected near the top of the
// LLM system prompt at every turn; see ADR-0012 and
// docs/{en,ja}/reference/system-rules*.md.
//
// Concurrency: the Store has no internal mutex. Agent.mu
// serialises all access from the bindings layer (Get/Save) and
// the turn loop reads the value as a snapshot through the agent.
// This matches the GlobalMemoryStore pattern in
// internal/memory/global_memory.go.
package sysrules

import (
	"errors"
	"os"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/atomicio"
	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Store holds the in-memory cache of the System Rules document
// and persists it to a single Markdown file under the application
// data directory.
type Store struct {
	path    string
	content string
}

// NewStore returns a Store backed by config.SystemRulesPath().
func NewStore() *Store {
	return &Store{path: config.SystemRulesPath()}
}

// NewStoreAt returns a Store backed by the supplied path. Used by
// tests to redirect storage to a temp directory.
func NewStoreAt(path string) *Store {
	return &Store{path: path}
}

// Path returns the on-disk location of the rules file. Useful for
// the Settings UI to surface where edits land.
func (s *Store) Path() string {
	return s.path
}

// Load reads the rules file into the in-memory cache. A missing
// file is treated as an empty document (no error).
func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.content = ""
			return nil
		}
		return err
	}
	s.content = normalize(string(data))
	return nil
}

// Save normalises the supplied content (CRLF→LF, single trailing
// newline) and atomically writes it to disk. An empty document
// writes an empty file (so "explicitly cleared" is durable).
func (s *Store) Save(content string) error {
	if err := os.MkdirAll(parentDir(s.path), 0700); err != nil {
		return err
	}
	normalised := normalize(content)
	if err := atomicio.WriteFileAtomic(s.path, []byte(normalised), 0600); err != nil {
		return err
	}
	s.content = normalised
	return nil
}

// Get returns the in-memory cached content. Callers must NOT
// mutate the returned string (Go strings are immutable, this is
// a reminder for future maintainers).
func (s *Store) Get() string {
	return s.content
}

// normalize folds CRLF to LF and pins exactly one trailing newline
// when the content is non-empty. An empty input stays empty.
func normalize(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

func parentDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}
