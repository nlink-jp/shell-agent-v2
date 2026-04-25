//go:build lmstudio
// +build lmstudio

// These tests require a running LM Studio server at localhost:1234.
// Run with: go test ./internal/llm/ -tags lmstudio -v -timeout 120s
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

func newLMStudioClient() *Local {
	return NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "google/gemma-4-26b-a4b",
	})
}

// TestLMStudio_BasicChat verifies basic chat works.
func TestLMStudio_BasicChat(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Chat(ctx, []Message{
		{Role: "system", Content: "You are a helpful assistant. Reply briefly."},
		{Role: "user", Content: "Say hello in one word."},
	}, nil)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.Content == "" {
		t.Error("empty response")
	}
	t.Logf("Response: %s", resp.Content)
}

// TestLMStudio_ToolCall verifies the LLM can call a tool.
func TestLMStudio_ToolCall(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools := []ToolDef{{
		Name:        "get_time",
		Description: "Get the current time",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}}

	resp, err := client.Chat(ctx, []Message{
		{Role: "system", Content: "You are a helpful assistant. Use tools when appropriate."},
		{Role: "user", Content: "What time is it right now?"},
	}, tools)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}

	t.Logf("Content: %q, ToolCalls: %d", resp.Content, len(resp.ToolCalls))
	if len(resp.ToolCalls) == 0 {
		t.Error("expected tool call, got text response")
	} else {
		t.Logf("Tool call: %s(%s)", resp.ToolCalls[0].Name, resp.ToolCalls[0].Arguments)
	}
}

// TestLMStudio_ToolCallThenFinalResponse verifies the full tool call flow:
// 1. Call with tools → LLM calls tool
// 2. Add tool result to messages
// 3. Call WITHOUT tools → LLM generates final text
func TestLMStudio_ToolCallThenFinalResponse(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tools := []ToolDef{{
		Name:        "get_time",
		Description: "Get the current time",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}}

	// Round 1: Call with tools
	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant. Use tools when appropriate."},
		{Role: "user", Content: "What time is it?"},
	}

	resp1, err := client.Chat(ctx, messages, tools)
	if err != nil {
		t.Fatalf("Round 1 error: %v", err)
	}

	t.Logf("Round 1: content=%q toolCalls=%d", resp1.Content, len(resp1.ToolCalls))

	if len(resp1.ToolCalls) == 0 {
		t.Skip("LLM didn't call tool — model-dependent, skipping")
	}

	// Add assistant tool call message + tool result
	tc := resp1.ToolCalls[0]
	messages = append(messages, Message{
		Role:    "assistant",
		Content: fmt.Sprintf("[Calling: %s]", tc.Name),
	})
	messages = append(messages, Message{
		Role:    "tool",
		Content: `{"time": "14:30:00", "timezone": "JST"}`,
	})

	// Round 2: Call WITHOUT tools
	resp2, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Round 2 error: %v", err)
	}

	t.Logf("Round 2: content=%q toolCalls=%d", resp2.Content, len(resp2.ToolCalls))

	if resp2.Content == "" {
		t.Error("Round 2: expected text response, got empty")
	}
	if len(resp2.ToolCalls) > 0 {
		t.Errorf("Round 2: expected no tool calls, got %d", len(resp2.ToolCalls))
	}
}

// TestLMStudio_CreateReportFlow tests the exact create-report scenario.
func TestLMStudio_CreateReportFlow(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tools := []ToolDef{{
		Name:        "create-report",
		Description: "Create a structured markdown report.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":   map[string]any{"type": "string", "description": "Report title"},
				"content": map[string]any{"type": "string", "description": "Markdown content"},
			},
			"required": []string{"title", "content"},
		},
	}}

	// Round 1: Ask for report with tools
	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant. When asked to create a report, use the create-report tool."},
		{Role: "user", Content: "Please create a short report about the weather today."},
	}

	resp1, err := client.Chat(ctx, messages, tools)
	if err != nil {
		t.Fatalf("Round 1 error: %v", err)
	}

	t.Logf("Round 1: content=%q toolCalls=%d", truncateStr(resp1.Content, 100), len(resp1.ToolCalls))

	if len(resp1.ToolCalls) == 0 {
		t.Skip("LLM didn't call create-report — model-dependent, skipping")
	}

	tc := resp1.ToolCalls[0]
	if tc.Name != "create-report" {
		t.Fatalf("expected create-report, got %s", tc.Name)
	}

	// Parse the report content
	var reportArgs struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	json.Unmarshal([]byte(tc.Arguments), &reportArgs)
	t.Logf("Report title: %s, content length: %d", reportArgs.Title, len(reportArgs.Content))

	// Add assistant tool call message + tool result as user role
	// (gemma-4 stays in tool-calling mode with role="tool")
	messages = append(messages, Message{
		Role:    "assistant",
		Content: fmt.Sprintf("[Calling: %s]", tc.Name),
	})
	messages = append(messages, Message{
		Role:    "user",
		Content: fmt.Sprintf("SUCCESS: Report '%s' has been created and displayed to the user. Do not explain or describe the report contents. Reply only with a brief confirmation.", reportArgs.Title),
	})

	// Round 2: Call WITHOUT tools — should get brief confirmation, NOT report content
	resp2, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Round 2 error: %v", err)
	}

	t.Logf("Round 2: content=%q", truncateStr(resp2.Content, 200))

	if len(resp2.ToolCalls) > 0 {
		t.Errorf("Round 2: should have no tool calls, got %d", len(resp2.ToolCalls))
	}

	// Check if the response repeats the report content
	if resp2.Content != "" && len(resp2.Content) > len(reportArgs.Content)/2 {
		t.Logf("WARNING: Round 2 response (%d chars) may be repeating report content (%d chars)",
			len(resp2.Content), len(reportArgs.Content))
	}

	// Check for gemma tool call tags in response
	if strings.Contains(resp2.Content, "<|tool_call>") || strings.Contains(resp2.Content, "<tool_call>") {
		t.Errorf("Round 2: gemma tool call tags found in response: %s", truncateStr(resp2.Content, 200))
	}
}

// TestLMStudio_NoToolsNoGemmaTags verifies that calling without tools
// doesn't produce gemma-style text tool calls.
func TestLMStudio_NoToolsNoGemmaTags(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Simulate post-tool-execution context
	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Create a report about cats."},
		{Role: "assistant", Content: "[Calling: create-report]"},
		{Role: "tool", Content: "SUCCESS: Report 'Cats Report' has been created and displayed to the user."},
	}

	resp, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}

	t.Logf("Response: %q", truncateStr(resp.Content, 200))

	if strings.Contains(resp.Content, "<|tool_call>") || strings.Contains(resp.Content, "<tool_call>") {
		t.Errorf("gemma tool call tags in response without tools: %s", truncateStr(resp.Content, 200))
	}
}

// TestLMStudio_MultiToolChain tests chaining multiple tools.
func TestLMStudio_MultiToolChain(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tools := []ToolDef{
		{
			Name:        "resolve-date",
			Description: "Resolve relative date expressions to absolute dates.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"expression": map[string]any{"type": "string"},
				},
				"required": []string{"expression"},
			},
		},
		{
			Name:        "create-report",
			Description: "Create a markdown report.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":   map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"title", "content"},
			},
		},
	}

	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant. Use tools when needed."},
		{Role: "user", Content: "What was last Thursday's date? Then create a brief report about it."},
	}

	// Allow up to 3 rounds of tool calling
	for round := 0; round < 3; round++ {
		var toolsForRound []ToolDef
		if round == 0 {
			toolsForRound = tools
		}

		resp, err := client.Chat(ctx, messages, toolsForRound)
		if err != nil {
			t.Fatalf("Round %d error: %v", round, err)
		}

		t.Logf("Round %d: content=%q toolCalls=%d", round, truncateStr(resp.Content, 100), len(resp.ToolCalls))

		if len(resp.ToolCalls) == 0 {
			// Final response
			t.Logf("Final response at round %d: %s", round, truncateStr(resp.Content, 200))
			return
		}

		// Add tool call request
		tc := resp.ToolCalls[0]
		messages = append(messages, Message{
			Role:    "assistant",
			Content: fmt.Sprintf("[Calling: %s]", tc.Name),
		})

		// Simulate tool result
		var result string
		switch tc.Name {
		case "resolve-date":
			result = "2026-04-23 (Thursday)"
		case "create-report":
			result = fmt.Sprintf("SUCCESS: Report created and displayed to the user. Do not repeat the content.")
		default:
			result = "Unknown tool"
		}

		messages = append(messages, Message{
			Role:    "tool",
			Content: result,
		})
	}

	t.Log("Reached max rounds without final response")
}

// TestLMStudio_CreateReportResultWording tests different tool result wordings.
func TestLMStudio_CreateReportResultWording(t *testing.T) {
	client := newLMStudioClient()

	wordings := map[string]string{
		"do_not_repeat": "SUCCESS: Report '%s' has been created and displayed to the user. Do not repeat the report content. Simply confirm to the user that the report has been created.",
		"do_not_explain": "SUCCESS: Report '%s' has been created and displayed to the user. Do not explain or describe the report contents. Reply only with a brief confirmation.",
	}

	tools := []ToolDef{{
		Name:        "create-report",
		Description: "Create a structured markdown report.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":   map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"title", "content"},
		},
	}}

	for name, wording := range wordings {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			messages := []Message{
				{Role: "system", Content: "You are a helpful assistant. When asked to create a report, use the create-report tool."},
				{Role: "user", Content: "Create a short report about the weather."},
			}

			resp1, err := client.Chat(ctx, messages, tools)
			if err != nil {
				t.Fatalf("Round 1: %v", err)
			}
			if len(resp1.ToolCalls) == 0 {
				t.Skip("LLM didn't call tool")
			}

			tc := resp1.ToolCalls[0]
			var args struct{ Title string }
			json.Unmarshal([]byte(tc.Arguments), &args)

			messages = append(messages, Message{
				Role:    "assistant",
				Content: fmt.Sprintf("[Calling: %s]", tc.Name),
			})
			messages = append(messages, Message{
				Role:    "tool",
				Content: fmt.Sprintf(wording, args.Title),
			})

			resp2, err := client.Chat(ctx, messages, nil)
			if err != nil {
				t.Fatalf("Round 2: %v", err)
			}

			t.Logf("Wording: %s", name)
			t.Logf("Round 2 length: %d chars", len(resp2.Content))
			t.Logf("Round 2 content: %q", truncateStr(resp2.Content, 200))

			if len(resp2.Content) > 300 {
				t.Errorf("Response too long (%d chars) — likely repeating report content", len(resp2.Content))
			}
		})
	}
}

// TestLMStudio_CreateReportWithProperFormat tests using the exact format
// from LM Studio multi-turn example documentation.
func TestLMStudio_CreateReportWithProperFormat(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tools := []ToolDef{{
		Name:        "create-report",
		Description: "Create a structured markdown report.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":   map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"title", "content"},
		},
	}}

	// Round 1: with tools
	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant. Use the create-report tool when asked to create a report."},
		{Role: "user", Content: "Create a short report about cats."},
	}

	resp1, err := client.Chat(ctx, messages, tools)
	if err != nil {
		t.Fatalf("Round 1: %v", err)
	}
	if len(resp1.ToolCalls) == 0 {
		t.Skip("LLM didn't call tool")
	}

	tc := resp1.ToolCalls[0]
	t.Logf("Round 1: tool_call %s", tc.Name)

	// Per LM Studio docs multi-turn example:
	// Don't add synthetic text — use the tool role with tool_call_id
	messages = append(messages, Message{
		Role:    "tool",
		Content: "Report created and displayed.",
	})

	// Round 2: WITHOUT tools — just get final response
	resp2, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Round 2: %v", err)
	}

	t.Logf("Round 2 length: %d", len(resp2.Content))
	t.Logf("Round 2: %q", truncateStr(resp2.Content, 200))

	if len(resp2.Content) > 300 {
		t.Logf("WARNING: long response (%d chars)", len(resp2.Content))
	}
}

// TestLMStudio_CreateReportMinimalHistory tests with minimal conversation history.
// Skip the assistant tool_call message entirely — go straight from user to tool result.
func TestLMStudio_CreateReportMinimalHistory(t *testing.T) {
	client := newLMStudioClient()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Simulate: user asked → tool was called → result came back
	// No assistant message at all between user and tool
	messages := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Create a short report about dogs."},
		{Role: "assistant", Content: "I'll create that report for you."},
		{Role: "user", Content: "The report has been created and is now displayed. Please confirm this to the user briefly."},
	}

	resp, err := client.Chat(ctx, messages, nil)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}

	t.Logf("Response length: %d", len(resp.Content))
	t.Logf("Response: %q", truncateStr(resp.Content, 200))

	if len(resp.Content) > 300 {
		t.Logf("WARNING: long response (%d chars)", len(resp.Content))
	}
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
