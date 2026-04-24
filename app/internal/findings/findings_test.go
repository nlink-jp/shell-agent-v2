package findings

import (
	"strings"
	"testing"
)

func TestAddFinding(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}

	f := s.Add("Sales peak in April", "sess-001", "Sales Analysis", []string{"sales"})

	if f.Content != "Sales peak in April" {
		t.Errorf("content = %v, want 'Sales peak in April'", f.Content)
	}
	if f.OriginSessionID != "sess-001" {
		t.Errorf("origin session = %v, want sess-001", f.OriginSessionID)
	}
	if f.CreatedLabel == "" {
		t.Error("created label is empty")
	}
	if !strings.Contains(f.CreatedLabel, "(") {
		t.Errorf("created label missing day of week: %s", f.CreatedLabel)
	}
	if len(s.All()) != 1 {
		t.Errorf("findings count = %d, want 1", len(s.All()))
	}
}

func TestFormatForPrompt(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}
	s.Add("Sales peak in April", "sess-001", "Sales Analysis", []string{"sales"})

	prompt := s.FormatForPrompt()
	if !strings.Contains(prompt, "Sales peak in April") {
		t.Error("prompt missing finding content")
	}
	if !strings.Contains(prompt, "Sales Analysis") {
		t.Error("prompt missing session title")
	}
}

func TestFormatForPromptEmpty(t *testing.T) {
	s := &Store{path: "/tmp/test-findings.json", findings: []Finding{}}
	if s.FormatForPrompt() != "" {
		t.Error("empty store should return empty string")
	}
}
