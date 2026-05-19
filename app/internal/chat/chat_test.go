package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestRenderTemporalPrefix_ContainsDateTimeWeekday(t *testing.T) {
	ts := time.Date(2026, 5, 20, 12, 34, 56, 0, time.UTC)
	got := RenderTemporalPrefix(ts, time.UTC)
	for _, want := range []string{"2026-05-20", "Wednesday", "12:34:56", "UTC"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderTemporalPrefix output missing %q\nfull: %s", want, got)
		}
	}
	if !strings.HasPrefix(got, "[Time:") || !strings.HasSuffix(got, "]") {
		t.Errorf("RenderTemporalPrefix output not in expected [Time: …] form: %s", got)
	}
}

// TestRenderTemporalPrefix_ByteStable is the load-bearing invariant
// for ADR-0017: two renders of the same timestamp must produce
// byte-identical strings, otherwise the server-side KV cache can't
// reuse the prefix across requests.
func TestRenderTemporalPrefix_ByteStable(t *testing.T) {
	ts := time.Date(2026, 5, 20, 12, 34, 56, 789_000_000, time.UTC)
	a := RenderTemporalPrefix(ts, time.UTC)
	b := RenderTemporalPrefix(ts, time.UTC)
	if a != b {
		t.Errorf("non-deterministic output for the same input:\nA: %s\nB: %s", a, b)
	}
}

// TestRenderTemporalPrefix_NilLocFallback ensures defensive callers
// (legacy contextbuild paths without an explicit Loc) get a sane
// default instead of a panic.
func TestRenderTemporalPrefix_NilLocFallback(t *testing.T) {
	ts := time.Now()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RenderTemporalPrefix panicked with nil loc: %v", r)
		}
	}()
	_ = RenderTemporalPrefix(ts, nil)
}

func TestBuildMessages(t *testing.T) {
	e := New("You are a helpful assistant.")
	session := &memory.Session{
		Records: []memory.Record{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}

	msgs, err := e.BuildMessages(session, "", "")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("first message role = %v, want system", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "helpful assistant") {
		t.Error("system prompt not included")
	}
	// v0.13.0 (ADR-0017): temporal context is no longer injected
	// into the system prompt — it travels with each user record via
	// contextbuild's UserRecordTemporalPrefix hook.
	for _, banned := range []string{"Current date and time", "Yesterday:"} {
		if strings.Contains(msgs[0].Content, banned) {
			t.Errorf("system prompt should not contain %q; got:\n%s", banned, msgs[0].Content)
		}
	}
}

func TestBuildSystemPrompt_IncludesAllChannels(t *testing.T) {
	e := New("BASE PROMPT")
	e.SetLocation("Tokyo")
	got := e.BuildSystemPrompt("- pinned fact (learned 2026-04-15)", "", "- finding from 2026-04-20", "")
	// v0.13.0 (ADR-0017): "Current date and time" is no longer
	// expected here — temporal context now lives on user records.
	for _, want := range []string{
		"BASE PROMPT",
		"Tokyo",
		"pinned fact",
		"learned 2026-04-15",
		"finding from 2026-04-20",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q\nfull: %s", want, got)
		}
	}
}

// TestBuildSystemPrompt_NoTemporalLines is the ADR-0017 §3.3
// invariant on the system prompt side: no `Current date and time:`
// and no `Yesterday:` lines should ever appear in the assembled
// system block. Their presence would re-break llama.cpp prefix
// caching by introducing a per-call volatile region.
func TestBuildSystemPrompt_NoTemporalLines(t *testing.T) {
	e := New("BASE PROMPT")
	e.SetLocation("Tokyo")
	e.SetSandboxEnabled(true)
	got := e.BuildSystemPrompt("global", "session", "findings", "system rules")
	for _, banned := range []string{
		"Current date and time",
		"Yesterday:",
		"[Time:",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("system prompt unexpectedly contains %q\nfull: %s", banned, got)
		}
	}
}

func TestBuildSystemPrompt_SystemRulesInjection(t *testing.T) {
	e := New("BASE PROMPT")

	// Empty rules: no marker, no preamble sentence.
	got := e.BuildSystemPrompt("", "", "", "")
	for _, unwanted := range []string{"<system_rules>", "standing instructions"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("empty rules should not inject %q\nfull: %s", unwanted, got)
		}
	}

	// Short rules: marker + preamble + content present.
	got = e.BuildSystemPrompt("", "", "", "be terse")
	for _, want := range []string{
		"standing instructions",
		"<system_rules>\nbe terse\n</system_rules>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in prompt\nfull: %s", want, got)
		}
	}

	// Surrounding whitespace is trimmed inside the envelope.
	got = e.BuildSystemPrompt("", "", "", "\n  rule\n  ")
	if !strings.Contains(got, "<system_rules>\nrule\n</system_rules>") {
		t.Errorf("expected TrimSpace inside envelope\nfull: %s", got)
	}
	if strings.Contains(got, "<system_rules>\n\n") || strings.Contains(got, "  \n</system_rules>") {
		t.Errorf("trimmed rules should not retain padding\nfull: %s", got)
	}
}

func TestBuildSystemPrompt_SystemRulesPosition(t *testing.T) {
	// Structural invariant: rules sit between the base prompt and
	// the Global Memory header. Mechanical guard against future
	// drift moving the injection point. ADR-0012 §4.4.
	//
	// v0.13.0 (ADR-0017): the "rules before temporal context"
	// assertion is gone because temporal context no longer lives
	// in the system prompt.
	e := New("BASE PROMPT MARKER")
	got := e.BuildSystemPrompt("- pinned fact", "", "", "RULE BODY")

	idxBase := strings.Index(got, "BASE PROMPT MARKER")
	idxRules := strings.Index(got, "<system_rules>")
	idxGlobal := strings.Index(got, "Important facts you remember")

	if idxBase < 0 || idxRules < 0 || idxGlobal < 0 {
		t.Fatalf("missing anchors: base=%d rules=%d global=%d\n%s",
			idxBase, idxRules, idxGlobal, got)
	}
	if !(idxBase < idxRules) {
		t.Errorf("rules must come after base prompt: base=%d rules=%d", idxBase, idxRules)
	}
	if !(idxRules < idxGlobal) {
		t.Errorf("rules must come before Global Memory: rules=%d global=%d", idxRules, idxGlobal)
	}
}

func TestBuildSystemPrompt_SandboxGuidanceConditional(t *testing.T) {
	e := New("base prompt")

	// Off by default: no sandbox section.
	if got := e.BuildSystemPrompt("", "", "", ""); strings.Contains(got, "sandbox-run-shell") {
		t.Error("sandbox guidance should be absent when SetSandboxEnabled was not called")
	}

	e.SetSandboxEnabled(true)
	got := e.BuildSystemPrompt("", "", "", "")
	for _, want := range []string{"sandbox-run-shell", "sandbox-run-python", "sandbox-write-file", "sandbox-copy-object", "sandbox-register-object", "sandbox-info"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected sandbox guidance to mention %q", want)
		}
	}

	// And turning it back off removes it.
	e.SetSandboxEnabled(false)
	if got := e.BuildSystemPrompt("", "", "", ""); strings.Contains(got, "sandbox-run-shell") {
		t.Error("disabling should remove the sandbox guidance")
	}
}

func TestWrapUserToolContent_RotatesWithSystemPrompt(t *testing.T) {
	e := New("base")
	_ = e.BuildSystemPrompt("", "", "", "")
	wrapped1, err := e.WrapUserToolContent("hi")
	if err != nil {
		t.Fatalf("WrapUserToolContent: %v", err)
	}
	if wrapped1 == "hi" {
		t.Error("expected wrap to add markers")
	}
	// Building the system prompt again rotates the guard tag, so a
	// previously-wrapped string from the old tag would no longer match.
	_ = e.BuildSystemPrompt("", "", "", "")
	wrapped2, err := e.WrapUserToolContent("hi")
	if err != nil {
		t.Fatalf("WrapUserToolContent (2): %v", err)
	}
	if wrapped1 == wrapped2 {
		t.Error("guard tag should rotate per BuildSystemPrompt call")
	}
}

// TestStripCurrentGuardTags pins the v0.2.0 fix for the
// guard-envelope leak: when Vertex Gemini quoted a wrapped
// tool result and reproduced the `<user_data_NONCE>` envelope
// verbatim, the chat pane would render the marker as text. The
// agent loop now scrubs those tags using the *current turn's*
// nonce so unrelated user prose mentioning a similar-looking
// placeholder isn't mangled.
func TestStripCurrentGuardTags(t *testing.T) {
	e := New("base")
	_ = e.BuildSystemPrompt("", "", "", "")
	// Simulate a wrapped tool result that the LLM then quoted.
	wrapped, err := e.WrapUserToolContent("payload body")
	if err != nil {
		t.Fatalf("WrapUserToolContent: %v", err)
	}
	leaked := "Here is what the tool said: " + wrapped + " — that's the result."
	cleaned := e.StripCurrentGuardTags(leaked)
	if strings.Contains(cleaned, "user_data_") {
		t.Errorf("current-turn guard tag should be stripped: %q", cleaned)
	}
	if !strings.Contains(cleaned, "payload body") {
		t.Errorf("payload body should survive: %q", cleaned)
	}

	// A different turn's tag must NOT be touched (precision check) —
	// rotate the engine's tag, then ask it to strip a previous-turn
	// envelope. The previous nonce no longer matches, so the text
	// survives. This is the property that distinguishes targeted
	// nonce-stripping from a generic regex sweep over the family.
	staleEnvelope := wrapped // produced under the now-rotated tag
	_ = e.BuildSystemPrompt("", "", "", "")
	stillThere := e.StripCurrentGuardTags(staleEnvelope)
	if !strings.Contains(stillThere, "user_data_") {
		t.Errorf("previous-turn envelope should be left alone after rotation: %q", stillThere)
	}
}

func TestBuildMessagesWithFindings(t *testing.T) {
	e := New("test")
	session := &memory.Session{}

	msgs, err := e.BuildMessages(session, "", "Sales peak in April")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !strings.Contains(msgs[0].Content, "Sales peak in April") {
		t.Error("findings context not included in system prompt")
	}
}

// v0.2.0: BuildMessagesWithBudget was deleted along with the
// v1 destructive-compaction code path. The contextbuild package
// (internal/contextbuild) is now the only message-building
// code path; its tests live in builder_test.go and exercise the
// non-destructive derivation model.
