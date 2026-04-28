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

// sandboxToolDefs returns the LLM-facing definitions for the six
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
	}
}

// executeSandboxTool dispatches sandbox-* tool calls. Returns a
// human-readable string suitable for the LLM tool_result (or an
// error to abort the round). Caller must have verified a.sandbox is
// non-nil.
func (a *Agent) executeSandboxTool(ctx context.Context, name, argsJSON string) string {
	if a.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if a.session == nil {
		return "Error: no active session"
	}
	sid := a.session.ID

	// All sandbox tools require the container to exist; EnsureContainer
	// is idempotent and cheap on subsequent calls.
	if err := a.sandbox.EnsureContainer(ctx, sid); err != nil {
		return fmt.Sprintf("Error: ensure container: %v", err)
	}

	switch name {
	case "sandbox-run-shell":
		return a.toolSandboxRunShell(ctx, sid, argsJSON)
	case "sandbox-run-python":
		return a.toolSandboxRunPython(ctx, sid, argsJSON)
	case "sandbox-write-file":
		return a.toolSandboxWriteFile(sid, argsJSON)
	case "sandbox-copy-object":
		return a.toolSandboxCopyObject(sid, argsJSON)
	case "sandbox-register-object":
		return a.toolSandboxRegisterObject(sid, argsJSON)
	case "sandbox-info":
		return a.toolSandboxInfo(ctx, sid)
	default:
		return fmt.Sprintf("Error: unknown sandbox tool %q", name)
	}
}

func (a *Agent) toolSandboxRunShell(ctx context.Context, sid, argsJSON string) string {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if args.Command == "" {
		return "Error: 'command' is required"
	}
	res, err := a.sandbox.Exec(ctx, sid, sandbox.ExecArgs{Language: "shell", Code: args.Command})
	if err != nil {
		return "Error: " + err.Error()
	}
	return sandbox.FormatExecResult(res)
}

func (a *Agent) toolSandboxRunPython(ctx context.Context, sid, argsJSON string) string {
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if args.Code == "" {
		return "Error: 'code' is required"
	}
	res, err := a.sandbox.Exec(ctx, sid, sandbox.ExecArgs{Language: "python", Code: args.Code})
	if err != nil {
		return "Error: " + err.Error()
	}
	return sandbox.FormatExecResult(res)
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
	return fmt.Sprintf("wrote %s to /work/%s", humanSize(int64(len(args.Content))), filepath.ToSlash(args.Path))
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
func safeWorkPath(workDir, rel string) (string, error) {
	rel = strings.TrimPrefix(rel, "/")
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
