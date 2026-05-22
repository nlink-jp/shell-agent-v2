// agent_mcp.go — MCP guardian management.
//
// Extracted from agent.go in v0.14.3 (ADR-0022). MCP guardians are
// the one Agent subsystem that's fully orthogonal to the send /
// loop / FSM core — readers focused on those concerns can skip
// this file entirely. Every function here is either a method on
// *Agent or a pure helper called only from this file or the
// dispatcher's mcp__ routing branch.
//
// The `validGuardianName` regex stays in agent.go (it's referenced
// by both spawnGuardian here and the dispatcher's name parsing in
// executeTool). The MCPStatus type, all spawn / stop / restart
// methods, and splitMCPName all live in this file.

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"github.com/nlink-jp/shell-agent-v2/internal/mcp"
)

// MCPStatus holds the status of a guardian for UI display.
type MCPStatus struct {
	Name      string `json:"name"`
	Status    string `json:"status"`    // "running", "disabled", "error"
	ToolCount int    `json:"tool_count"`
	Error     string `json:"error,omitempty"`
}

// startGuardians launches MCP guardian processes from config.
func (a *Agent) startGuardians() {
	// Take guardiansMu for the whole start sequence so concurrent
	// readers (ListTools, buildToolDefs, executeTool) see a
	// consistent map. Process spawning happens inside the lock —
	// startup is a one-shot blocking phase, not a hot path.
	a.guardiansMu.Lock()
	defer a.guardiansMu.Unlock()
	a.mcpStatuses = nil
	for _, p := range a.cfg.Tools.MCPProfiles {
		g, status := spawnGuardian(p)
		if g != nil {
			a.guardians[p.Name] = g
		}
		a.mcpStatuses = append(a.mcpStatuses, status)
	}
}

// spawnGuardian builds and starts a guardian for one profile and
// returns (live guardian or nil, MCPStatus to record). Pure helper
// used by both startGuardians (boot path) and restartGuardian
// (post-abort recovery in v0.6.1). Callers hold guardiansMu when
// updating the agent's map / status slice with the return values;
// spawnGuardian itself is stateless w.r.t. the Agent.
func spawnGuardian(p config.MCPProfileConfig) (*mcp.Guardian, MCPStatus) {
	if !p.Enabled || p.Name == "" || p.Binary == "" {
		return nil, MCPStatus{Name: p.Name, Status: "disabled"}
	}
	if !validGuardianName.MatchString(p.Name) {
		// Reject profiles whose name doesn't match the allowed
		// character set so the dispatcher's `mcp__<name>__<tool>`
		// parsing stays unambiguous (security-hardening-2.md H3).
		// Underscores and double-underscores in particular collide
		// with the separator.
		err := fmt.Errorf("invalid guardian name %q: must match %s", p.Name, validGuardianName)
		logger.Error("MCP guardian %q rejected: %v", p.Name, err)
		return nil, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()}
	}
	binary, err := validateBinaryPath(p.Binary)
	if err != nil {
		logger.Error("MCP guardian %q binary validation failed: %v", p.Name, err)
		return nil, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()}
	}
	profile, err := validateProfilePath(p.ProfilePath)
	if err != nil {
		logger.Error("MCP guardian %q profile validation failed: %v", p.Name, err)
		return nil, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()}
	}
	g := mcp.NewGuardian(binary, "--profile", profile)
	// Tag the guardian so its drained stderr lines (and any future
	// log lines) carry the profile name.
	g.SetName(p.Name)
	if err := g.Start(); err != nil {
		logger.Error("MCP guardian %q start failed: %v", p.Name, err)
		return nil, MCPStatus{Name: p.Name, Status: "error", Error: err.Error()}
	}
	toolCount := len(g.Tools())
	logger.Info("MCP guardian %q started (%d tools)", p.Name, toolCount)
	return g, MCPStatus{Name: p.Name, Status: "running", ToolCount: toolCount}
}

// validateBinaryPath ensures the path resolves to an existing executable
// regular file. Prevents arbitrary command execution if config is corrupted.
func validateBinaryPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty binary path")
	}
	expanded := config.ExpandPath(path)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("binary not found: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", abs)
	}
	if info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("not executable: %s", abs)
	}
	return abs, nil
}

// validateProfilePath validates the --profile arg for mcp-guardian, which
// accepts either a bare profile name or a file path. For paths, verify the
// file exists; for bare names, pass through after rejecting control chars.
func validateProfilePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty profile path")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("control characters not allowed in profile")
		}
	}
	if strings.ContainsRune(path, '/') || strings.HasPrefix(path, "~") {
		expanded := config.ExpandPath(path)
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return "", fmt.Errorf("invalid path: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("profile not found: %w", err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("profile is a directory: %s", abs)
		}
		return abs, nil
	}
	return path, nil
}

// MCPStatuses returns the status of all configured MCP guardians.
func (a *Agent) MCPStatuses() []MCPStatus {
	return a.mcpStatuses
}

// stopGuardians stops all running MCP guardian processes.
func (a *Agent) stopGuardians() {
	a.guardiansMu.Lock()
	defer a.guardiansMu.Unlock()
	for name, g := range a.guardians {
		g.Stop()
		logger.Info("MCP guardian %q stopped", name)
	}
	a.guardians = make(map[string]*mcp.Guardian)
}

// RestartGuardians stops and restarts all MCP guardians from current config.
func (a *Agent) RestartGuardians() {
	a.stopGuardians()
	a.guardiansMu.Lock()
	a.guardians = make(map[string]*mcp.Guardian)
	a.guardiansMu.Unlock()
	a.startGuardians()
}

// restartGuardian re-spawns the named guardian using the current
// config. Called after CallToolContext returns context.Canceled
// (which kills the child process to break out of a hung Scan) so
// the next user turn can use this guardian again. Safe to call
// from a goroutine — guardiansMu serialises map mutation against
// other readers/writers.
//
// Reading a.cfg.Tools.MCPProfiles here is safe under guardiansMu
// because Settings-driven config edits funnel through bindings.go's
// SaveConfig path, which eventually calls RestartGuardians (taking
// the same lock); there is no other writer to MCPProfiles.
func (a *Agent) restartGuardian(name string) {
	a.guardiansMu.Lock()
	defer a.guardiansMu.Unlock()

	// Find the profile config for this name. If config no longer
	// contains it (race with a Settings save), nothing to do — the
	// dead map entry stays absent and the user's next attempt
	// surfaces "guardian not found".
	var profile *config.MCPProfileConfig
	for i := range a.cfg.Tools.MCPProfiles {
		if a.cfg.Tools.MCPProfiles[i].Name == name {
			profile = &a.cfg.Tools.MCPProfiles[i]
			break
		}
	}
	if profile == nil {
		delete(a.guardians, name)
		logger.Info("MCP guardian %q removed: no longer in config", name)
		return
	}

	// Drop the stale handle. The underlying process was already
	// killed by Stop() inside CallToolContext, so no extra cleanup
	// is needed here.
	delete(a.guardians, name)

	g, status := spawnGuardian(*profile)
	if g != nil {
		a.guardians[name] = g
	}
	// Replace the existing MCPStatus entry in place so the Settings
	// UI sees the live state. Append if the entry vanished somehow.
	replaced := false
	for i := range a.mcpStatuses {
		if a.mcpStatuses[i].Name == name {
			a.mcpStatuses[i] = status
			replaced = true
			break
		}
	}
	if !replaced {
		a.mcpStatuses = append(a.mcpStatuses, status)
	}
	logger.Info("MCP guardian %q restarted: status=%s", name, status.Status)
}

// splitMCPName parses the part after `mcp__` of an MCP tool call name
// into (guardianName, toolName). It first tries the naive split on
// the first `__`; if the resulting guardianName isn't registered, it
// walks the registered guardians and picks the longest one whose name
// is a prefix of `rest` followed by `__`. This makes the parser
// tolerant of guardian or upstream-tool names containing `__`
// (security-hardening-2.md H3). Caller must hold guardiansMu.
func splitMCPName(rest string, guardians map[string]*mcp.Guardian) (string, string, bool) {
	// ADR-0023: callers pass the canonical (snake_case) form of the
	// tool envelope. Guardian registration keys may still hold the
	// original kebab form (the registered profile name is preserved
	// verbatim for log fidelity), so every lookup below compares
	// against canonicalToolName(name) rather than `name` directly.
	// The returned guardian string is the original registered key so
	// the caller can look it up in a.guardians without a second
	// canonicalisation pass.
	if i := strings.Index(rest, "__"); i > 0 {
		guardianCanon := rest[:i]
		tool := rest[i+2:]
		if tool != "" {
			for name := range guardians {
				if canonicalToolName(name) == guardianCanon {
					return name, tool, true
				}
			}
		}
	}
	// Fall back to longest-prefix match against canonicalised
	// registered names.
	var bestGuardian string
	for name := range guardians {
		marker := canonicalToolName(name) + "__"
		if !strings.HasPrefix(rest, marker) {
			continue
		}
		if len(name) > len(bestGuardian) {
			bestGuardian = name
		}
	}
	if bestGuardian == "" {
		return "", "", false
	}
	tool := rest[len(canonicalToolName(bestGuardian))+2:]
	if tool == "" {
		return "", "", false
	}
	return bestGuardian, tool, true
}
