// tool_descriptor.go — single source of truth for analysis +
// builtin + sandbox tool definitions. Replaces the v0.5
// six-parallel-list pattern that caused drift bugs every
// time a new tool was added (each name had to appear in
// analysisToolMITLDefault, analysisToolMITLCategory,
// analysisTools, executeAnalysisTool, executeTool's outer
// case-label, and ListTools — and the v0.5.0 → v0.5.1 manual
// smoke caught two of those forgotten).
//
// Design: docs/en/tool-registry-refactor.md.
//
// Phase 1 of the refactor introduces only the type and the
// per-agent storage / helper. No view function consumes the
// descriptor list yet — that arrives incrementally in
// Phase 2 sub-commits.

package agent

import (
	"context"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
)

// ToolDescriptor is the single source of truth for a tool —
// the same value backs the LLM tool def, the Settings →
// Tools UI entry, the MITL default, and the dispatch
// handler. The fields are deliberately a flat struct rather
// than a builder pattern: descriptor lists are constructed
// as plain slice literals in `tool_descriptors_*.go` files,
// one entry per tool, easy to read and easy to grep.
type ToolDescriptor struct {
	// --- Identity ---
	// Name is the canonical tool name the LLM uses to invoke
	// the tool. Must be unique across all sources for a
	// given Agent — TestToolDescriptors_UniqueNames enforces
	// this at test time.
	Name string

	// --- LLM-facing ---
	// Description is what the LLM sees in its tool list.
	// Single source of truth: Settings → Tools UI displays
	// the same string.
	Description string

	// Parameters is the JSON Schema describing the tool's
	// arguments. Conventionally a `map[string]any` shaped
	// per the LLM provider's expectations (Vertex Gemini and
	// OpenAI-style local backends both accept this shape).
	Parameters any

	// --- UI / classification ---
	// Category is "read" | "write" | "execute" — drives the
	// generic MITL confirmation dialog. Specific MITL
	// categories ("sql_preview", "analysis_plan") flow
	// through MITLCategoryOverride; Category remains the
	// fallback / default.
	Category string

	// Source is "analysis" | "builtin" | "sandbox" — purely
	// classification metadata for the Settings UI to group
	// entries by origin. The dispatcher does NOT branch on
	// Source — it dispatches via Handle directly.
	Source string

	// --- MITL ---
	// MITLDefault is the per-tool default for the
	// Settings → Tools toggle. Consulted by
	// IsToolMITLRequired() when no per-tool override is set.
	MITLDefault bool

	// MITLCategoryOverride is non-empty when the UI should
	// render a specialised confirmation dialog instead of
	// the generic Category one. Currently used for:
	//   - "sql_preview"   (query-sql — SQL syntax-highlighted preview)
	//   - "analysis_plan" (analyze-data — analysis prompt editor)
	// New specialised UIs are added by setting a new
	// override value here and teaching the frontend to
	// render it; the dispatcher does not change.
	MITLCategoryOverride string

	// --- Visibility ---
	// HideUntilDataLoaded is true for tools that legacy
	// mode hides until the analysis engine has loaded at
	// least one table. Mirrors the existing config flag
	// `cfg.Tools.HideAnalysisToolsUntilDataLoaded` so the
	// policy and its consumer line up. Most tools leave
	// this false (always visible).
	HideUntilDataLoaded bool

	// --- Dispatch ---
	// Handle is the tool's executor. Closures capture the
	// *Agent at descriptor-construction time so the
	// descriptor list can be a method on *Agent and reuse
	// existing toolXxx() handlers without signature
	// changes. The MITL gate is applied centrally in the
	// outer dispatcher — Handle is invoked only after the
	// gate passes.
	Handle func(ctx context.Context, args string) (string, ActivityEventStatus)
}

// toolDescriptorByName returns the descriptor for a given
// tool name and a present-flag, or the zero value + false
// when no descriptor matches. O(1) via toolDescriptorIndex.
//
// Used by the Phase 2 view functions
// (toolDefsForLLM / dispatchDescriptor / ListTools etc.)
// to locate the right descriptor without scanning the
// slice. In Phase 1 there is no consumer; the helper sits
// here so subsequent commits don't need to also touch the
// agent struct.
func (a *Agent) toolDescriptorByName(name string) (ToolDescriptor, bool) {
	idx, ok := a.toolDescriptorIndex[name]
	if !ok || idx < 0 || idx >= len(a.toolDescriptors) {
		return ToolDescriptor{}, false
	}
	return a.toolDescriptors[idx], true
}

// rebuildToolDescriptorIndex rebuilds the name→index map
// from the descriptor slice. Called from New() after the
// descriptor list is populated by the per-source builders.
// Idempotent — safe to call again if a future code path
// mutates the slice (none does today).
func (a *Agent) rebuildToolDescriptorIndex() {
	a.toolDescriptorIndex = make(map[string]int, len(a.toolDescriptors))
	for i, d := range a.toolDescriptors {
		a.toolDescriptorIndex[d.Name] = i
	}
}

// wrapErrHandler adapts a toolXxx-style handler returning
// (string, error) to ToolDescriptor.Handle's signature
// (string, ActivityEventStatus). Used by every analysis +
// builtin descriptor so the existing toolXxx implementations
// stay unchanged. The "Error: %v" prefix matches the
// formatting that executeAnalysisTool used to apply at the
// outer dispatcher level (agent.go's "Error: %v" path).
func wrapErrHandler(fn func(ctx context.Context, args string) (string, error)) func(ctx context.Context, args string) (string, ActivityEventStatus) {
	return func(ctx context.Context, args string) (string, ActivityEventStatus) {
		result, err := fn(ctx, args)
		if err != nil {
			return "Error: " + err.Error(), ActivityStatusError
		}
		return result, ActivityStatusSuccess
	}
}

// wrapStringHandler adapts a toolXxx-style handler that
// returns just `string` (no error) to ToolDescriptor.Handle.
// Used for the no-error builtin handlers (toolListObjects,
// toolGetObject) that surface their own error text in the
// returned string and never need a status flip. Always
// reports ActivityStatusSuccess — error reporting is the
// caller's responsibility via the returned message body.
func wrapStringHandler(fn func(ctx context.Context, args string) string) func(ctx context.Context, args string) (string, ActivityEventStatus) {
	return func(ctx context.Context, args string) (string, ActivityEventStatus) {
		return fn(ctx, args), ActivityStatusSuccess
	}
}

// descriptorToolDefs derives the LLM tool-def slice from the
// agent's descriptor list, applying the same visibility
// filters (analysis-engine presence, legacy data-gating)
// that the pre-refactor `analysisTools()` builder applied.
//
// Replaces analysisTools() and the hand-coded resolve-date
// entry that buildToolDefs() prepended in v0.5. Sandbox
// tools are not included here — Phase 3 migrates them.
func (a *Agent) descriptorToolDefs(hasData, legacyMode bool) []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(a.toolDescriptors))
	for _, d := range a.toolDescriptors {
		// Analysis-source tools depend on a.analysis being
		// non-nil. Pre-refactor: buildToolDefs guarded the
		// analysisTools() call with `if a.analysis != nil`,
		// so calling analyse-data with no engine never even
		// surfaced as an LLM-visible option. Preserve that.
		if d.Source == "analysis" && a.analysis == nil {
			continue
		}
		// Data gating: legacy hide-until-data-loaded mode
		// hides data-dependent tools until the engine has at
		// least one table.
		if d.HideUntilDataLoaded && legacyMode && !hasData {
			continue
		}
		out = append(out, llm.ToolDef{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Parameters,
		})
	}
	return out
}
