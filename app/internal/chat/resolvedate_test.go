package chat

import (
	"strings"
	"testing"
	"time"
)

func TestResolveDate(t *testing.T) {
	// Use a fixed reference date for deterministic tests
	ref := "2026-04-24" // a Thursday

	tests := []struct {
		expr string
		want string
	}{
		{"today", "2026-04-24 (Friday)"},
		{"yesterday", "2026-04-23 (Thursday)"},
		{"tomorrow", "2026-04-25 (Saturday)"},
		{"last thursday", "2026-04-23 (Thursday)"},
		{"last monday", "2026-04-20 (Monday)"},
		{"next monday", "2026-04-27 (Monday)"},
		{"next friday", "2026-05-01 (Friday)"},
		{"3 days ago", "2026-04-21 (Tuesday)"},
		{"7 days ago", "2026-04-17 (Friday)"},
		{"2 weeks ago", "2026-04-10 (Friday)"},
		{"1 month ago", "2026-03-24 (Tuesday)"},
		{"3 days from now", "2026-04-27 (Monday)"},
		{"1 week from now", "2026-05-01 (Friday)"},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			args := `{"expression":"` + tt.expr + `","reference_date":"` + ref + `"}`
			got, err := ResolveDate(args)
			if err != nil {
				t.Fatalf("ResolveDate(%q) error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("ResolveDate(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestResolveDateWithoutReference(t *testing.T) {
	args := `{"expression":"today"}`
	got, err := ResolveDate(args)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	today := time.Now().Format("2006-01-02")
	if !strings.HasPrefix(got, today) {
		t.Errorf("got %v, want prefix %v", got, today)
	}
}

func TestResolveDateInvalidExpression(t *testing.T) {
	args := `{"expression":"gibberish nonsense"}`
	_, err := ResolveDate(args)
	if err == nil {
		t.Error("expected error for invalid expression")
	}
}

func TestResolveDateInvalidJSON(t *testing.T) {
	_, err := ResolveDate("not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestResolveDateInvalidReference(t *testing.T) {
	args := `{"expression":"today","reference_date":"not-a-date"}`
	_, err := ResolveDate(args)
	if err == nil {
		t.Error("expected error for invalid reference date")
	}
}

func TestResolveDateToolDef(t *testing.T) {
	def := ResolveDateToolDef()
	if def["type"] != "object" {
		t.Error("tool def type should be object")
	}
	props, ok := def["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be map")
	}
	if _, ok := props["expression"]; !ok {
		t.Error("missing expression property")
	}
}
