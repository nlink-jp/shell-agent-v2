// Package pathfix augments PATH at process start so subprocesses
// (the sandbox engine, MCP guardians, bundled tools) can find
// binaries that the macOS launchd-inherited PATH would otherwise
// hide.
//
// When the .app bundle is launched from Finder, the inherited PATH
// is the launchd default (roughly /usr/bin:/bin:/usr/sbin:/sbin),
// which excludes Homebrew (/opt/homebrew/bin on Apple Silicon,
// /usr/local/bin on Intel) and the user's ~/bin. Anything we shell
// out to that lives in those locations — podman, docker, gh, etc.
// — silently fails to resolve.
//
// Augment() prepends the well-known prefixes (only ones that exist
// on disk and aren't already in PATH) to the current PATH and
// returns the new value, ready to be passed back to os.Setenv.
package pathfix

import (
	"os"
	"path/filepath"
	"strings"
)

// Candidates returns the macOS-typical user-controlled bin
// directories in priority order. Splitting this out of Augment
// makes the merging logic easy to unit-test against fake home
// directories.
func Candidates(home string) []string {
	c := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home != "" {
		c = append(c, filepath.Join(home, "bin"), filepath.Join(home, "go", "bin"))
	}
	return c
}

// Augment returns a PATH value with each of `candidates` prepended,
// skipping entries that don't exist on disk or are already present
// in `current`. The returned string is colon-separated; pass it
// directly to os.Setenv("PATH", ...).
func Augment(current string, candidates []string, exists func(string) bool) string {
	if exists == nil {
		exists = func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		}
	}
	have := map[string]bool{}
	for p := range strings.SplitSeq(current, ":") {
		if p != "" {
			have[p] = true
		}
	}
	prefix := []string{}
	for _, c := range candidates {
		if have[c] || !exists(c) {
			continue
		}
		prefix = append(prefix, c)
		have[c] = true
	}
	if len(prefix) == 0 {
		return current
	}
	if current == "" {
		return strings.Join(prefix, ":")
	}
	return strings.Join(prefix, ":") + ":" + current
}
