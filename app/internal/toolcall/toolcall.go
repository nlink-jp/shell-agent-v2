// Package toolcall manages shell script tool registration and execution with MITL.
package toolcall

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/logger"
)

// DefaultTimeout for tool execution.
const DefaultTimeout = 30 * time.Second

// Category determines MITL approval requirements.
type Category string

const (
	CategoryRead    Category = "read"
	CategoryWrite   Category = "write"
	CategoryExecute Category = "execute"
)

// Param is a tool parameter definition.
type Param struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// Tool represents a registered shell script tool.
//
// Timeout, when > 0, overrides the package-level DefaultTimeout for
// this tool only. Set via the `@timeout: N` script header (N in
// seconds). Zero (the default) means use DefaultTimeout. Per-tool
// override exists for legitimately long-running tools — e.g. a
// script that polls an external service or runs a heavy local
// command — so they can opt out of the 30-second cap without
// raising the floor for every other tool.
// Design: docs/en/history/tool-execution-timeout.md.
type Tool struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Params      []Param       `json:"params"`
	Category    Category      `json:"category"`
	ScriptPath  string        `json:"script_path"`
	Timeout     time.Duration `json:"timeout,omitempty"`
}

// NeedsMITL reports whether this tool requires Man-In-The-Loop approval.
func (t *Tool) NeedsMITL() bool {
	return t.Category == CategoryWrite || t.Category == CategoryExecute
}

// Registry manages discovered shell script tools.
type Registry struct {
	tools map[string]*Tool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*Tool)}
}

// ScanDir discovers tools by scanning scripts in the given directory.
// Scripts must have header comments in the standard format.
func (r *Registry) ScanDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		tool, err := parseToolHeader(path)
		if err != nil {
			continue // skip unparseable files
		}
		if tool != nil {
			r.tools[tool.Name] = tool
		}
	}
	return nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (*Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns all registered tools.
func (r *Registry) All() []*Tool {
	result := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// ExecOption configures one Execute call. Variadic options
// pattern keeps the (ctx, tool, args) signature additive — existing
// callers remain valid; new options like WithWorkDir layer on top
// without breaking them.
type ExecOption func(*execConfig)

type execConfig struct {
	workDir string
}

// WithWorkDir sets the SHELL_AGENT_WORK_DIR environment variable
// on the spawned process so the script can write artefacts to the
// per-session work directory the sandbox bind-mounts at /work.
// Design: docs/en/history/work-dir-shell-bridge.md.
func WithWorkDir(path string) ExecOption {
	return func(c *execConfig) { c.workDir = path }
}

// Execute runs a tool script with the given JSON arguments.
//
// Per-tool Timeout (set via the `@timeout: N` header) wins over the
// package DefaultTimeout when > 0. Design:
// docs/en/history/tool-execution-timeout.md.
//
// Variadic ExecOption args allow callers to inject extra context
// such as the session work directory; absent options preserve the
// pre-v0.1.25 behaviour exactly.
func Execute(ctx context.Context, tool *Tool, argsJSON string, opts ...ExecOption) (string, error) {
	cfg := execConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	timeout := tool.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tool.ScriptPath)
	cmd.Stdin = strings.NewReader(argsJSON)
	if cfg.workDir != "" {
		cmd.Env = append(os.Environ(), "SHELL_AGENT_WORK_DIR="+cfg.workDir)
	}

	// Abort responsiveness fix: shell scripts spawn child processes
	// (curl, sleep, gem-search, …) that exec.CommandContext's
	// default cancel does NOT reach — it only signals the script
	// itself, leaving the children holding the stdout/stderr
	// pipes. CombinedOutput then blocks forever in Wait waiting
	// for the pipes to close, so a UI Abort never terminates the
	// tool round and the agent loop never advances.
	//
	// Three cooperating fixes:
	//   1. Setpgid: put the script in its own process group so
	//      we can signal the whole tree with one Kill.
	//   2. Cmd.Cancel: override CommandContext's default kill to
	//      SIGKILL the negative pid (= the entire group).
	//   3. Cmd.WaitDelay: if a stubborn child still holds the
	//      pipe past the cancel, force-close it after a short
	//      grace so Wait returns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the entire process group. Negative pid means
		// "every process whose pgid equals abs(pid)".
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Surface ctx-driven termination distinctly so the operator
		// can tell whether Abort actually reached this layer.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("tool %q cancelled: %w (ctx err: %v)\nOutput: %s", tool.Name, err, ctxErr, string(output))
		}
		return "", fmt.Errorf("tool %q failed: %w\nOutput: %s", tool.Name, err, string(output))
	}

	return string(output), nil
}

// ToolDefParams converts tool params to a JSON schema for LLM tool definitions.
func (t *Tool) ToolDefParams() map[string]any {
	properties := make(map[string]any)
	required := make([]string, 0)

	for _, p := range t.Params {
		properties[p.Name] = map[string]any{
			"type":        p.Type,
			"description": p.Description,
		}
		required = append(required, p.Name)
	}

	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

// --- header parsing ---

// parseToolHeader reads header comments from a script file.
// Format:
//
//	#!/bin/bash
//	# @tool: tool-name
//	# @description: Tool description
//	# @param: name type "description"
//	# @category: read|write|execute
//	# @timeout: 120                  (optional; positive integer of
//	                                  seconds, default DefaultTimeout)
//
// Unknown directives are silently ignored. An invalid @timeout
// (non-numeric, zero, negative) is logged via internal/logger and
// the script falls back to DefaultTimeout. Design:
// docs/en/history/tool-execution-timeout.md.
func parseToolHeader(path string) (*Tool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tool := &Tool{
		ScriptPath: path,
		Category:   CategoryRead,
	}

	scanner := bufio.NewScanner(f)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		if lineCount > 20 { // only scan first 20 lines
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#") {
			if lineCount > 1 { // allow shebang on line 1
				break
			}
			continue
		}

		line = strings.TrimPrefix(line, "#")
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "@tool:") {
			tool.Name = strings.TrimSpace(strings.TrimPrefix(line, "@tool:"))
		} else if strings.HasPrefix(line, "@description:") {
			tool.Description = strings.TrimSpace(strings.TrimPrefix(line, "@description:"))
		} else if strings.HasPrefix(line, "@category:") {
			cat := strings.TrimSpace(strings.TrimPrefix(line, "@category:"))
			switch Category(cat) {
			case CategoryRead, CategoryWrite, CategoryExecute:
				tool.Category = Category(cat)
			}
		} else if strings.HasPrefix(line, "@param:") {
			param := parseParam(strings.TrimPrefix(line, "@param:"))
			if param != nil {
				tool.Params = append(tool.Params, *param)
			}
		} else if strings.HasPrefix(line, "@timeout:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "@timeout:"))
			if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
				tool.Timeout = time.Duration(secs) * time.Second
			} else {
				// Surface the typo via the regular log path; the
				// script still loads with DefaultTimeout. See
				// docs/en/history/tool-execution-timeout.md §4.4.
				logger.Error("toolcall: %s: ignoring invalid @timeout %q (must be a positive integer of seconds)", path, raw)
			}
		}
	}

	if tool.Name == "" {
		return nil, nil // not a tool script
	}

	return tool, nil
}

func parseParam(s string) *Param {
	s = strings.TrimSpace(s)
	// Format: name type "description"
	parts := strings.SplitN(s, " ", 3)
	if len(parts) < 2 {
		return nil
	}

	p := &Param{
		Name: parts[0],
		Type: parts[1],
	}
	if len(parts) >= 3 {
		p.Description = strings.Trim(parts[2], "\"")
	}
	return p
}

// ArgsFromJSON extracts arguments from JSON for display.
func ArgsFromJSON(argsJSON string) map[string]any {
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)
	return args
}
