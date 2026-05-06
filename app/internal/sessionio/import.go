// import.go: read a .shellagent bundle, extract it as a fresh
// session, and rewrite all object references to the IDs returned
// by the live objstore.
//
// Three classes of failure are surfaced distinctly:
//   - validation failures (bad zip, missing manifest, wrong schema,
//     zip-slip path, objects index out of sync) — bundle is
//     untouched; nothing is written to disk.
//   - extract / objstore failures partway through — full rollback:
//     any objects already registered are deleted, the partial
//     destination directory is removed.
//   - successful import returns the freshly minted session ID;
//     caller (the agent) auto-switches to it.

package sessionio

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/contextbuild"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// ImportResult describes a successful import. Used by callers
// (bindings / agent) for audit logging without needing a second
// disk read.
type ImportResult struct {
	NewSessionID string
	Manifest     *Manifest
	Bytes        int64
	ObjectCount  int
}

// ImportSession extracts a .shellagent bundle into a new session
// directory and registers any bundled objstore objects under
// fresh IDs.
//
// srcPath is the bundle on disk. sessionsBaseDir is the directory
// that will contain the new `<newID>/` subdirectory (typically
// derived from memory.SessionDir but passed in for testability).
// objstoreWriter receives each bundled object via Store(); the
// returned ObjectMeta.ID becomes the new ID for reference rewriting.
// existingTitles is the snapshot of session titles taken from the
// caller (used for collision-suffix logic).
func ImportSession(srcPath, sessionsBaseDir string, objstoreWriter ObjstoreWriter, existingTitles []string) (*ImportResult, error) {
	if objstoreWriter == nil {
		return nil, errors.New("sessionio: nil ObjstoreWriter")
	}

	st, err := os.Stat(srcPath)
	if err != nil {
		return nil, fmt.Errorf("sessionio: stat bundle: %w", err)
	}
	r, err := zip.OpenReader(srcPath)
	if err != nil {
		return nil, fmt.Errorf("sessionio: not a valid .shellagent bundle: %w", err)
	}
	defer r.Close()

	plan, err := validateBundle(&r.Reader)
	if err != nil {
		return nil, err
	}

	// Title collision: suffix " (imported)", " (imported 2)", ...
	finalTitle := resolveTitle(plan.manifest.Session.Title, existingTitles)

	// Generate a unique session ID. Collisions on disk are
	// vanishingly unlikely with millisecond precision but we
	// retry a handful of times to cover the case where two
	// imports land in the same millisecond.
	var newID, destDir string
	for attempt := 0; attempt < 5; attempt++ {
		candidate := fmt.Sprintf("sess-%d", nowUnixMilli())
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d", candidate, attempt)
		}
		dir := filepath.Join(sessionsBaseDir, candidate)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			newID, destDir = candidate, dir
			break
		}
	}
	if newID == "" {
		return nil, errors.New("sessionio: could not allocate fresh session ID")
	}

	if err := os.MkdirAll(destDir, 0700); err != nil {
		return nil, fmt.Errorf("sessionio: mkdir destination: %w", err)
	}

	// Track work that needs rolling back if anything below fails.
	var registeredObjectIDs []string
	rollback := func() {
		for _, id := range registeredObjectIDs {
			_ = objstoreWriter.Delete(id)
		}
		_ = os.RemoveAll(destDir)
	}

	// 1. Extract every entry except objects/.
	for _, f := range plan.entries {
		if strings.HasPrefix(f.Name, "objects/") || f.Name == "manifest.json" {
			continue
		}
		if err := extractEntry(f, destDir); err != nil {
			rollback()
			return nil, err
		}
	}

	// 2. Register objstore objects under fresh IDs and build the
	//    old→new map for reference rewriting.
	idMap := make(map[string]string, len(plan.objectIndex))
	for _, entry := range plan.objectIndex {
		blobFile, ok := plan.objectBlobs[entry.ID]
		if !ok {
			rollback()
			return nil, fmt.Errorf("sessionio: bundle objects index/blobs out of sync: missing blob for %s", entry.ID)
		}
		rc, err := blobFile.Open()
		if err != nil {
			rollback()
			return nil, fmt.Errorf("sessionio: open bundled blob %s: %w", entry.ID, err)
		}
		meta, storeErr := objstoreWriter.Store(rc, entry.Type, entry.MimeType, entry.OrigName, newID)
		closeErr := rc.Close()
		if storeErr != nil {
			rollback()
			return nil, fmt.Errorf("sessionio: register object %s: %w", entry.ID, storeErr)
		}
		if closeErr != nil {
			rollback()
			return nil, fmt.Errorf("sessionio: close bundled blob %s: %w", entry.ID, closeErr)
		}
		registeredObjectIDs = append(registeredObjectIDs, meta.ID)
		idMap[entry.ID] = meta.ID
	}

	// 3. Rewrite chat.json: id field + Record.ObjectIDs[] + Content.
	if err := rewriteChatJSON(filepath.Join(destDir, "chat.json"), newID, finalTitle, plan.manifest.Session.Private, idMap); err != nil {
		rollback()
		return nil, err
	}

	// 4. Rewrite summaries.json (if present).
	summariesPath := filepath.Join(destDir, "summaries.json")
	if _, err := os.Stat(summariesPath); err == nil {
		if err := rewriteSummariesJSON(summariesPath, idMap); err != nil {
			rollback()
			return nil, err
		}
	}

	return &ImportResult{
		NewSessionID: newID,
		Manifest:     plan.manifest,
		Bytes:        st.Size(),
		ObjectCount:  len(plan.objectIndex),
	}, nil
}

// importPlan is the result of the validation pass: every entry
// that will be extracted, plus parsed object metadata if present.
type importPlan struct {
	manifest    *Manifest
	entries     []*zip.File
	objectIndex []objectIndexEntry
	objectBlobs map[string]*zip.File
}

func validateBundle(r *zip.Reader) (*importPlan, error) {
	plan := &importPlan{
		objectBlobs: map[string]*zip.File{},
	}
	var (
		manifestFile *zip.File
		hasChatJSON  bool
	)
	for _, f := range r.File {
		// 1. Path safety: refuse absolute, refuse "..", normalise
		//    via path.Clean and confirm the cleaned name still
		//    matches the original (= no traversal).
		if err := safeArchivePath(f.Name); err != nil {
			return nil, err
		}
		// 2. Categorise. Directory entries (trailing /) are kept so
		//    extraction can recreate empty work/ subdirs.
		switch {
		case f.Name == "manifest.json":
			manifestFile = f
		case f.Name == "chat.json":
			hasChatJSON = true
		case f.Name == "objects/index.json":
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("sessionio: open objects/index.json: %w", err)
			}
			data, readErr := io.ReadAll(rc)
			_ = rc.Close()
			if readErr != nil {
				return nil, fmt.Errorf("sessionio: read objects/index.json: %w", readErr)
			}
			if err := json.Unmarshal(data, &plan.objectIndex); err != nil {
				return nil, fmt.Errorf("sessionio: parse objects/index.json: %w", err)
			}
		case strings.HasPrefix(f.Name, "objects/data/"):
			id := strings.TrimPrefix(f.Name, "objects/data/")
			if id == "" || strings.Contains(id, "/") {
				return nil, fmt.Errorf("sessionio: invalid object blob path: %s", f.Name)
			}
			plan.objectBlobs[id] = f
		}
		plan.entries = append(plan.entries, f)
	}

	// 3. Manifest is mandatory.
	if manifestFile == nil {
		return nil, errors.New("sessionio: missing or corrupt manifest.json")
	}
	rc, err := manifestFile.Open()
	if err != nil {
		return nil, fmt.Errorf("sessionio: open manifest.json: %w", err)
	}
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return nil, fmt.Errorf("sessionio: read manifest.json: %w", readErr)
	}
	manifest, err := UnmarshalManifest(data)
	if err != nil {
		return nil, err
	}
	plan.manifest = manifest

	// 4. chat.json is required.
	if !hasChatJSON {
		return nil, errors.New("sessionio: bundle missing required file: chat.json")
	}

	// 5. objects/ index ↔ blob consistency.
	indexIDs := make(map[string]struct{}, len(plan.objectIndex))
	for _, e := range plan.objectIndex {
		if e.ID == "" {
			return nil, errors.New("sessionio: bundle objects index has empty ID")
		}
		indexIDs[e.ID] = struct{}{}
		if _, ok := plan.objectBlobs[e.ID]; !ok {
			return nil, fmt.Errorf("sessionio: bundle objects index/blobs out of sync: missing blob for %s", e.ID)
		}
	}
	for id := range plan.objectBlobs {
		if _, ok := indexIDs[id]; !ok {
			return nil, fmt.Errorf("sessionio: bundle objects index/blobs out of sync: orphan blob %s", id)
		}
	}

	return plan, nil
}

// safeArchivePath enforces the zip-slip mitigation: refuse
// absolute paths, refuse traversal segments, refuse anything that
// path.Clean would change (which catches subtle attacks like
// `foo//../bar`).
func safeArchivePath(name string) error {
	if name == "" {
		return errors.New("sessionio: bundle contains empty entry name")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return fmt.Errorf("sessionio: bundle contains unsafe path: %s", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return fmt.Errorf("sessionio: bundle contains unsafe path: %s", name)
	}
	// path.Clean strips trailing slashes; preserve dir entries.
	wantTrailingSlash := strings.HasSuffix(name, "/")
	hasTrailingSlash := strings.HasSuffix(cleaned, "/")
	if wantTrailingSlash != hasTrailingSlash {
		// Allow normalisation that only differs in trailing slash.
		if !wantTrailingSlash {
			return fmt.Errorf("sessionio: bundle contains unsafe path: %s", name)
		}
	}
	return nil
}

func extractEntry(f *zip.File, destDir string) error {
	target := filepath.Join(destDir, filepath.FromSlash(f.Name))

	// Defensive: confirm the target stays under destDir even after
	// platform-specific path handling. zip-slip belt + braces.
	rel, err := filepath.Rel(destDir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("sessionio: bundle contains unsafe path: %s", f.Name)
	}

	if strings.HasSuffix(f.Name, "/") {
		return os.MkdirAll(target, 0700)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return fmt.Errorf("sessionio: mkdir for %s: %w", f.Name, err)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("sessionio: open %s: %w", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("sessionio: create %s: %w", f.Name, err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		return fmt.Errorf("sessionio: write %s: %w", f.Name, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("sessionio: close %s: %w", f.Name, err)
	}
	return nil
}

func rewriteChatJSON(path, newID, finalTitle string, private bool, idMap map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("sessionio: read chat.json: %w", err)
	}
	var session memory.Session
	if err := json.Unmarshal(data, &session); err != nil {
		return fmt.Errorf("sessionio: parse chat.json: %w", err)
	}
	session.ID = newID
	session.Title = finalTitle
	session.Private = private
	RewriteRecords(session.Records, idMap)
	out, err := json.MarshalIndent(&session, "", "  ")
	if err != nil {
		return fmt.Errorf("sessionio: marshal chat.json: %w", err)
	}
	if err := os.WriteFile(path, out, 0600); err != nil {
		return fmt.Errorf("sessionio: write chat.json: %w", err)
	}
	return nil
}

func rewriteSummariesJSON(path string, idMap map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("sessionio: read summaries.json: %w", err)
	}
	var cache contextbuild.SummaryCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return fmt.Errorf("sessionio: parse summaries.json: %w", err)
	}
	RewriteSummaries(cache.Entries, idMap)
	out, err := json.MarshalIndent(&cache, "", "  ")
	if err != nil {
		return fmt.Errorf("sessionio: marshal summaries.json: %w", err)
	}
	if err := os.WriteFile(path, out, 0600); err != nil {
		return fmt.Errorf("sessionio: write summaries.json: %w", err)
	}
	return nil
}

func resolveTitle(base string, existing []string) string {
	if base == "" {
		base = "Imported Session"
	}
	taken := make(map[string]struct{}, len(existing))
	for _, t := range existing {
		taken[t] = struct{}{}
	}
	if _, clash := taken[base]; !clash {
		return base
	}
	candidate := base + " (imported)"
	if _, clash := taken[candidate]; !clash {
		return candidate
	}
	for n := 2; n < 1000; n++ {
		candidate = fmt.Sprintf("%s (imported %d)", base, n)
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
	// Pathological case: thousand collisions. Append a timestamp.
	return fmt.Sprintf("%s (imported %d)", base, time.Now().UnixNano())
}

func nowUnixMilli() int64 {
	return time.Now().UnixNano() / 1e6
}
