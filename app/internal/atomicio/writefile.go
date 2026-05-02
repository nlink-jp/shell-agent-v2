// Package atomicio provides crash-safe file writes shared by the
// per-data-dir JSON stores (objstore index, findings, pinned memory,
// summary cache). All-or-nothing semantics: a reader sees either the
// previous file or the fully-written new file, never a partial /
// torn file produced by an interrupted write.
//
// Implementation: write to a sibling tempfile, fsync the file, rename
// over the destination, then fsync the parent directory so the
// rename is durable across power-fail. The tempfile lives in the
// same directory as the destination so the rename is guaranteed to
// be atomic on the same filesystem.
//
// See security-hardening-2.md C4 / H10.
package atomicio

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path via the tmp+rename pattern.
// On success the file at path has the supplied contents and the
// requested permission bits. On error path is left untouched.
//
// perm is applied to the destination via os.Chmod after the rename
// because os.CreateTemp does not honour a perm argument; the temp
// file is created with 0600 by default. This matches the behaviour
// of os.WriteFile.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// CreateTemp ensures we don't clobber an existing tempfile. The
	// "*" placeholder turns into a random suffix.
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicio: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before rename.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicio: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicio: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomicio: close: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		cleanup()
		return fmt.Errorf("atomicio: chmod: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("atomicio: rename: %w", err)
	}

	// Fsync the parent directory so the rename itself survives a
	// power loss. On macOS APFS this is a no-op for all practical
	// purposes (the FS journals metadata changes), but it is
	// required on ext4 / XFS for the rename to reach disk before
	// the next operation. Failures here are logged-but-ignored
	// because the rename has already succeeded.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
