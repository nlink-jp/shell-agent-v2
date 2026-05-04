package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

func TestBuildTemporalContext(t *testing.T) {
	ctx := buildTemporalContext()
	now := time.Now()

	if !strings.Contains(ctx, now.Format("2006-01-02")) {
		t.Errorf("temporal context missing today's date: %s", ctx)
	}
	if !strings.Contains(ctx, now.Format("Monday")) {
		t.Errorf("temporal context missing day of week: %s", ctx)
	}
	if !strings.Contains(ctx, "Yesterday:") {
		t.Errorf("temporal context missing yesterday: %s", ctx)
	}
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
	if !strings.Contains(msgs[0].Content, "Current date and time") {
		t.Error("temporal context not included")
	}
}

func TestBuildSystemPrompt_IncludesAllChannels(t *testing.T) {
	e := New("BASE PROMPT")
	e.SetLocation("Tokyo")
	got := e.BuildSystemPrompt("- pinned fact (learned 2026-04-15)", "", "- finding from 2026-04-20")
	for _, want := range []string{
		"BASE PROMPT",
		"Tokyo",
		"Current date and time",
		"pinned fact",
		"learned 2026-04-15",
		"finding from 2026-04-20",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q\nfull: %s", want, got)
		}
	}
}

func TestBuildSystemPrompt_SandboxGuidanceConditional(t *testing.T) {
	e := New("base prompt")

	// Off by default: no sandbox section.
	if got := e.BuildSystemPrompt("", "", ""); strings.Contains(got, "sandbox-run-shell") {
		t.Error("sandbox guidance should be absent when SetSandboxEnabled was not called")
	}

	e.SetSandboxEnabled(true)
	got := e.BuildSystemPrompt("", "", "")
	for _, want := range []string{"sandbox-run-shell", "sandbox-run-python", "sandbox-write-file", "sandbox-copy-object", "sandbox-register-object", "sandbox-info"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected sandbox guidance to mention %q", want)
		}
	}

	// And turning it back off removes it.
	e.SetSandboxEnabled(false)
	if got := e.BuildSystemPrompt("", "", ""); strings.Contains(got, "sandbox-run-shell") {
		t.Error("disabling should remove the sandbox guidance")
	}
}

func TestWrapUserToolContent_RotatesWithSystemPrompt(t *testing.T) {
	e := New("base")
	_ = e.BuildSystemPrompt("", "", "")
	wrapped1, err := e.WrapUserToolContent("hi")
	if err != nil {
		t.Fatalf("WrapUserToolContent: %v", err)
	}
	if wrapped1 == "hi" {
		t.Error("expected wrap to add markers")
	}
	// Building the system prompt again rotates the guard tag, so a
	// previously-wrapped string from the old tag would no longer match.
	_ = e.BuildSystemPrompt("", "", "")
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
	_ = e.BuildSystemPrompt("", "", "")
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
	_ = e.BuildSystemPrompt("", "", "")
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
