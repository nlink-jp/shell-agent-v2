package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"
)

// executeSandboxTool dispatches a sandbox-* tool call by name
// via the descriptor registry, bypassing the MITL gate that
// dispatchDescriptor applies for production callers. Used by
// the unit tests that assert raw handler behaviour without
// simulating an MITL approval; production tool calls flow
// through executeTool → dispatchDescriptor → descriptor.Handle,
// which centralises the MITL gate.
//
// Returns "unknown sandbox tool %q" when no descriptor matches
// or when the matched descriptor isn't a sandbox-source one
// (which catches typos in test names).
func (a *Agent) executeSandboxTool(ctx context.Context, name, argsJSON string) (string, ActivityEventStatus) {
	d, ok := a.toolDescriptorByName(name)
	if !ok || d.Source != "sandbox" || d.Handle == nil {
		return fmt.Sprintf("Error: unknown sandbox tool %q", name), ActivityStatusError
	}
	return d.Handle(ctx, argsJSON)
}

func (a *Agent) toolSandboxRunShell(ctx context.Context, sid, argsJSON string) (string, ActivityEventStatus) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	if args.Command == "" {
		return "Error: 'command' is required", ActivityStatusError
	}
	res, err := a.sandbox.Exec(ctx, sid, sandbox.ExecArgs{Language: "shell", Code: args.Command})
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	return sandbox.FormatExecResult(res), execResultStatus(res)
}

func (a *Agent) toolSandboxRunPython(ctx context.Context, sid, argsJSON string) (string, ActivityEventStatus) {
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	if args.Code == "" {
		return "Error: 'code' is required", ActivityStatusError
	}
	res, err := a.sandbox.Exec(ctx, sid, sandbox.ExecArgs{Language: "python", Code: args.Code})
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	return sandbox.FormatExecResult(res), execResultStatus(res)
}

// execResultStatus maps a container ExecResult to the activity
// event status. A failed pip install or a Python traceback both
// land here as ExitCode != 0; a Vertex-side stall that fires the
// per-call timeout shows up as TimedOut.
func execResultStatus(res *sandbox.ExecResult) ActivityEventStatus {
	if res == nil {
		return ActivityStatusError
	}
	if res.TimedOut || res.ExitCode != 0 {
		return ActivityStatusError
	}
	return ActivityStatusSuccess
}

func (a *Agent) toolSandboxWriteFile(sid, argsJSON string) (string, ActivityEventStatus) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	if args.Path == "" {
		return "Error: 'path' is required", ActivityStatusError
	}
	dest, err := safeWorkPath(a.sandbox.WorkDir(sid), args.Path)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "Error: mkdir: " + err.Error(), ActivityStatusError
	}
	if err := os.WriteFile(dest, []byte(args.Content), 0644); err != nil {
		return "Error: write: " + err.Error(), ActivityStatusError
	}
	rel, _ := filepath.Rel(a.sandbox.WorkDir(sid), dest)
	return fmt.Sprintf("wrote %s to /work/%s", humanSize(int64(len(args.Content))), filepath.ToSlash(rel)), ActivityStatusSuccess
}

func (a *Agent) toolSandboxCopyObject(sid, argsJSON string) (string, ActivityEventStatus) {
	var args struct {
		ObjectID string `json:"object_id"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	if args.ObjectID == "" {
		return "Error: 'object_id' is required", ActivityStatusError
	}
	if a.objects == nil {
		return "Error: object store not available", ActivityStatusError
	}
	meta, ok := a.objects.Get(args.ObjectID)
	if !ok {
		return "Error: object not found: " + args.ObjectID, ActivityStatusError
	}
	destPath := args.Path
	if destPath == "" {
		destPath = meta.OrigName
		if destPath == "" {
			destPath = meta.ID
		}
	}
	dest, err := safeWorkPath(a.sandbox.WorkDir(sid), destPath)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "Error: mkdir: " + err.Error(), ActivityStatusError
	}
	src, err := a.objects.ReadData(args.ObjectID)
	if err != nil {
		return "Error: read object: " + err.Error(), ActivityStatusError
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "Error: create dest: " + err.Error(), ActivityStatusError
	}
	n, copyErr := io.Copy(out, src)
	if cerr := out.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		return "Error: copy: " + copyErr.Error(), ActivityStatusError
	}
	return fmt.Sprintf("copied object %s (%s) to /work/%s", args.ObjectID, humanSize(n), filepath.ToSlash(destPath)), ActivityStatusSuccess
}

func (a *Agent) toolSandboxRegisterObject(sid, argsJSON string) (string, ActivityEventStatus) {
	var args struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	if args.Path == "" {
		return "Error: 'path' is required", ActivityStatusError
	}
	if a.objects == nil {
		return "Error: object store not available", ActivityStatusError
	}
	src, err := safeWorkPath(a.sandbox.WorkDir(sid), args.Path)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	f, err := os.Open(src)
	if err != nil {
		return "Error: open source: " + err.Error(), ActivityStatusError
	}
	defer f.Close()

	mime := sandbox.MimeFromPath(args.Path)
	objType := args.Type
	if objType == "" {
		objType = sandbox.ObjectTypeForMIME(mime)
	}
	name := args.Name
	if name == "" {
		name = filepath.Base(args.Path)
	}
	meta, err := a.objects.Store(f, objstore.ObjectType(objType), mime, name, sid)
	if err != nil {
		return "Error: store: " + err.Error(), ActivityStatusError
	}
	return fmt.Sprintf("registered as object %s (%s, %s)", meta.ID, objType, humanSize(meta.Size)), ActivityStatusSuccess
}

func (a *Agent) toolSandboxInfo(ctx context.Context, sid string) (string, ActivityEventStatus) {
	info, err := a.sandbox.Info(ctx, sid)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	return sandbox.FormatInfo(info), ActivityStatusSuccess
}

// toolSandboxLoadIntoAnalysis bridges a /work file into the DuckDB
// analysis engine. The /work directory is on the host filesystem
// (mounted into the container), so we can read it directly via
// analysis.LoadFile without going through the container.
//
// Accepts both `file_path` (matching load-data) and `path` so that
// either spelling the LLM picks up works. We trim a leading "/work/"
// because the LLM tends to write the in-container path verbatim.
func (a *Agent) toolSandboxLoadIntoAnalysis(sid, argsJSON string) (string, ActivityEventStatus) {
	if a.analysis == nil {
		return "Error: analysis engine not available in this session", ActivityStatusError
	}
	var args struct {
		FilePath  string `json:"file_path"`
		Path      string `json:"path"`
		TableName string `json:"table_name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	rel := args.FilePath
	if rel == "" {
		rel = args.Path
	}
	rel = strings.TrimPrefix(rel, "/work/")
	if rel == "" || args.TableName == "" {
		return "Error: 'file_path' and 'table_name' are required", ActivityStatusError
	}
	src, err := safeWorkPath(a.sandbox.WorkDir(sid), rel)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	if _, statErr := os.Stat(src); statErr != nil {
		return "Error: file not found at /work/" + rel, ActivityStatusError
	}
	if err := a.analysis.LoadFile(args.TableName, src); err != nil {
		return "Error: load: " + err.Error(), ActivityStatusError
	}
	for _, t := range a.analysis.Tables() {
		if t.Name == args.TableName {
			return fmt.Sprintf("Loaded /work/%s into table %q: %d rows, columns: %v",
				rel, t.Name, t.RowCount, t.Columns), ActivityStatusSuccess
		}
	}
	return fmt.Sprintf("Loaded /work/%s into table %q", rel, args.TableName), ActivityStatusSuccess
}

// RestartSandbox tears down every existing sandbox container and
// re-evaluates cfg.Sandbox, so Settings changes take effect without
// an app restart. Safe to call when sandbox is disabled (no-op for
// the engine) or when cfg.Sandbox.Enabled has just flipped.
func (a *Agent) RestartSandbox() {
	if a.sandbox != nil {
		_ = a.sandbox.StopAll(context.Background())
	}
	a.sandbox = nil
	a.maybeStartSandbox()
}

// toolSandboxExportSQL runs a SELECT query and writes the result as
// CSV into /work, so the LLM can hand a precise dataset to
// sandbox-run-python (pandas, scikit-learn, …) without
// reconstructing it from text.
func (a *Agent) toolSandboxExportSQL(sid, argsJSON string) (string, ActivityEventStatus) {
	if a.analysis == nil {
		return "Error: analysis engine not available in this session", ActivityStatusError
	}
	var args struct {
		SQL      string `json:"sql"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"` // accept either spelling, like load-into-analysis
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	rel := args.FilePath
	if rel == "" {
		rel = args.Path
	}
	rel = strings.TrimPrefix(rel, "/work/")
	if args.SQL == "" || rel == "" {
		return "Error: 'sql' and 'file_path' are required", ActivityStatusError
	}
	dest, err := safeWorkPath(a.sandbox.WorkDir(sid), rel)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "Error: mkdir: " + err.Error(), ActivityStatusError
	}
	f, err := os.Create(dest)
	if err != nil {
		return "Error: create: " + err.Error(), ActivityStatusError
	}
	cols, n, err := a.analysis.QuerySQLToCSV(args.SQL, f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		return "Error: " + err.Error(), ActivityStatusError
	}
	relOut, _ := filepath.Rel(a.sandbox.WorkDir(sid), dest)
	return fmt.Sprintf("wrote %d rows × %d columns to /work/%s (columns: %v)", n, len(cols), filepath.ToSlash(relOut), cols), ActivityStatusSuccess
}

// SandboxStop tears down the per-session sandbox container, if any.
// Safe to call when sandbox is disabled (no-op).
func (a *Agent) SandboxStop(ctx context.Context, sessionID string) error {
	if a.sandbox == nil {
		return nil
	}
	return a.sandbox.Stop(ctx, sessionID)
}

// SandboxStopAll reaps every container belonging to this app — call
// from the bindings shutdown hook.
func (a *Agent) SandboxStopAll(ctx context.Context) error {
	if a.sandbox == nil {
		return nil
	}
	return a.sandbox.StopAll(ctx)
}

// safeWorkPath joins workDir + relative under the sandbox's /work,
// rejecting absolute paths and "../" traversal that would escape the
// mount.
//
// LLMs often write the in-container path verbatim ("/work/foo.png")
// because that's what they see inside sandbox-run-python. We strip
// the "/work/" or leading "/" prefix as a courtesy so the join
// doesn't end up with a doubled "/work/work/" segment.
//
// Returns the host-side absolute path the caller can pass to
// os.Open / os.Create / os.WriteFile / DuckDB. The returned path
// is guaranteed to live under workDir even when the LLM tries to
// escape via "..", absolute paths, or symlinks created from inside
// the container. (The container runs as the host UID, so /work
// symlinks resolve on the host when the Go side opens them — see
// docs/{en,ja}/security-hardening{,.ja}.md §3.2.1.)
func safeWorkPath(workDir, rel string) (string, error) {
	// Normalise the LLM's path back into a /work-relative form.
	for {
		trimmed := strings.TrimPrefix(rel, "/work/")
		trimmed = strings.TrimPrefix(trimmed, "/")
		if trimmed == rel {
			break
		}
		rel = trimmed
	}
	rel = strings.TrimPrefix(rel, "work/")

	if rel == "" {
		return "", fmt.Errorf("path is empty after normalising the /work prefix")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to /work")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes /work: %q", rel)
	}

	joined := filepath.Join(workDir, cleaned)

	// macOS /var → /private/var means the lexical workDir often
	// doesn't match the symlink-resolved form. Resolve the
	// workDir once so the prefix check below compares like with
	// like.
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		// workDir is created by EnsureContainer before any tool
		// runs; if it doesn't resolve, that's a setup bug.
		return "", fmt.Errorf("resolve workDir: %w", err)
	}

	// Resolve symlinks on the parent (the leaf may not exist for
	// write operations). EvalSymlinks errors on a non-existent
	// component; treat that as "no resolution needed" — the
	// lexical check above already proved cleaned stays under
	// workDir.
	parent := filepath.Dir(joined)
	leaf := filepath.Base(joined)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return joined, nil
		}
		return "", fmt.Errorf("resolve parent: %w", err)
	}
	if !strings.HasPrefix(resolvedParent+string(filepath.Separator), resolvedWorkDir+string(filepath.Separator)) && resolvedParent != resolvedWorkDir {
		return "", fmt.Errorf("path escapes /work via symlink: %q", rel)
	}
	final := filepath.Join(resolvedParent, leaf)

	// Reject when the leaf itself is a symlink, even one that
	// resolves inside workDir — keeps the attack surface from
	// growing across follow-up operations on the same session
	// (e.g. an attacker swapping the target after a write).
	if info, err := os.Lstat(final); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path is a symlink: %q", rel)
	}
	return final, nil
}

// humanSize is a tiny duplicate of sandbox.humanSize so we can keep
// it unexported in the sandbox package.
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
