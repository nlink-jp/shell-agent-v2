// export.go: write a session directory plus its objstore objects
// to a .shellagent ZIP bundle.
//
// Atomicity: the bundle is written to a sibling temp file and
// renamed onto the destination at the end. A crash mid-write
// leaves a partial temp file (which the user can delete) but the
// destination either does not exist or contains the previous
// successful bundle — never a partial one.
//
// File ordering inside the ZIP: manifest.json is written first so
// `unzip -l` on a partial bundle still surfaces version info, and
// the importer can read the manifest before allocating any other
// state.

package sessionio

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionFiles enumerates the per-session artifacts that may live
// in `sessions/<id>/` and should be packaged when present. Order
// matters only for cosmetics inside the zip (`unzip -l` output);
// validation is by name on import.
var sessionFiles = []string{
	"chat.json",            // required (validated on import)
	"session_memory.json",  // optional; absence = empty store
	"findings.json",        // optional; absence = empty store
	"summaries.json",       // optional
	"analysis.duckdb",      // optional; only after first analysis use
}

// ExportSession packages a session directory and its objstore
// objects into a .shellagent bundle at dest.
//
// srcDir is the on-disk session directory (e.g. SessionDir(id)).
// meta is the manifest to write at the bundle root; the caller
// owns its construction so per-app-version fields stay accurate.
// objects is the list of objstore objects to bundle (can be nil).
//
// Returns the bundle size in bytes (handy for audit logging) and
// any error from the I/O path. On error the temp file is removed.
func ExportSession(srcDir string, dest string, meta *Manifest, objects []ObjectExport) (int64, error) {
	if meta == nil {
		return 0, errors.New("sessionio: nil manifest")
	}
	if srcDir == "" || dest == "" {
		return 0, errors.New("sessionio: empty srcDir or dest")
	}

	// Temp file in the same directory so os.Rename is atomic
	// (cross-device renames fall back to copy+remove which is
	// not atomic).
	destDir := filepath.Dir(dest)
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return 0, fmt.Errorf("sessionio: mkdir dest: %w", err)
	}
	tmp, err := os.CreateTemp(destDir, ".shellagent-export-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("sessionio: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Defensive: ensure the temp file is removed if anything below
	// fails before the final rename.
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	zw := zip.NewWriter(tmp)
	if err := writeBundle(zw, srcDir, meta, objects); err != nil {
		_ = zw.Close()
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, fmt.Errorf("sessionio: close zip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("sessionio: sync temp: %w", err)
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("sessionio: stat temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("sessionio: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return 0, fmt.Errorf("sessionio: rename: %w", err)
	}
	cleanup = false
	return size, nil
}

func writeBundle(zw *zip.Writer, srcDir string, meta *Manifest, objects []ObjectExport) error {
	// 1. manifest.json — first entry so partial-bundle inspection
	//    still surfaces version info.
	manifestBytes, err := MarshalManifest(meta)
	if err != nil {
		return fmt.Errorf("sessionio: marshal manifest: %w", err)
	}
	if err := writeZipFile(zw, "manifest.json", manifestBytes, time.Now()); err != nil {
		return err
	}

	// 2. Per-session artifacts that exist on disk.
	for _, name := range sessionFiles {
		path := filepath.Join(srcDir, name)
		if err := copyFileIntoZip(zw, path, name); err != nil {
			return err
		}
	}

	// 3. work/ directory — recursive walk if present.
	workRoot := filepath.Join(srcDir, "work")
	if err := walkWorkIntoZip(zw, workRoot); err != nil {
		return err
	}

	// 4. objstore objects — index.json then each blob.
	if len(objects) > 0 {
		if err := writeObjectsIntoZip(zw, objects); err != nil {
			return err
		}
	}
	return nil
}

// copyFileIntoZip adds the file at srcPath into the zip under
// archiveName. A missing file is silently skipped — these per-
// session artifacts are lazily created and a brand-new session may
// only have chat.json. The importer treats missing as "empty store".
func copyFileIntoZip(zw *zip.Writer, srcPath, archiveName string) error {
	st, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sessionio: stat %s: %w", archiveName, err)
	}
	// Refuse symlinks. Sessions shouldn't contain them and
	// following one risks pulling outside the session dir.
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sessionio: refuse to follow symlink: %s", archiveName)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("sessionio: open %s: %w", archiveName, err)
	}
	defer src.Close()
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name:     archiveName,
		Method:   zip.Deflate,
		Modified: st.ModTime(),
	})
	if err != nil {
		return fmt.Errorf("sessionio: zip header %s: %w", archiveName, err)
	}
	if _, err := io.Copy(w, src); err != nil {
		return fmt.Errorf("sessionio: write %s: %w", archiveName, err)
	}
	return nil
}

func walkWorkIntoZip(zw *zip.Writer, workRoot string) error {
	st, err := os.Stat(workRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sessionio: stat work: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("sessionio: work/ is not a directory")
	}
	return filepath.Walk(workRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == workRoot {
			return nil
		}
		// Refuse symlinks anywhere in the tree.
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sessionio: refuse symlink in work/: %s", path)
		}
		rel, err := filepath.Rel(workRoot, path)
		if err != nil {
			return err
		}
		archive := "work/" + filepath.ToSlash(rel)
		if info.IsDir() {
			// Encode directory entries explicitly so empty dirs
			// survive the round-trip. Trailing slash signals dir
			// to zip readers.
			_, err := zw.CreateHeader(&zip.FileHeader{
				Name:     archive + "/",
				Method:   zip.Store,
				Modified: info.ModTime(),
			})
			return err
		}
		return copyFileIntoZip(zw, path, archive)
	})
}

func writeObjectsIntoZip(zw *zip.Writer, objects []ObjectExport) error {
	entries := make([]objectIndexEntry, 0, len(objects))
	for _, o := range objects {
		if o.Meta == nil {
			return errors.New("sessionio: nil ObjectExport.Meta")
		}
		entries = append(entries, objectIndexEntry{
			ID:        o.Meta.ID,
			Type:      o.Meta.Type,
			MimeType:  o.Meta.MimeType,
			OrigName:  o.Meta.OrigName,
			CreatedAt: o.Meta.CreatedAt.UTC().Format(time.RFC3339Nano),
			Size:      o.Meta.Size,
		})
	}
	indexBytes, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("sessionio: marshal objects index: %w", err)
	}
	if err := writeZipFile(zw, "objects/index.json", indexBytes, time.Now()); err != nil {
		return err
	}
	for _, o := range objects {
		archive := "objects/data/" + o.Meta.ID
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     archive,
			Method:   zip.Deflate,
			Modified: o.Meta.CreatedAt,
		})
		if err != nil {
			return fmt.Errorf("sessionio: zip header %s: %w", archive, err)
		}
		rc, err := o.Open()
		if err != nil {
			return fmt.Errorf("sessionio: open object %s: %w", o.Meta.ID, err)
		}
		_, copyErr := io.Copy(w, rc)
		closeErr := rc.Close()
		if copyErr != nil {
			return fmt.Errorf("sessionio: write object %s: %w", o.Meta.ID, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("sessionio: close object %s: %w", o.Meta.ID, closeErr)
		}
	}
	return nil
}

func writeZipFile(zw *zip.Writer, name string, data []byte, mod time.Time) error {
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: mod,
	})
	if err != nil {
		return fmt.Errorf("sessionio: zip header %s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("sessionio: write %s: %w", name, err)
	}
	return nil
}

// SafeBundleFilename produces the default filename proposed by the
// save dialog (design §4.5). Disallowed FS characters and ASCII
// control codes are replaced with `_`; the title is truncated to
// 64 chars; an empty title falls back to `session-<short-id>`.
func SafeBundleFilename(title, sessionID string, when time.Time) string {
	clean := sanitizeTitle(title)
	if clean == "" {
		short := sessionID
		if len(short) > 8 {
			short = short[:8]
		}
		clean = "session-" + short
	}
	return fmt.Sprintf("%s-%s.shellagent", clean, when.Format("20060102-150405"))
}

func sanitizeTitle(t string) string {
	var b strings.Builder
	for _, r := range t {
		switch {
		case r < 0x20:
			b.WriteRune('_')
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
