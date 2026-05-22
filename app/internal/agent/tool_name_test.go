package agent

import (
	"context"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
)

func TestCanonicalToolName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"list-objects", "list_objects"},
		{"list_objects", "list_objects"},                 // idempotent
		{"sandbox-run-shell", "sandbox_run_shell"},       // multi-hyphen
		{"mcp__weather__forecast", "mcp__weather__forecast"}, // `__` envelope preserved
		{"mcp__foo-bar__baz-qux", "mcp__foo_bar__baz_qux"},   // hyphens inside MCP segments rewritten
		{"", ""},                                          // empty
		{"already_snake", "already_snake"},
		{"--", "__"},
	}
	for _, c := range cases {
		got := canonicalToolName(c.in)
		if got != c.want {
			t.Errorf("canonicalToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExecuteTool_AcceptsKebabFromHistoryReplay locks in the
// session-history backward compatibility promise of ADR-0023:
// an old `ToolCall.Name` carrying a hyphenated descriptor name
// (e.g. `list-objects` from a pre-rename session) still routes
// to the registered descriptor after the boundary normalization
// is in place, even before the descriptor source is renamed.
// Without canonicalisation at executeTool ingress this would
// fall through to "unknown tool".
func TestExecuteTool_AcceptsKebabFromHistoryReplay(t *testing.T) {
	a := agentForToolDefs(t)
	// list-objects is a builtin descriptor (Source=builtin) with
	// no analysis/sandbox dependency, so dispatch succeeds against
	// the empty objstore — the handler returns a JSON envelope,
	// not an error.
	result, status := a.executeTool(context.Background(), llm.ToolCall{
		Name:      "list-objects",
		Arguments: "{}",
	})
	if status == ActivityStatusError {
		t.Fatalf("executeTool(list-objects) returned error status: %q", result)
	}
	// Same call with the canonical form must also succeed —
	// confirms the two forms route to the same handler.
	result2, status2 := a.executeTool(context.Background(), llm.ToolCall{
		Name:      "list_objects",
		Arguments: "{}",
	})
	if status2 == ActivityStatusError {
		t.Fatalf("executeTool(list_objects) returned error status: %q", result2)
	}
}
