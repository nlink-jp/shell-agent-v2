package agent

import (
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestUserTurnCount verifies the helper that derives currentTurn
// from a session's record stream. Tool / assistant records do not
// contribute; the count is strictly user-role records.
func TestUserTurnCount(t *testing.T) {
	cases := []struct {
		name string
		recs []memory.Record
		want int
	}{
		{"empty", nil, 0},
		{"one user", []memory.Record{{Role: "user"}}, 1},
		{
			"user, assistant, user",
			[]memory.Record{{Role: "user"}, {Role: "assistant"}, {Role: "user"}},
			2,
		},
		{
			"tool records ignored",
			[]memory.Record{{Role: "user"}, {Role: "assistant"}, {Role: "tool"}, {Role: "assistant"}, {Role: "user"}},
			2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := userTurnCount(tc.recs); got != tc.want {
				t.Errorf("userTurnCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRunMemoryLifecycle_DecaysAndTouches exercises ADR-0031's
// agent-loop hook: every memory entry is decayed by one turn, and
// entries lexically referenced by the user message get touched
// back to relevance 1.0.
func TestRunMemoryLifecycle_DecaysAndTouches(t *testing.T) {
	a := New(config.Default())
	a.globalMemory = &memory.GlobalMemoryStore{
		MaxEntries: 100,
		Thresholds: memory.DefaultThresholds(),
		Entries: []memory.GlobalMemoryEntry{
			{
				Fact: "user prefers Go programming language", Category: "preference",
				Source: memory.GlobalSourceUserTurn,
				// Past fresh window, mid-active relevance so decay
				// can be observed without flipping state.
				Relevance: 0.7, CreatedTurn: 0, State: memory.StateActive,
			},
			{
				Fact: "unrelated trivia about Tokyo cuisine", Category: "preference",
				Source:    memory.GlobalSourceUserTurn,
				Relevance: 0.7, CreatedTurn: 0, State: memory.StateActive,
			},
		},
	}
	a.sessionMemory = &memory.SessionMemoryStore{
		MaxEntries: 50,
		Thresholds: memory.DefaultThresholds(),
		Entries: []memory.SessionMemoryEntry{
			{
				Fact: "current dataset is Q1 sales", Category: "context",
				Source:    memory.SessionSourceUserTurn,
				Relevance: 0.7, CreatedTurn: 0, State: memory.StateActive,
			},
		},
	}
	// Five user turns puts currentTurn well past FreshTurns (3),
	// so the fixture entries (CreatedTurn=0) qualify for decay.
	a.session = &memory.Session{
		ID: "lifecycle-test", Title: "T",
		Records: []memory.Record{
			{Role: "user", Content: "t1"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "t2"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "t3"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "t4"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "Tell me more about Go programming language patterns"},
		},
	}

	// Snapshot pre-state for comparison.
	preG := a.globalMemory.All()
	preS := a.sessionMemory.All()
	if preG[0].Relevance != 0.7 {
		t.Fatalf("setup: global[0].Relevance = %v, want 0.7", preG[0].Relevance)
	}
	if preS[0].Relevance != 0.7 {
		t.Fatalf("setup: session[0].Relevance = %v, want 0.7", preS[0].Relevance)
	}

	a.runMemoryLifecycle("Tell me more about Go programming language patterns")

	postG := a.globalMemory.All()
	postS := a.sessionMemory.All()

	// global[0] "Go programming language" overlaps user message →
	// touched back to 1.0.
	if postG[0].Relevance != 1.0 {
		t.Errorf("global[0] (lexical match) Relevance = %v, want 1.0 after touch", postG[0].Relevance)
	}
	if postG[0].TouchCount != 1 {
		t.Errorf("global[0] TouchCount = %d, want 1", postG[0].TouchCount)
	}

	// global[1] unrelated → decayed only (no touch).
	if postG[1].Relevance >= preG[1].Relevance {
		t.Errorf("global[1] (unrelated) should have decayed; pre=%v post=%v",
			preG[1].Relevance, postG[1].Relevance)
	}
	if postG[1].TouchCount != 0 {
		t.Errorf("global[1] TouchCount = %d, want 0 (no touch)", postG[1].TouchCount)
	}

	// session[0] unrelated to user message → decayed only.
	if postS[0].Relevance >= preS[0].Relevance {
		t.Errorf("session[0] should have decayed; pre=%v post=%v",
			preS[0].Relevance, postS[0].Relevance)
	}
}

// TestRunMemoryLifecycle_NilSession is the no-op guard — runtime
// boundary check that the hook is safe to call before any session
// is loaded.
func TestRunMemoryLifecycle_NilSession(t *testing.T) {
	a := New(config.Default())
	a.session = nil
	// Must not panic, must not touch nil stores.
	a.runMemoryLifecycle("anything")
}

// TestCompactionSummarizerPrompt_NoPreserveKeyFacts verifies the
// ADR-0032 §2.5 prompt replaces the load-bearing "Preserve key
// facts" instruction that drove early-anchor reinforcement and
// asks for topic bullets instead.
func TestCompactionSummarizerPrompt_NoPreserveKeyFacts(t *testing.T) {
	if strings.Contains(compactionSummarizerPrompt, "Preserve key facts") {
		t.Error("ADR-0032 prompt must NOT contain the old 'Preserve key facts' instruction")
	}
	if !strings.Contains(compactionSummarizerPrompt, "topic bullets") {
		t.Error("ADR-0032 prompt must ask for topic bullets")
	}
	if !strings.Contains(compactionSummarizerPrompt, "preserved verbatim in a separate block") {
		t.Error("ADR-0032 prompt must tell the LLM anchor preservation lives elsewhere")
	}
}
