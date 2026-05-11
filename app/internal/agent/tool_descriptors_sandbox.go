// tool_descriptors_sandbox.go — descriptors for the eight
// sandbox-* tools (sandbox-run-shell, sandbox-run-python,
// sandbox-write-file, sandbox-copy-object,
// sandbox-register-object, sandbox-info, sandbox-export-sql,
// sandbox-load-into-analysis).
//
// All sandbox tools require a live container, so each
// descriptor's Handle goes through sandboxHandle() — the
// per-tool wrapper that nil-checks a.sandbox / a.session and
// runs EnsureContainer once before delegating to the
// underlying toolSandboxXxx() method. This mirrors what
// executeSandboxTool used to do at the top of its switch.
//
// Source = "sandbox" labels them in the Settings UI's per-source
// grouping. Category = "execute" everywhere — the sandbox runs
// arbitrary code on the user's machine via podman/docker, so
// the MITL gate must always fire by default.
//
// MITLDefault = true. The pre-refactor IsToolMITLRequired
// resolved this via the strings.HasPrefix("sandbox-", name)
// branch (line 1947 of agent.go). That prefix branch stays in
// place after the cutover as defense in depth: if a future
// edit accidentally drops a sandbox descriptor, the prefix
// branch still gates the call. The override priority is
// preserved: cfg.Tools.MITLOverrides[name] wins over both the
// prefix branch and the descriptor default.
//
// Phase 3a of the refactor: defines the descriptors only. No
// consumer is wired yet; Phase 3b cuts buildToolDefs /
// ListTools / executeTool over in a single atomic commit so
// no transient state double-emits sandbox tools.

package agent

import (
	"context"
	"fmt"
)

// sandboxDescriptors returns the eight sandbox tool
// descriptors. Caller in New() should append the slice only
// when a.sandbox != nil — there is no point exposing
// sandbox-* tools to the LLM when the engine isn't running,
// and that conditional registration also lets the Settings UI
// stop listing sandbox tools when the user has disabled the
// sandbox or no image is selected.
//
// The eight definitions are ordered to match v0.5's
// sandboxToolDefs() so the LLM tool-list ordering and the
// Settings UI table both remain visually identical to the
// pre-refactor build. Order changes are cosmetic but easy to
// notice in screenshots, so we preserve it.
func (a *Agent) sandboxDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
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
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(ctx context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxRunShell(ctx, sid, args)
			}),
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
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(ctx context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxRunPython(ctx, sid, args)
			}),
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
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(_ context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxWriteFile(sid, args)
			}),
		},
		{
			Name:        "sandbox-copy-object",
			Description: "Copy a stored object (image / blob / report / markdown) from the session object store into /work/<path> inside this session's sandbox. Use to bring user-uploaded images, markdown attachments, or earlier reports into the sandbox for analysis (e.g. running ripgrep or pandoc against an attached document). Use list-objects to find a valid object_id.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object_id": map[string]any{"type": "string", "description": "Object ID from list-objects."},
					"path":      map[string]any{"type": "string", "description": "Destination path under /work. Defaults to the object's orig_name when omitted."},
				},
				"required": []string{"object_id"},
			},
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(_ context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxCopyObject(sid, args)
			}),
		},
		{
			Name:        "sandbox-register-object",
			Description: "Register a file from /work (typically an output from sandbox-run-python — chart, generated CSV, etc.) into the session object store. Returns the object ID, which can be referenced in reports as ![alt](object:ID).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Source path under /work."},
					"type": map[string]any{"type": "string", "description": "image | blob | report | markdown. Defaults to inference from MIME (image/* → image, text/markdown → report, otherwise blob). Pass \"markdown\" explicitly when staging user-supplied source material rather than agent-generated content."},
					"name": map[string]any{"type": "string", "description": "Friendly name (orig_name); defaults to filename."},
				},
				"required": []string{"path"},
			},
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(_ context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxRegisterObject(sid, args)
			}),
		},
		{
			Name:        "sandbox-info",
			Description: "Return a description of this session's sandbox: engine, image, Python version, key pre-installed packages, network policy, resource limits, and the contents of /work (path, size, mtime). Use this to discover the runtime before running code.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			// sandbox-info takes no JSON args; the wrapper still
			// receives the empty-object string from the
			// dispatcher — discard it.
			Handle: a.sandboxHandle(func(ctx context.Context, sid, _ string) (string, ActivityEventStatus) {
				return a.toolSandboxInfo(ctx, sid)
			}),
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
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(_ context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxExportSQL(sid, args)
			}),
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
			Category:    "execute",
			Source:      "sandbox",
			MITLDefault: true,
			Handle: a.sandboxHandle(func(_ context.Context, sid, args string) (string, ActivityEventStatus) {
				return a.toolSandboxLoadIntoAnalysis(sid, args)
			}),
		},
	}
}

// sandboxHandle wraps a sandbox per-tool function so it slots
// into ToolDescriptor.Handle. It performs the common
// preconditions (a.sandbox + a.session presence checks plus
// EnsureContainer) before invoking fn. Mirrors what
// executeSandboxTool used to do at the top of its switch.
//
// The nil-check on a.sandbox is defense in depth: New() only
// registers sandbox descriptors when a.sandbox != nil, so in
// normal operation this branch never fires. It exists so a
// future edit that loosens the registration condition (or a
// concurrent shutdown of the engine) cannot panic.
func (a *Agent) sandboxHandle(fn func(ctx context.Context, sid, args string) (string, ActivityEventStatus)) func(ctx context.Context, args string) (string, ActivityEventStatus) {
	return func(ctx context.Context, args string) (string, ActivityEventStatus) {
		if a.sandbox == nil {
			return "Error: sandbox is not enabled", ActivityStatusError
		}
		if a.session == nil {
			return "Error: no active session", ActivityStatusError
		}
		sid := a.session.ID
		if err := a.sandbox.EnsureContainer(ctx, sid); err != nil {
			return fmt.Sprintf("Error: ensure container: %v", err), ActivityStatusError
		}
		return fn(ctx, sid, args)
	}
}
