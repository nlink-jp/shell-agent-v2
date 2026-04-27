// Package bundled embeds the default tool scripts and installs them
// into the user's tool directory on first launch.
//
// Behaviour: only files that don't already exist at the destination are
// written. Pre-existing files (user customizations) are never overwritten.
// This means new bundled tools added in a release ship to existing users,
// while their edits are preserved.
//
// The "examples/" subdirectory is intentionally not auto-installed —
// example scripts are reference material the user copies in deliberately.
package bundled

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed tools
var toolsFS embed.FS

const toolsRoot = "tools"

// Install copies the embedded default tool scripts into targetDir for any
// names that aren't already present. Returns the list of names installed.
func Install(targetDir string) ([]string, error) {
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return nil, err
	}
	entries, err := toolsFS.ReadDir(toolsRoot)
	if err != nil {
		return nil, err
	}
	var installed []string
	for _, e := range entries {
		if e.IsDir() {
			continue // skip examples/
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sh") {
			continue
		}
		dst := filepath.Join(targetDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // user already has this file (possibly edited)
		} else if !errors.Is(err, os.ErrNotExist) {
			return installed, err
		}
		data, err := toolsFS.ReadFile(filepath.Join(toolsRoot, name))
		if err != nil {
			return installed, err
		}
		if err := os.WriteFile(dst, data, 0700); err != nil {
			return installed, err
		}
		installed = append(installed, name)
	}
	return installed, nil
}

// List returns the names of all bundled tool scripts (for diagnostics).
func List() []string {
	entries, err := toolsFS.ReadDir(toolsRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".sh") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Open exposes the embedded fs for advanced callers (e.g., a future
// "reset to bundled" feature).
func Open(name string) (fs.File, error) {
	return toolsFS.Open(filepath.Join(toolsRoot, name))
}
