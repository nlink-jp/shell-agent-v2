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

func TestAppendImageIDLabel(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		ids         []string
		mustContain []string
		mustEqual   string // when empty, only mustContain is checked
	}{
		{"no ids returns content unchanged", "hello", nil, nil, "hello"},
		{"empty ids returns content unchanged", "hello", []string{}, nil, "hello"},
		{
			"single id appended",
			"What is this?",
			[]string{"abc123"},
			[]string{"What is this?", "abc123", "Attached image"},
			"",
		},
		{
			"multiple ids enumerated in order",
			"Compare these",
			[]string{"id-a", "id-b", "id-c"},
			[]string{"Compare these", "1. id-a", "2. id-b", "3. id-c", "Attached images"},
			"",
		},
		{
			"empty content + ids — no leading newlines",
			"",
			[]string{"id1"},
			[]string{"id1"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AppendImageIDLabel(tc.content, tc.ids)
			if tc.mustEqual != "" {
				if got != tc.mustEqual {
					t.Errorf("got %q, want %q", got, tc.mustEqual)
				}
				return
			}
			for _, sub := range tc.mustContain {
				if !strings.Contains(got, sub) {
					t.Errorf("output missing %q in %q", sub, got)
				}
			}
		})
	}
}

// Order matters: the label must list IDs in the exact order they
// appear in the input slice, because the matching ImageURLs are
// sent to the LLM in the same order.
func TestAppendImageIDLabel_PreservesOrder(t *testing.T) {
	got := AppendImageIDLabel("x", []string{"first", "second", "third"})
	idxFirst := strings.Index(got, "first")
	idxSecond := strings.Index(got, "second")
	idxThird := strings.Index(got, "third")
	if idxFirst < 0 || idxSecond < 0 || idxThird < 0 {
		t.Fatalf("missing IDs in output: %q", got)
	}
	if !(idxFirst < idxSecond && idxSecond < idxThird) {
		t.Errorf("IDs out of order in %q", got)
	}
}

// TestLocalBuildRequest_ImageAnchors verifies that messages with
// ImageURLs and matching ObjectIDs produce BEGIN/END text brackets
// around each image_url in the OpenAI-compatible request body, so
// a multimodal model sees explicit per-image boundaries (and not
// just a flat sequence of unlabeled images).
func TestLocalBuildRequest_ImageAnchors(t *testing.T) {
	l := NewLocal(config.LocalConfig{Model: "test-model"})
	messages := []Message{
		{
			Role:      "user",
			Content:   "What's in these images?",
			ImageURLs: []string{"data:image/png;base64,A", "data:image/png;base64,B"},
			ObjectIDs: []string{"id-aaaa", "id-bbbb"},
		},
	}

	data := l.buildRequest(messages, nil, false)
	body := string(data)

	// Each image must be sandwiched between a BEGIN and END marker
	// containing its ID. Use sequential search so we also verify
	// ordering: BEGIN A < image A < END A < BEGIN B < image B < END B.
	checkpoints := []string{
		"BEGIN IMAGE 1", "id-aaaa",
		"base64,A",
		"END IMAGE 1", "id-aaaa",
		"BEGIN IMAGE 2", "id-bbbb",
		"base64,B",
		"END IMAGE 2", "id-bbbb",
	}
	cursor := 0
	for _, needle := range checkpoints {
		idx := strings.Index(body[cursor:], needle)
		if idx < 0 {
			t.Fatalf("missing or out-of-order checkpoint %q in body (search starts at %d):\n%s", needle, cursor, body)
		}
		cursor += idx + len(needle)
	}
}

// TestLocalBuildRequest_ImagesNoAnchorsWhenObjectIDsMissing ensures
// the legacy path (ImageURLs without ObjectIDs) still works — old
// session records may not have ObjectIDs populated.
func TestLocalBuildRequest_ImagesNoAnchorsWhenObjectIDsMissing(t *testing.T) {
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

	if !strings.Contains(body, "base64,Z") {
		t.Errorf("image still must be sent: %s", body)
	}
	if strings.Contains(body, "object ID") {
		t.Errorf("no object ID anchor expected when ObjectIDs unset: %s", body)
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
