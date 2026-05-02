package agent

import (
	"strings"
	"testing"
)

func TestPushToolCallTrace_KeepsOnlyLastWindow(t *testing.T) {
	var buf []toolCallTrace
	for range 5 {
		buf = pushToolCallTrace(buf, toolCallTrace{Name: "x", Status: ActivityStatusError})
	}
	if len(buf) != recentToolWindow {
		t.Fatalf("len=%d, want %d", len(buf), recentToolWindow)
	}
}

func TestPushToolCallTrace_BelowWindowGrows(t *testing.T) {
	var buf []toolCallTrace
	buf = pushToolCallTrace(buf, toolCallTrace{Name: "a", Status: ActivityStatusSuccess})
	buf = pushToolCallTrace(buf, toolCallTrace{Name: "b", Status: ActivityStatusError})
	if len(buf) != 2 {
		t.Errorf("len=%d, want 2", len(buf))
	}
}

func TestDetectStuckLoop(t *testing.T) {
	cases := []struct {
		name     string
		buf      []toolCallTrace
		wantOK   bool
		wantName string
	}{
		{
			"empty",
			nil,
			false, "",
		},
		{
			"under window",
			[]toolCallTrace{
				{"x", ActivityStatusError},
				{"x", ActivityStatusError},
			},
			false, "",
		},
		{
			"all same name all error",
			[]toolCallTrace{
				{"sandbox-run-python", ActivityStatusError},
				{"sandbox-run-python", ActivityStatusError},
				{"sandbox-run-python", ActivityStatusError},
			},
			true, "sandbox-run-python",
		},
		{
			"all same name but one success",
			[]toolCallTrace{
				{"x", ActivityStatusError},
				{"x", ActivityStatusSuccess},
				{"x", ActivityStatusError},
			},
			false, "",
		},
		{
			"all error but mixed names",
			[]toolCallTrace{
				{"a", ActivityStatusError},
				{"b", ActivityStatusError},
				{"a", ActivityStatusError},
			},
			false, "",
		},
		{
			"all same name all success — not stuck, just running",
			[]toolCallTrace{
				{"x", ActivityStatusSuccess},
				{"x", ActivityStatusSuccess},
				{"x", ActivityStatusSuccess},
			},
			false, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotOK := detectStuckLoop(tc.buf)
			if gotOK != tc.wantOK || gotName != tc.wantName {
				t.Errorf("detectStuckLoop = (%q, %v), want (%q, %v)", gotName, gotOK, tc.wantName, tc.wantOK)
			}
		})
	}
}

func TestLoopHintFor_MentionsToolName(t *testing.T) {
	hint := loopHintFor("sandbox-run-python")
	if !strings.Contains(hint, "sandbox-run-python") {
		t.Errorf("hint missing tool name: %q", hint)
	}
	if !strings.Contains(strings.ToLower(hint), "three times") {
		t.Errorf("hint should explain why it's firing: %q", hint)
	}
}

// Simulates the agentLoop interaction with the buffer:
// 3 consecutive errors of same tool → loop detected; reset → no
// re-fire on the next push of the same kind unless 3 fresh
// entries accumulate.
func TestDetectStuckLoop_OneShotPerStretch(t *testing.T) {
	var buf []toolCallTrace
	for range 3 {
		buf = pushToolCallTrace(buf, toolCallTrace{Name: "t", Status: ActivityStatusError})
	}
	if _, ok := detectStuckLoop(buf); !ok {
		t.Fatal("expected detection on 3rd error")
	}
	// Reset (mirrors what agentLoop does).
	buf = nil
	// Add one more error — must NOT detect again on a single push.
	buf = pushToolCallTrace(buf, toolCallTrace{Name: "t", Status: ActivityStatusError})
	if _, ok := detectStuckLoop(buf); ok {
		t.Errorf("re-fired on a single post-reset error; should require fresh window")
	}
}
