package agent

import (
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/mcp"
)

// TestIsToolMITLRequired_AnalysisDefaultsMatchTable confirms that the
// per-analysis-tool defaults exposed via IsToolMITLRequired stay in
// sync with analysisToolMITLDefault. Before security-hardening-2.md
// H1+H2 these defaults existed only in inline switch statements that
// the dispatcher bypassed; now they are the source of truth and the
// Settings → Tools toggle reads from the same table.
func TestIsToolMITLRequired_AnalysisDefaultsMatchTable(t *testing.T) {
	a := New(config.Default())
	for name, want := range analysisToolMITLDefault {
		if got := a.IsToolMITLRequired(name); got != want {
			t.Errorf("IsToolMITLRequired(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestIsToolMITLRequired_OverrideRespectedForAnalysis pins the bug
// the user spotted during the design review: the Settings UI MITL
// toggle for analysis tools used to have no effect because the
// dispatcher never consulted MITLOverrides. Now ON / OFF both win
// over the analysisToolMITLDefault entry.
func TestIsToolMITLRequired_OverrideRespectedForAnalysis(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.MITLOverrides = map[string]bool{
		"load-data":      false, // forced OFF, even though default is true
		"create-report":  true,  // forced ON, even though default is false
	}
	a := New(cfg)
	if a.IsToolMITLRequired("load-data") {
		t.Error("MITLOverrides[load-data]=false should disable MITL")
	}
	if !a.IsToolMITLRequired("create-report") {
		t.Error("MITLOverrides[create-report]=true should enable MITL")
	}
}

// TestIsToolMITLRequired_PriorityOrder verifies the documented
// priority: override > mcp/sandbox prefix > analysis default >
// shell fallback (false).
func TestIsToolMITLRequired_PriorityOrder(t *testing.T) {
	cfg := config.Default()
	// Override must beat the mcp__ prefix default.
	cfg.Tools.MITLOverrides = map[string]bool{
		"mcp__foo__bar": false,
	}
	a := New(cfg)
	if a.IsToolMITLRequired("mcp__foo__bar") {
		t.Error("override should win over mcp__ prefix")
	}
	// Bare unknown shell tool — fall through to false.
	if a.IsToolMITLRequired("some-shell-tool") {
		t.Error("unknown shell tool should default to false")
	}
}

// --- splitMCPName -----------------------------------------------------

func TestSplitMCPName_NaiveSplit(t *testing.T) {
	guardians := map[string]*mcp.Guardian{
		"weather": nil,
	}
	g, tool, ok := splitMCPName("weather__forecast", guardians)
	if !ok {
		t.Fatal("expected ok=true for naive split")
	}
	if g != "weather" || tool != "forecast" {
		t.Errorf("got (%q, %q), want (weather, forecast)", g, tool)
	}
}

// TestSplitMCPName_GuardianContainsDoubleUnderscore is the bug case:
// the naive SplitN("__", 2) parse mis-splits when a guardian's name
// itself contains "__". With config-side validation this can no
// longer arrive in practice, but we keep the longest-prefix fallback
// so the parser stays tolerant if someone bypasses the validator
// (e.g. legacy on-disk config from before validation existed).
func TestSplitMCPName_GuardianContainsDoubleUnderscore(t *testing.T) {
	guardians := map[string]*mcp.Guardian{
		"weather__pro": nil,
	}
	g, tool, ok := splitMCPName("weather__pro__forecast", guardians)
	if !ok {
		t.Fatal("expected longest-prefix fallback to succeed")
	}
	if g != "weather__pro" {
		t.Errorf("guardian = %q, want weather__pro", g)
	}
	if tool != "forecast" {
		t.Errorf("tool = %q, want forecast", tool)
	}
}

// TestSplitMCPName_ToolContainsDoubleUnderscore: tool name has "__",
// guardian name does not. Naive split picks the wrong boundary;
// fallback recovers via prefix match against the registered guardian.
func TestSplitMCPName_ToolContainsDoubleUnderscore(t *testing.T) {
	guardians := map[string]*mcp.Guardian{
		"weather": nil,
	}
	g, tool, ok := splitMCPName("weather__forecast__hourly", guardians)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if g != "weather" {
		t.Errorf("guardian = %q, want weather", g)
	}
	// Naive split: tool=forecast__hourly. Either form is acceptable
	// (the fallback only kicks in when the naive split's guardian
	// isn't registered) — assert that the tool is the full
	// remainder after the first `__`, since `weather` IS registered.
	if tool != "forecast__hourly" {
		t.Errorf("tool = %q, want forecast__hourly", tool)
	}
}

// TestSplitMCPName_UnknownGuardian falls all the way through and
// reports failure rather than guessing.
func TestSplitMCPName_UnknownGuardian(t *testing.T) {
	guardians := map[string]*mcp.Guardian{
		"weather": nil,
	}
	if _, _, ok := splitMCPName("ghosts__do_things", guardians); ok {
		t.Error("expected ok=false for unknown guardian")
	}
}

// TestSplitMCPName_EmptyToolNameRejected: `mcp__weather__` (no tool
// after the trailing separator) must not be silently routed.
func TestSplitMCPName_EmptyToolNameRejected(t *testing.T) {
	guardians := map[string]*mcp.Guardian{
		"weather": nil,
	}
	if _, _, ok := splitMCPName("weather__", guardians); ok {
		t.Error("empty tool name should be rejected")
	}
}

// TestValidGuardianName covers the registration-time regex. The set
// of allowed characters is intentionally narrow so the
// `mcp__<guardian>__<tool>` flat namespace stays unambiguous.
func TestValidGuardianName(t *testing.T) {
	for _, name := range []string{"weather", "Weather-Pro", "abc123"} {
		if !validGuardianName.MatchString(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range []string{"", "with space", "weather__pro", "no_underscore", "dot.name", "slash/name"} {
		if validGuardianName.MatchString(name) {
			t.Errorf("%q should be rejected", name)
		}
	}
}
