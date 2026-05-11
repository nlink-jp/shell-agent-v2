package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

func TestLocalImplementsBackend(t *testing.T) {
	var _ Backend = NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "test",
	})
}

func TestVertexImplementsBackend(t *testing.T) {
	var _ Backend = NewVertex(config.VertexAIConfig{
		ProjectID: "test-project",
		Region:    "us-central1",
		Model:     "gemini-2.5-flash",
	})
}

func TestLocalName(t *testing.T) {
	l := NewLocal(config.LocalConfig{})
	if l.Name() != "local" {
		t.Errorf("Name() = %v, want local", l.Name())
	}
}

func TestVertexName(t *testing.T) {
	v := NewVertex(config.VertexAIConfig{})
	if v.Name() != "vertex_ai" {
		t.Errorf("Name() = %v, want vertex_ai", v.Name())
	}
}

// TestLocalBuildRequest_SingleImage verifies the multi-image
// split also covers N=1: one image-bearing user turn + one
// trailing user turn carrying the original text.
func TestLocalBuildRequest_SingleImage(t *testing.T) {
	l := NewLocal(config.LocalConfig{Model: "test-model"})
	messages := []Message{
		{
			Role:      "user",
			Content:   "What is this?",
			ImageURLs: []string{"data:image/png;base64,A"},
			ObjectIDs: []string{"id-aaaa"},
		},
	}

	data := l.buildRequest(messages, nil, false)
	var req chatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (image turn + question turn)", len(req.Messages))
	}

	parts, ok := req.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("first message Content = %T, want []any (multipart)", req.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Errorf("first turn parts = %d, want 2 (prefix + image_url)", len(parts))
	}

	body := string(data)
	if !strings.Contains(body, "Image (object ID: id-aaaa):") {
		t.Errorf("missing ID prefix in body: %s", body)
	}
	if !strings.Contains(body, "base64,A") {
		t.Errorf("missing image_url in body: %s", body)
	}
	if got := req.Messages[1].Content.(string); got != "What is this?" {
		t.Errorf("question turn content = %q, want %q", got, "What is this?")
	}
}

// TestLocalBuildRequest_MultipleImages is the core fix for the
// Gemma multi-image swap bug: each image is in its own user turn,
// so llama.cpp's mmproj slot-reuse can't reorder them across one
// prompt.
func TestLocalBuildRequest_MultipleImages(t *testing.T) {
	l := NewLocal(config.LocalConfig{Model: "test-model"})
	messages := []Message{
		{
			Role:      "user",
			Content:   "Describe each in order.",
			ImageURLs: []string{"data:image/png;base64,A", "data:image/png;base64,B", "data:image/png;base64,C"},
			ObjectIDs: []string{"id-aaaa", "id-bbbb", "id-cccc"},
		},
	}

	data := l.buildRequest(messages, nil, false)
	var req chatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (3 image turns + 1 question turn)", len(req.Messages))
	}

	body := string(data)
	// IDs must appear in attach order, each adjacent to its image.
	cursor := 0
	for _, expect := range []string{
		"id-aaaa", "base64,A",
		"id-bbbb", "base64,B",
		"id-cccc", "base64,C",
	} {
		idx := strings.Index(body[cursor:], expect)
		if idx < 0 {
			t.Fatalf("missing or out-of-order %q in body:\n%s", expect, body)
		}
		cursor += idx + len(expect)
	}

	if got := req.Messages[3].Content.(string); got != "Describe each in order." {
		t.Errorf("last turn content = %q", got)
	}
}

// TestLocalBuildRequest_LegacyImageWithoutObjectID ensures old
// session records (ImageURLs but no ObjectIDs) still build into a
// valid request — they fall back to a positional "Image N:" prefix
// and the per-image-turn split still applies.
func TestLocalBuildRequest_LegacyImageWithoutObjectID(t *testing.T) {
	l := NewLocal(config.LocalConfig{Model: "test-model"})
	messages := []Message{
		{
			Role:      "user",
			Content:   "Old session",
			ImageURLs: []string{"data:image/png;base64,Z"},
		},
	}

	data := l.buildRequest(messages, nil, false)
	body := string(data)
	if !strings.Contains(body, "Image 1:") {
		t.Errorf("expected positional Image 1: prefix, body=%s", body)
	}
	if !strings.Contains(body, "base64,Z") {
		t.Errorf("image still must be sent: %s", body)
	}
}

// TestImageIDPrefix exercises the helper directly.
func TestImageIDPrefix(t *testing.T) {
	cases := []struct {
		i     int
		ids   []string
		want  string
	}{
		{0, []string{"abc"}, "Image (object ID: abc):"},
		{1, []string{"a", "b"}, "Image (object ID: b):"},
		{2, []string{"a"}, "Image 3:"}, // out-of-range falls back
		{0, nil, "Image 1:"},
	}
	for _, tc := range cases {
		got := imageIDPrefix(tc.i, tc.ids)
		if got != tc.want {
			t.Errorf("imageIDPrefix(%d, %v) = %q, want %q", tc.i, tc.ids, got, tc.want)
		}
	}
}

// TestDocumentIDPrefix exercises the v0.5 markdown attachment
// anchor helper. The token-suffix tiers (none / N / Nk / N.NM)
// keep the anchor compact for small docs and informative for
// large ones without leaking precise counts that would balloon
// the prompt across sessions.
func TestDocumentIDPrefix(t *testing.T) {
	cases := []struct {
		id     string
		name   string
		tokens int
		want   string
	}{
		// No size hint when tokens=0.
		{"abc", "x.md", 0, "Document (object ID: abc, name: x.md):"},
		// Small files: N tokens.
		{"abc", "x.md", 42, "Document (object ID: abc, name: x.md, 42 tokens):"},
		// Medium: kilotokens.
		{"abc", "audit.md", 5432, "Document (object ID: abc, name: audit.md, 5k tokens):"},
		// Large: megatokens with one decimal.
		{"abc", "huge.md", 1_500_000, "Document (object ID: abc, name: huge.md, 1.5M tokens):"},
		// Missing name → omit ", name: ...".
		{"abc", "", 100, "Document (object ID: abc, 100 tokens):"},
	}
	for _, tc := range cases {
		got := DocumentIDPrefix(tc.id, tc.name, tc.tokens)
		if got != tc.want {
			t.Errorf("DocumentIDPrefix(%q,%q,%d) = %q, want %q", tc.id, tc.name, tc.tokens, got, tc.want)
		}
	}
}

func TestLocalBuildRequest(t *testing.T) {
	l := NewLocal(config.LocalConfig{Model: "test-model"})
	messages := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}
	tools := []ToolDef{
		{Name: "resolve-date", Description: "resolve dates", Parameters: map[string]any{}},
	}

	data := l.buildRequest(messages, tools, false)
	if len(data) == 0 {
		t.Error("buildRequest returned empty data")
	}

	// Verify it's valid JSON
	var req chatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("buildRequest produced invalid JSON: %v", err)
	}
	if req.Model != "test-model" {
		t.Errorf("model = %v, want test-model", req.Model)
	}
	if len(req.Messages) != 2 {
		t.Errorf("messages count = %d, want 2", len(req.Messages))
	}
	if len(req.Tools) != 1 {
		t.Errorf("tools count = %d, want 1", len(req.Tools))
	}
	if req.Stream {
		t.Error("stream should be false")
	}
}
