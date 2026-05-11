// tool_descriptor_structural_test.go — invariants over the
// descriptor registry that previously had to be enforced by
// hand. The v0.5 → v0.5.1 manual smoke caught two of these
// (Settings tab missing a tool; the other a stale MITL map
// entry); v0.6 makes them structurally checkable instead of
// review-checkable.
//
// These tests intentionally exercise the registry as data, not
// behaviour — they should be cheap to run and easy to reason
// about. If a future descriptor edit breaks one of them, the
// failure message names the offending descriptor so the fix
// is local.

package agent

import (
	"context"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/llm"
)

// TestToolDescriptors_UniqueNames verifies no two descriptors
// share the same Name. Pre-refactor the same invariant lived
// implicitly in hand-written switch case-labels: a duplicate
// would have shadowed the earlier branch silently. With the
// descriptor index map a duplicate would clobber the earlier
// entry's index — `dispatchDescriptor` would still work but
// the LLM tool list would carry two entries with the same
// name, which most providers reject.
func TestToolDescriptors_UniqueNames(t *testing.T) {
	a := agentForToolDefs(t)
	seen := make(map[string]string, len(a.toolDescriptors))
	for _, d := range a.toolDescriptors {
		if prev, ok := seen[d.Name]; ok {
			t.Errorf("duplicate descriptor Name %q: registered as Source=%q and again as Source=%q",
				d.Name, prev, d.Source)
			continue
		}
		seen[d.Name] = d.Source
	}
}

// TestToolDescriptors_AllHaveHandlers asserts every descriptor
// has a non-nil Handle. A nil Handle would crash the
// dispatcher; this test catches that at registration time
// rather than at the first user-triggered call.
func TestToolDescriptors_AllHaveHandlers(t *testing.T) {
	a := agentForToolDefs(t)
	for _, d := range a.toolDescriptors {
		if d.Handle == nil {
			t.Errorf("descriptor %q (source=%q) has nil Handle", d.Name, d.Source)
		}
	}
}

// TestToolDescriptors_RequiredFields checks the metadata
// fields the Settings UI and dispatcher both depend on.
// Empty Name / Description / Category / Source would slip
// past the compiler but break Settings rendering or MITL
// routing at runtime.
func TestToolDescriptors_RequiredFields(t *testing.T) {
	a := agentForToolDefs(t)
	validSources := map[string]bool{"analysis": true, "builtin": true, "sandbox": true}
	validCategories := map[string]bool{"read": true, "write": true, "execute": true}
	for _, d := range a.toolDescriptors {
		if d.Name == "" {
			t.Error("descriptor with empty Name")
			continue
		}
		if d.Description == "" {
			t.Errorf("descriptor %q has empty Description", d.Name)
		}
		if !validSources[d.Source] {
			t.Errorf("descriptor %q has invalid Source %q (want one of analysis|builtin|sandbox)", d.Name, d.Source)
		}
		if !validCategories[d.Category] {
			t.Errorf("descriptor %q has invalid Category %q (want one of read|write|execute)", d.Name, d.Category)
		}
	}
}

// TestDispatchDescriptor_RoutesAllNamesInLLMToolDefs ensures
// every tool surfaced via descriptorToolDefs() can also be
// dispatched. Pre-refactor the LLM tool-def list and the
// switch dispatcher were maintained as parallel surfaces; the
// v0.5.0 → v0.5.1 manual smoke caught the case where the LLM
// could call analyze-text but the dispatcher hadn't been
// wired up yet ("unknown tool"). Now both derive from the
// same descriptor list, so this test is automatically
// satisfied as long as descriptorToolDefs() and
// dispatchDescriptor() both consult a.toolDescriptors.
func TestDispatchDescriptor_RoutesAllNamesInLLMToolDefs(t *testing.T) {
	a := agentForToolDefs(t)
	// hasData=true so data-gated descriptors surface; legacyMode=false
	// so all descriptors flow through (no hide-until-data-loaded
	// gate). This is the broadest exposure mode.
	tools := a.descriptorToolDefs(true, false)
	for _, td := range tools {
		_, ok := a.toolDescriptorByName(td.Name)
		if !ok {
			t.Errorf("LLM tool def %q has no descriptor — dispatchDescriptor cannot route it", td.Name)
		}
	}
}

// TestListTools_ContainsEveryVisibleDescriptor pins that the
// Settings → Tools UI catalogue (a.ListTools()) lists every
// descriptor that the LLM can see (a.descriptorToolDefs()).
// Pre-refactor the two surfaces were hand-maintained and the
// v0.5.1 release notes documented a "Settings missing
// analyze-text" drift bug from exactly this gap. Same
// gating: hasData=true / legacyMode=false so the broadest
// set is checked.
func TestListTools_ContainsEveryVisibleDescriptor(t *testing.T) {
	a := agentForToolDefs(t)
	want := make(map[string]bool)
	for _, td := range a.descriptorToolDefs(true, false) {
		want[td.Name] = false
	}
	for _, item := range a.ListTools() {
		if _, expected := want[item.Name]; expected {
			want[item.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("ListTools missing %q (LLM sees it via descriptorToolDefs but Settings UI does not)", name)
		}
	}
}

// TestDispatchDescriptor_HandlesUnknownAsFallthrough ensures
// the dispatcher returns handled=false (rather than panicking
// or returning a fake error) for an unknown tool name. The
// outer executeTool relies on this to fall through to MCP
// and shell-tool sources.
func TestDispatchDescriptor_HandlesUnknownAsFallthrough(t *testing.T) {
	a := agentForToolDefs(t)
	_, _, handled := a.dispatchDescriptor(context.Background(), llm.ToolCall{Name: "no-such-tool", Arguments: "{}"})
	if handled {
		t.Error("dispatchDescriptor returned handled=true for unknown tool — outer dispatcher fall-through is broken")
	}
}
