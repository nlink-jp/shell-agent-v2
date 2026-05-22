// tool_descriptors_builtin.go — descriptors for the four
// "builtin" tools that v0.5 dispatched directly from
// executeTool() rather than going through executeAnalysisTool():
// resolve-date, list-objects, get-object, register-object.
//
// These tools don't depend on the analysis engine
// (a.analysis), so they don't fit the "delegate to
// executeAnalysisTool which checks a.analysis != nil"
// pattern. The Settings UI catalogues them under
// Source="builtin" (v0.6 — was Source="analysis" pre-refactor
// for ListTools categorisation; the new label is more
// accurate).
//
// Phase 2b of the refactor: defines the descriptors. No
// consumer yet — executeTool() still has its hand-coded
// case branches for these names. Phase 2f migrates the outer
// dispatcher to descriptor-based routing and the case
// branches go away.

package agent

import (
	"context"

	"github.com/nlink-jp/shell-agent-v2/internal/chat"
)

// builtinDescriptors returns the four builtin tools. They
// are dispatched at the outer-executeTool level today
// (case-label intercepts before the analysis-tools branch);
// after Phase 2f they flow through the same dispatchDescriptor
// path as analysis tools.
//
// Source = "builtin" labels them as v0.6 separates them from
// the analysis bucket. The Settings UI groups them under a
// "Builtin" tab; pre-refactor they were lumped under
// "analysis" because that's where the ListTools section
// lived.
func (a *Agent) builtinDescriptors() []ToolDescriptor {
	out := []ToolDescriptor{
		{
			Name:        "resolve_date",
			Description: "Resolve relative date expressions to absolute dates. Use when you need to calculate dates like 'last Thursday', '3 weeks ago', 'first Monday of last month'.",
			Parameters:  chat.ResolveDateToolDef(),
			Category:    "read",
			Source:      "builtin",
			MITLDefault: false,
			HideUntilDataLoaded: false,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return chat.ResolveDate(args)
			}),
		},
		{
			Name:        "list_objects",
			Description: "List all objects (images, files, reports, markdown attachments) in the current session. Returns ID, type, filename, size, and creation time. For text-bearing types (markdown / report) the output also includes Lines and Tokens so you can plan whether to read the whole document via get_text or summarise via analyze_text without first having to count lines yourself.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type_filter": map[string]any{
						"type":        "string",
						"enum":        []string{"image", "blob", "report", "markdown", "all"},
						"description": "Filter by object type (default: all)",
					},
				},
			},
			Category:            "read",
			Source:              "builtin",
			MITLDefault:         false,
			HideUntilDataLoaded: false,
			Handle: wrapStringHandler(func(_ context.Context, args string) string {
				return a.toolListObjects(args)
			}),
		},
		{
			Name:        "get_object",
			Description: "Retrieve an object by ID. For images, returns a marker that will be resolved to the actual image. For text/data, returns the content directly.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Object ID (hex string, currently 32 chars; legacy 12-char IDs continue to work)",
					},
				},
				"required": []string{"id"},
			},
			Category:            "read",
			Source:              "builtin",
			MITLDefault:         false,
			HideUntilDataLoaded: false,
			Handle: wrapStringHandler(func(_ context.Context, args string) string {
				return a.toolGetObject(args)
			}),
		},
		{
			Name:        "register_object",
			Description: "Register a file already present in the session work directory ($SHELL_AGENT_WORK_DIR — same physical path that the sandbox sees as /work) into the central object store, returning an object:<ID> reference the chat can render. Use this to surface artefacts produced by shell tools (e.g. generate_image): write to $SHELL_AGENT_WORK_DIR from the shell tool, then call this with the same filename. For artefacts produced by sandbox_run_python / sandbox_run_shell, prefer sandbox_register_object (both end up reading from the same physical directory). Design: docs/en/history/work-dir-shell-bridge.md.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path inside the work directory. Relative paths only; '..' traversal is rejected. e.g. 'sunset.png'",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Human-readable name for the object (shown in the Data panel). Falls back to the basename if omitted.",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"image", "blob", "report", "markdown"},
						"description": "Object type. If omitted, inferred from the file's MIME (image/* → image, text/markdown → report, otherwise blob). Pass \"markdown\" explicitly when the file is user-staged source material rather than an agent-generated report.",
					},
				},
				"required": []string{"path"},
			},
			Category:            "write",
			Source:              "builtin",
			MITLDefault:         false, // matches existing analysisToolMITLDefault entry
			HideUntilDataLoaded: false,
			// register-object's signature already matches
			// ToolDescriptor.Handle exactly (string, ActivityEventStatus),
			// so no wrapper helper is needed — just bind the closure.
			Handle: func(_ context.Context, args string) (string, ActivityEventStatus) {
				return a.toolRegisterObject(args)
			},
		},
	}
	// ADR-0019: remember-fact is always registered as a descriptor
	// so the dispatcher can route the call if the LLM ever emits
	// the name. Whether the LLM SEES it in its tool list is decided
	// at buildToolDefs time via autoExtractEnabled() — the live
	// gate respects per-profile and /model switches without rebuilding
	// the descriptor cache.
	out = append(out, ToolDescriptor{
		Name:        "remember_fact",
		Description: "Save a fact about the user to memory so it persists across turns and (for preference/decision) sessions. Use when the user states a stable preference, makes an explicit decision, or shares a fact about themselves that will matter later. Do NOT use for transient context or anything already obvious from the conversation history. Aim for at most a few calls per session.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact": map[string]any{
					"type":        "string",
					"description": "Concise statement of the fact (one sentence). Written in the user's own words when possible.",
				},
				"category": map[string]any{
					"type":        "string",
					"enum":        []string{"preference", "decision", "fact", "context"},
					"description": "preference/decision persist cross-session in Global Memory; fact/context stay in the current session's memory.",
				},
			},
			"required": []string{"fact", "category"},
		},
		Category:            "write",
		Source:              "builtin",
		MITLDefault:         false,
		HideUntilDataLoaded: false,
		Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
			return a.toolRememberFact(args)
		}),
	})
	return out
}
