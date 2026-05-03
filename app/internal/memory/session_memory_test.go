package memory

import (
	"strings"
	"testing"
	"time"
)

func TestSessionMemory_AddDedup(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{}, MaxEntries: 50}
	if !s.Add(SessionMemoryEntry{Fact: "x", Category: "context"}) {
		t.Error("first add should succeed")
	}
	if s.Add(SessionMemoryEntry{Fact: "x", Category: "fact"}) {
		t.Error("duplicate should dedup")
	}
}

func TestSessionMemory_FIFOEviction(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{}, MaxEntries: 3}
	for _, f := range []string{"a", "b", "c", "d"} {
		s.Add(SessionMemoryEntry{Fact: f, Category: "fact"})
	}
	if len(s.Entries) != 3 {
		t.Fatalf("got %d, want 3", len(s.Entries))
	}
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if s.Entries[i].Fact != w {
			t.Errorf("Entries[%d] = %q, want %q", i, s.Entries[i].Fact, w)
		}
	}
}

func TestSessionMemory_FormatForPrompt_TrustTag(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{
		{Fact: "u-fact", Category: "context", Source: SessionSourceUserTurn, CreatedAt: time.Now()},
		{Fact: "a-fact", Category: "fact", Source: SessionSourceAssistantTurn, CreatedAt: time.Now()},
		{Fact: "legacy-fact", Category: "fact", CreatedAt: time.Now()},
	}}
	out := s.FormatForPrompt()
	cases := []struct {
		fact string
		tag  string
	}{
		{"u-fact", "[user-stated]"},
		{"a-fact", "[derived]"},
		{"legacy-fact", "[derived]"},
	}
	for _, c := range cases {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, c.fact) {
				continue
			}
			if !strings.Contains(line, c.tag) {
				t.Errorf("fact %q: line %q missing tag %s", c.fact, line, c.tag)
			}
		}
	}
}

func TestSessionMemory_Get(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{
		{Fact: "a"}, {Fact: "b"},
	}}
	if e, ok := s.Get(0); !ok || e.Fact != "a" {
		t.Errorf("Get(0) = %+v, %v", e, ok)
	}
	if _, ok := s.Get(2); ok {
		t.Error("out-of-range Get should return false")
	}
	if _, ok := s.Get(-1); ok {
		t.Error("negative Get should return false")
	}
}

func TestSessionMemory_DeleteByFacts(t *testing.T) {
	s := &SessionMemoryStore{Entries: []SessionMemoryEntry{
		{Fact: "a"}, {Fact: "b"}, {Fact: "c"},
	}}
	got := s.DeleteByFacts([]string{"a", "c"})
	if got != 2 {
		t.Errorf("deleted = %d, want 2", got)
	}
	if len(s.Entries) != 1 || s.Entries[0].Fact != "b" {
		t.Errorf("remaining = %+v", s.Entries)
	}
}
