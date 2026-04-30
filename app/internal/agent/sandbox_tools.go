package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"
)

// sandboxToolDefs returns the LLM-facing definitions for the eight
// sandbox-* tools. Returned only when a.sandbox is non-nil; the
// caller in buildToolDefs gates on that.
func sandboxToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        "sandbox-run-shell",
			Description: "Execute a shell command inside this session's sandbox container. Files in /work persist across calls within the session and are isolated between sessions. Side effects do not affect the host. Use for filesystem operations, package installs (pip), and orchestrating subprocesses.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "Shell command to execute."},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "sandbox-run-python",
			Description: "Execute Python code inside this session's sandbox container. Working directory is /work; files there persist across calls. Each call is a fresh interpreter, but the filesystem and any installed packages persist within the session.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string", "description": "Python source to execute."},
				},
				"required": []string{"code"},
			},
		},
		{
			Name:        "sandbox-write-file",
			Description: "Write text content to /work/<path> inside this session's sandbox. Use to seed the sandbox with data the LLM has already produced (CSVs, source files, configs) without escaping it through run-shell heredocs. Path must be relative to /work; parent directories are created if missing. Existing files are overwritten.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Relative path under /work."},
					"content": map[string]any{"type": "string", "description": "Text content to write."},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "sandbox-copy-object",
			Description: "Copy a stored object (image / blob / report) from the central object repository into /work/<path> inside this session's sandbox. Use to bring user-uploaded images or earlier reports into the sandbox for analysis. Use list-objects to find a valid object_id.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object_id": map[string]any{"type": "string", "description": "Object ID from list-objects."},
					"path":      map[string]any{"type": "string", "description": "Destination path under /work. Defaults to the object's orig_name when omitted."},
				},
				"required": []string{"object_id"},
			},
		},
		{
			Name:        "sandbox-register-object",
			Description: "Register a file from /work (typically an output from sandbox-run-python — chart, generated CSV, etc.) into the central object repository. Returns the object ID, which can be referenced in reports as ![alt](object:ID).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Source path under /work."},
					"type": map[string]any{"type": "string", "description": "image | blob | report. Defaults to inference from MIME."},
					"name": map[string]any{"type": "string", "description": "Friendly name (orig_name); defaults to filename."},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "sandbox-info",
			Description: "Return a description of this session's sandbox: engine, image, Python version, key pre-installed packages, network policy, resource limits, and the contents of /work (path, size, mtime). Use this to discover the runtime before running code.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "sandbox-export-sql",
			Description: "Run a SELECT query against the analysis database and write the result as CSV to /work/<file_path>. Use this when you want sandbox-run-python (pandas etc.) to operate on a query result — pasting the result text into Python is wasteful and lossy; this hands the data over as a precise CSV file. The file appears under /work and can also be loaded back with sandbox-load-into-analysis.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql":       map[string]any{"type": "string", "description": "SELECT query to run."},
					"file_path": map[string]any{"type": "string", "description": "Destination path under /work (e.g. 'tokyo_sales.csv'). Parent directories are created if missing."},
				},
				"required": []string{"sql", "file_path"},
			},
		},
		{
			Name:        "sandbox-load-into-analysis",
			Description: "Load a CSV/JSON/JSONL file from /work into the analysis database (DuckDB) as a table, so it can be queried with query-sql, described with describe-data, etc. Use this after generating data with sandbox-run-python to bridge the produced file into the analysis side. file_path is relative to /work (do not include the '/work/' prefix).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path":  map[string]any{"type": "string", "description": "Path to the data file under /work (e.g. 'sales.csv'). Same parameter name as load-data."},
					"table_name": map[string]any{"type": "string", "description": "Table name to create in the analysis database. Alphanumeric and underscores only."},
				},
				"required": []string{"file_path", "table_name"},
			},
		},
	}
}

// executeSandboxTool dispatches sandbox-* tool calls. Returns the
// LLM-facing text result and an ActivityEventStatus that the
// agentLoop forwards to the chat as the tool-end bubble colour.
//
// Phase B-1: only run-shell / run-python actually classify
// failure (non-zero exit code or TimedOut → error). Other
// sandbox tools return success unless the Go-side error path
// fired.
func (a *Agent) executeSandboxTool(ctx context.Context, name, argsJSON string) (string, ActivityEventStatus) {
	if a.sandbox == nil {
		return "Error: sandbox is not enabled", ActivityStatusError
	}
	if a.session == nil {
		return "Error: no active session", ActivityStatusError
	}
	sid := a.session.ID

	// All sandbox tools require the container to exist; EnsureContainer
	// is idempotent and cheap on subsequent calls.
	if err := a.sandbox.EnsureContainer(ctx, sid); err != nil {
		return fmt.Sprintf("Error: ensure container: %v", err), ActivityStatusError
	}

	// Helper to wrap return-string-only handlers — anything they
	// surface that starts with "Error:" is a Go-side failure
	// (validation, file I/O, etc.) and should colour the bubble
	// red. Container exit codes are NOT classified this way; the
	// run-shell / run-python branches handle those explicitly.
	wrapErrorPrefix := func(s string) (string, ActivityEventStatus) {
		if strings.HasPrefix(s, "Error:") {
			return s, ActivityStatusError
		}
		return s, ActivityStatusSuccess
	}

	switch name {
	case "sandbox-run-shell":
		return a.toolSandboxRunShell(ctx, sid, argsJSON)
	case "sandbox-run-python":
		return a.toolSandboxRunPython(ctx, sid, argsJSON)
	case "sandbox-write-file":
		return wrapErrorPrefix(a.toolSandboxWriteFile(sid, argsJSON))
	case "sandbox-copy-object":
		return wrapErrorPrefix(a.toolSandboxCopyObject(sid, argsJSON))
	case "sandbox-register-object":
		return wrapErrorPrefix(a.toolSandboxRegisterObject(sid, argsJSON))
	case "sandbox-info":
		return wrapErrorPrefix(a.toolSandboxInfo(ctx, sid))
	case "sandbox-load-into-analysis":
		return wrapErrorPrefix(a.toolSandboxLoadIntoAnalysis(sid, argsJSON))
	case "sandbox-export-sql":
		return wrapErrorPrefix(a.toolSandboxExportSQL(sid, argsJSON))
	default:
		return fmt.Sprintf("Error: unknown sandbox tool %q", name), ActivityStatusError
	}
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

func (a *Agent) toolSandboxWriteFile(sid, argsJSON string) string {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if args.Path == "" {
		return "Error: 'path' is required"
	}
	dest, err := safeWorkPath(a.sandbox.WorkDir(sid), args.Path)
	if err != nil {
		return "Error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "Error: mkdir: " + err.Error()
	}
	if err := os.WriteFile(dest, []byte(args.Content), 0644); err != nil {
		return "Error: write: " + err.Error()
	}
	rel, _ := filepath.Rel(a.sandbox.WorkDir(sid), dest)
	return fmt.Sprintf("wrote %s to /work/%s", humanSize(int64(len(args.Content))), filepath.ToSlash(rel))
}

func (a *Agent) toolSandboxCopyObject(sid, argsJSON string) string {
	var args struct {
		ObjectID string `json:"object_id"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if args.ObjectID == "" {
		return "Error: 'object_id' is required"
	}
	if a.objects == nil {
		return "Error: object store not available"
	}
	meta, ok := a.objects.Get(args.ObjectID)
	if !ok {
		return "Error: object not found: " + args.ObjectID
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
		return "Error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "Error: mkdir: " + err.Error()
	}
	src, err := a.objects.ReadData(args.ObjectID)
	if err != nil {
		return "Error: read object: " + err.Error()
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "Error: create dest: " + err.Error()
	}
	n, copyErr := io.Copy(out, src)
	if cerr := out.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		return "Error: copy: " + copyErr.Error()
	}
	return fmt.Sprintf("copied object %s (%s) to /work/%s", args.ObjectID, humanSize(n), filepath.ToSlash(destPath))
}

func (a *Agent) toolSandboxRegisterObject(sid, argsJSON string) string {
	var args struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if args.Path == "" {
		return "Error: 'path' is required"
	}
	if a.objects == nil {
		return "Error: object store not available"
	}
	src, err := safeWorkPath(a.sandbox.WorkDir(sid), args.Path)
	if err != nil {
		return "Error: " + err.Error()
	}
	f, err := os.Open(src)
	if err != nil {
		return "Error: open source: " + err.Error()
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
		return "Error: store: " + err.Error()
	}
	return fmt.Sprintf("registered as object %s (%s, %s)", meta.ID, objType, humanSize(meta.Size))
}

func (a *Agent) toolSandboxInfo(ctx context.Context, sid string) string {
	info, err := a.sandbox.Info(ctx, sid)
	if err != nil {
		return "Error: " + err.Error()
	}
	return sandbox.FormatInfo(info)
}

// toolSandboxLoadIntoAnalysis bridges a /work file into the DuckDB
// analysis engine. The /work directory is on the host filesystem
// (mounted into the container), so we can read it directly via
// analysis.LoadFile without going through the container.
//
// Accepts both `file_path` (matching load-data) and `path` so that
// either spelling the LLM picks up works. We trim a leading "/work/"
// because the LLM tends to write the in-container path verbatim.
func (a *Agent) toolSandboxLoadIntoAnalysis(sid, argsJSON string) string {
	if a.analysis == nil {
		return "Error: analysis engine not available in this session"
	}
	var args struct {
		FilePath  string `json:"file_path"`
		Path      string `json:"path"`
		TableName string `json:"table_name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	rel := args.FilePath
	if rel == "" {
		rel = args.Path
	}
	rel = strings.TrimPrefix(rel, "/work/")
	if rel == "" || args.TableName == "" {
		return "Error: 'file_path' and 'table_name' are required"
	}
	src, err := safeWorkPath(a.sandbox.WorkDir(sid), rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	if _, statErr := os.Stat(src); statErr != nil {
		return "Error: file not found at /work/" + rel
	}
	if err := a.analysis.LoadFile(args.TableName, src); err != nil {
		return "Error: load: " + err.Error()
	}
	for _, t := range a.analysis.Tables() {
		if t.Name == args.TableName {
			return fmt.Sprintf("Loaded /work/%s into table %q: %d rows, columns: %v",
				rel, t.Name, t.RowCount, t.Columns)
		}
	}
	return fmt.Sprintf("Loaded /work/%s into table %q", rel, args.TableName)
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
func (a *Agent) toolSandboxExportSQL(sid, argsJSON string) string {
	if a.analysis == nil {
		return "Error: analysis engine not available in this session"
	}
	var args struct {
		SQL      string `json:"sql"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"` // accept either spelling, like load-into-analysis
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	rel := args.FilePath
	if rel == "" {
		rel = args.Path
	}
	rel = strings.TrimPrefix(rel, "/work/")
	if args.SQL == "" || rel == "" {
		return "Error: 'sql' and 'file_path' are required"
	}
	dest, err := safeWorkPath(a.sandbox.WorkDir(sid), rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "Error: mkdir: " + err.Error()
	}
	f, err := os.Create(dest)
	if err != nil {
		return "Error: create: " + err.Error()
	}
	cols, n, err := a.analysis.QuerySQLToCSV(args.SQL, f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		return "Error: " + err.Error()
	}
	relOut, _ := filepath.Rel(a.sandbox.WorkDir(sid), dest)
	return fmt.Sprintf("wrote %d rows × %d columns to /work/%s (columns: %v)", n, len(cols), filepath.ToSlash(relOut), cols)
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
	return filepath.Join(workDir, cleaned), nil
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
