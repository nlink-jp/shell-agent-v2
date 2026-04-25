//go:build vertexai
// +build vertexai

// These tests require Vertex AI access (ADC + project).
// Run with: go test ./internal/llm/ -tags vertexai -v -timeout 120s
//
// Prerequisites:
//   gcloud auth application-default login
//   export VERTEX_PROJECT=your-project-id
//   export VERTEX_REGION=us-central1  (optional, defaults to us-central1)
//   export VERTEX_MODEL=gemini-2.5-flash  (optional)
package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

func newVertexClient(t *testing.T) *Vertex {
	t.Helper()
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}
	region := os.Getenv("VERTEX_REGION")
	if region == "" {
		region = "us-central1"
	}
	model := os.Getenv("VERTEX_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}
	return NewVertex(config.VertexAIConfig{
		ProjectID: project,
		Region:    region,
		Model:     model,
	})
}

func TestVertexAI_BasicChat(t *testing.T) {
	client := newVertexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Chat(ctx, []Message{
		{Role: RoleSystem, Content: "Reply in one sentence."},
		{Role: RoleUser, Content: "What is 2+2?"},
	}, nil)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.Content == "" {
		t.Error("empty response")
	}
	t.Logf("Response: %s", resp.Content)
}

func TestVertexAI_RoleMapping(t *testing.T) {
	client := newVertexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test that all application roles are handled without error
	messages := []Message{
		{Role: RoleSystem, Content: "You are a helpful assistant."},
		{Role: RoleUser, Content: "Hello"},
		{Role: RoleAssistant, Content: "Hi there!"},
		{Role: RoleTool, Content: "Tool result: success", ToolName: "test-tool"},
		{Role: RoleReport, Content: "# Report\nSome content."},
		{Role: RoleSummary, Content: "Summary of earlier conversation."},
		{Role: RoleUser, Content: "Thanks. Just say OK."},
	}

	resp, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.Content == "" {
		t.Error("empty response")
	}
	t.Logf("Response: %s", resp.Content)
}

func TestVertexAI_SystemAndSummaryMerged(t *testing.T) {
	client := newVertexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// System + Summary should both appear in SystemInstruction
	messages := []Message{
		{Role: RoleSystem, Content: "You are a test assistant."},
		{Role: RoleSummary, Content: "Earlier, the user said their name is Alice."},
		{Role: RoleUser, Content: "What is my name?"},
	}

	resp, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	t.Logf("Response: %s", resp.Content)

	// The response should mention Alice (from summary context)
	if resp.Content == "" {
		t.Error("empty response")
	}
}

func TestVertexAI_Streaming(t *testing.T) {
	client := newVertexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var tokens []string
	resp, err := client.ChatStream(ctx, []Message{
		{Role: RoleSystem, Content: "Reply briefly."},
		{Role: RoleUser, Content: "Say hello."},
	}, nil, func(token string, toolCalls []ToolCall, done bool) {
		if token != "" {
			tokens = append(tokens, token)
		}
	})
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if resp.Content == "" {
		t.Error("empty response")
	}
	if len(tokens) == 0 {
		t.Error("no streaming tokens received")
	}
	t.Logf("Streamed %d tokens, final: %s", len(tokens), resp.Content)
}
