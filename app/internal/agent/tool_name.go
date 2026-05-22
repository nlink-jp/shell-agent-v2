// Package agent — tool_name.go defines the canonical form of a
// tool name and the helper that converts to it.
//
// Background: some local backends (notably Gemma served via
// Ollama) emit tool calls in a Python `tool_code` block whose
// identifier syntax does not allow `-`. A registry keyed on
// `list-objects` therefore fails dispatch when such a backend
// emits `list_objects`. ADR-0023 standardises the canonical form
// as `snake_case` and applies this helper at the five registry
// boundaries (descriptor index build, descriptor lookup,
// executeTool ingress, LLM schema emit, UI tool list) so that
// hyphenated names from user shell scripts, upstream MCP
// servers, and persisted session histories continue to work
// without per-call-site special casing.
package agent

import "strings"

// canonicalToolName returns the canonical (snake_case) form of a
// tool name by replacing every `-` with `_`. Idempotent: applying
// the function twice yields the same result as applying it once.
// `mcp__<guardian>__<tool>` envelopes survive unchanged because
// only `-` characters are rewritten — the `__` separator is
// preserved.
func canonicalToolName(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}
