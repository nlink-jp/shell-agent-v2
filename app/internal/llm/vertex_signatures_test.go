package llm

import (
	"bytes"
	"testing"

	"google.golang.org/genai"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Unit tests for Gemini 3+ thought-signature capture in
// parseResponse and replay in buildContents. Pure in-process tests
// — no Vertex AI credentials needed, run on every `go test`.
// See ADR-0009.

func newVertexForUnit() *Vertex {
	return NewVertex(config.VertexAIConfig{
		ProjectID: "test-project",
		Region:    "us-central1",
		Model:     "gemini-3-pro",
	})
}

func TestParseResponse_CapturesFunctionCallSignature(t *testing.T) {
	v := newVertexForUnit()
	sig := []byte{0xde, 0xad, 0xbe, 0xef}
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "weather",
							Args: map[string]any{"city": "tokyo"},
						},
						ThoughtSignature: sig,
					},
				},
			},
		}},
	}
	got := v.parseResponse(resp)
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(got.ToolCalls))
	}
	if !bytes.Equal(got.ToolCalls[0].ThoughtSignature, sig) {
		t.Errorf("ToolCall.ThoughtSignature = %x, want %x", got.ToolCalls[0].ThoughtSignature, sig)
	}
}

func TestParseResponse_CapturesTextSignature(t *testing.T) {
	v := newVertexForUnit()
	sig := []byte{0xc0, 0xc1, 0xc2}
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{
						Text:             "I'll check the weather.",
						ThoughtSignature: sig,
					},
				},
			},
		}},
	}
	got := v.parseResponse(resp)
	if got.Content != "I'll check the weather." {
		t.Errorf("Content = %q", got.Content)
	}
	if !bytes.Equal(got.TextPartSig, sig) {
		t.Errorf("TextPartSig = %x, want %x", got.TextPartSig, sig)
	}
}

func TestParseResponse_CapturesThoughtSignatures(t *testing.T) {
	v := newVertexForUnit()
	sigA := []byte{0xa0, 0xa1}
	sigB := []byte{0xb0, 0xb1, 0xb2}
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{Thought: true, ThoughtSignature: sigA},
					{Thought: true, ThoughtSignature: sigB},
					{Text: "after thinking"},
				},
			},
		}},
	}
	got := v.parseResponse(resp)
	if len(got.ThoughtPartSigs) != 2 {
		t.Fatalf("ThoughtPartSigs = %d, want 2", len(got.ThoughtPartSigs))
	}
	if !bytes.Equal(got.ThoughtPartSigs[0], sigA) {
		t.Errorf("ThoughtPartSigs[0] = %x, want %x", got.ThoughtPartSigs[0], sigA)
	}
	if !bytes.Equal(got.ThoughtPartSigs[1], sigB) {
		t.Errorf("ThoughtPartSigs[1] = %x, want %x", got.ThoughtPartSigs[1], sigB)
	}
	// Order-preserving capture from the parts walk.
	if got.Content != "after thinking" {
		t.Errorf("Content = %q, want 'after thinking'", got.Content)
	}
}

func TestParseResponse_NoSignaturesIsNoop(t *testing.T) {
	v := newVertexForUnit()
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{Text: "plain response"},
					{
						FunctionCall: &genai.FunctionCall{
							Name: "tool", Args: map[string]any{},
						},
					},
				},
			},
		}},
	}
	got := v.parseResponse(resp)
	if len(got.ThoughtPartSigs) != 0 {
		t.Errorf("ThoughtPartSigs unexpectedly non-empty: %v", got.ThoughtPartSigs)
	}
	if len(got.TextPartSig) != 0 {
		t.Errorf("TextPartSig unexpectedly non-empty: %v", got.TextPartSig)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(got.ToolCalls))
	}
	if len(got.ToolCalls[0].ThoughtSignature) != 0 {
		t.Errorf("ToolCall.ThoughtSignature unexpectedly non-empty")
	}
}

func TestBuildContents_ReplaysFunctionCallSignature(t *testing.T) {
	v := newVertexForUnit()
	sig := []byte{0xde, 0xad, 0xbe, 0xef}
	msgs := []Message{
		{Role: RoleUser, Content: "weather in tokyo?"},
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{
				{
					ID:               "tc-1",
					Name:             "weather",
					Arguments:        `{"city":"tokyo"}`,
					ThoughtSignature: sig,
				},
			},
		},
	}
	contents := v.buildContents(msgs)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2 (user + model)", len(contents))
	}
	modelContent := contents[1]
	if modelContent.Role != genai.RoleModel {
		t.Errorf("model content role = %q, want %q", modelContent.Role, genai.RoleModel)
	}
	if len(modelContent.Parts) != 1 {
		t.Fatalf("model parts = %d, want 1", len(modelContent.Parts))
	}
	got := modelContent.Parts[0]
	if got.FunctionCall == nil || got.FunctionCall.Name != "weather" {
		t.Errorf("part not a weather function call: %#v", got)
	}
	if !bytes.Equal(got.ThoughtSignature, sig) {
		t.Errorf("ThoughtSignature = %x, want %x", got.ThoughtSignature, sig)
	}
}

func TestBuildContents_ReplaysThoughtSignatures(t *testing.T) {
	v := newVertexForUnit()
	sigA := []byte{0xa0, 0xa1}
	sigB := []byte{0xb0, 0xb1}
	msgs := []Message{
		{Role: RoleUser, Content: "what's up?"},
		{
			Role:            RoleAssistant,
			Content:         "thinking…",
			ThoughtPartSigs: [][]byte{sigA, sigB},
			TextPartSig:     []byte{0xc0, 0xc1},
			ToolCalls: []ToolCall{
				{ID: "tc-1", Name: "noop", Arguments: `{}`},
			},
		},
	}
	contents := v.buildContents(msgs)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(contents))
	}
	parts := contents[1].Parts
	// Expect: 2 thought parts, 1 text part, 1 function call part = 4.
	if len(parts) != 4 {
		t.Fatalf("parts = %d, want 4 (2 thought + 1 text + 1 fc)", len(parts))
	}
	// Thought parts in order.
	if !parts[0].Thought || !bytes.Equal(parts[0].ThoughtSignature, sigA) {
		t.Errorf("parts[0] = %#v, want Thought=true sig=sigA", parts[0])
	}
	if !parts[1].Thought || !bytes.Equal(parts[1].ThoughtSignature, sigB) {
		t.Errorf("parts[1] = %#v, want Thought=true sig=sigB", parts[1])
	}
	// Text part with signature.
	if parts[2].Text != "thinking…" {
		t.Errorf("parts[2].Text = %q, want 'thinking…'", parts[2].Text)
	}
	if !bytes.Equal(parts[2].ThoughtSignature, []byte{0xc0, 0xc1}) {
		t.Errorf("parts[2].ThoughtSignature mismatch")
	}
	// Function call part (no signature in this test).
	if parts[3].FunctionCall == nil || parts[3].FunctionCall.Name != "noop" {
		t.Errorf("parts[3] not the noop call: %#v", parts[3])
	}
}

func TestBuildContents_EmptySignaturesNoop(t *testing.T) {
	v := newVertexForUnit()
	msgs := []Message{
		{Role: RoleUser, Content: "hi"},
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{
				{ID: "tc-1", Name: "tool", Arguments: `{}`},
			},
		},
	}
	contents := v.buildContents(msgs)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(contents))
	}
	parts := contents[1].Parts
	// No thought sigs, no text content → just the function call part.
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1 (just the function call)", len(parts))
	}
	if parts[0].FunctionCall == nil {
		t.Errorf("parts[0] is not a function call: %#v", parts[0])
	}
	if len(parts[0].ThoughtSignature) != 0 {
		t.Errorf("ThoughtSignature unexpectedly non-empty: %x", parts[0].ThoughtSignature)
	}
	// And no leading Thought:true parts.
	if parts[0].Thought {
		t.Errorf("parts[0] unexpectedly marked Thought=true")
	}
}
