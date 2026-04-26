//go:build lmstudio
// +build lmstudio

// Tool chaining tests: verify multi-turn tool calling behavior.
// Run with: go test ./internal/agent/ -tags "lmstudio no_duckdb_arrow" -v -timeout 300s -run "TestLMStudio_Chain"
package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// TestLMStudio_Chain_ToolsEveryRound tests v1 pattern: pass tools on every round.
// Verifies whether gemma-4 can chain tools (e.g. get-location → weather)
// without entering an infinite loop.
func TestLMStudio_Chain_ToolsEveryRound(t *testing.T) {
	client := llm.NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "google/gemma-4-26b-a4b",
	})

	tools := []llm.ToolDef{
		{
			Name:        "get-location",
			Description: "Get the current location of this device",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "weather",
			Description: "Get weather forecast for a region",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"region": map[string]any{"type": "string", "description": "Region name"},
				},
				"required": []string{"region"},
			},
		},
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a helpful assistant. Use tools when needed. Respond in the user's language."},
		{Role: llm.RoleUser, Content: "現在地の天気を教えて"},
	}

	t.Log("=== Tools every round (v1 pattern) ===")

	for round := 0; round < 5; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.Chat(ctx, messages, tools) // tools EVERY round
		cancel()
		if err != nil {
			t.Fatalf("Round %d: %v", round, err)
		}

		status := "TEXT"
		if len(resp.ToolCalls) > 0 {
			names := make([]string, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				names[i] = tc.Name
			}
			status = fmt.Sprintf("TOOL(%s)", strings.Join(names, ","))
		}

		t.Logf("Round %d: %s | content=%s", round, status, truncateChain(resp.Content, 80))

		if len(resp.ToolCalls) == 0 {
			t.Logf("Final response at round %d", round)
			break
		}

		// Add tool call + simulated results
		for _, tc := range resp.ToolCalls {
			messages = append(messages, llm.Message{
				Role:    llm.RoleAssistant,
				Content: resp.Content,
			})
			var result string
			switch tc.Name {
			case "get-location":
				result = `{"timezone":"JST","country":"Japan","locality":"Tokyo","lat":35.68,"lon":139.77}`
			case "weather":
				result = `{"region":"東京","forecast":"晴れ時々くもり","temperature":"22℃","humidity":"55%"}`
			default:
				result = fmt.Sprintf("Unknown tool: %s", tc.Name)
			}
			messages = append(messages, llm.Message{
				Role:     llm.RoleTool,
				Content:  result,
				ToolName: tc.Name,
			})
			t.Logf("  → %s result: %s", tc.Name, truncateChain(result, 80))
		}
	}
}

// TestLMStudio_Chain_ToolsNilAfterExec tests v2 pattern: tools=nil after execution.
// This should prevent chaining.
func TestLMStudio_Chain_ToolsNilAfterExec(t *testing.T) {
	client := llm.NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "google/gemma-4-26b-a4b",
	})

	tools := []llm.ToolDef{
		{
			Name:        "get-location",
			Description: "Get the current location of this device",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "weather",
			Description: "Get weather forecast for a region",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"region": map[string]any{"type": "string", "description": "Region name"},
				},
				"required": []string{"region"},
			},
		},
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a helpful assistant. Use tools when needed. Respond in the user's language."},
		{Role: llm.RoleUser, Content: "現在地の天気を教えて"},
	}

	t.Log("=== Tools nil after execution (v2 pattern) ===")

	toolsExecuted := false
	for round := 0; round < 5; round++ {
		var roundTools []llm.ToolDef
		if toolsExecuted {
			roundTools = nil
		} else {
			roundTools = tools
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.Chat(ctx, messages, roundTools)
		cancel()
		if err != nil {
			t.Fatalf("Round %d: %v", round, err)
		}

		status := "TEXT"
		toolLabel := "tools=YES"
		if roundTools == nil {
			toolLabel = "tools=NIL"
		}
		if len(resp.ToolCalls) > 0 {
			names := make([]string, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				names[i] = tc.Name
			}
			status = fmt.Sprintf("TOOL(%s)", strings.Join(names, ","))
		}

		t.Logf("Round %d [%s]: %s | content=%s", round, toolLabel, status, truncateChain(resp.Content, 80))

		if len(resp.ToolCalls) == 0 {
			t.Logf("Final response at round %d", round)
			break
		}

		for _, tc := range resp.ToolCalls {
			messages = append(messages, llm.Message{
				Role:    llm.RoleAssistant,
				Content: resp.Content,
			})
			var result string
			switch tc.Name {
			case "get-location":
				result = `{"timezone":"JST","country":"Japan","locality":"Tokyo","lat":35.68,"lon":139.77}`
			case "weather":
				result = `{"region":"東京","forecast":"晴れ時々くもり","temperature":"22℃","humidity":"55%"}`
			default:
				result = fmt.Sprintf("Unknown tool: %s", tc.Name)
			}
			messages = append(messages, llm.Message{
				Role:     llm.RoleTool,
				Content:  result,
				ToolName: tc.Name,
			})
			t.Logf("  → %s result: %s", tc.Name, truncateChain(result, 80))
		}
		toolsExecuted = true
	}
}

// TestLMStudio_Chain_LoopDetection tests whether gemma-4 enters a loop
// when tools are always available. Uses create-report which was the
// original problematic tool.
func TestLMStudio_Chain_LoopDetection(t *testing.T) {
	client := llm.NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "google/gemma-4-26b-a4b",
	})

	tools := []llm.ToolDef{
		{
			Name:        "create-report",
			Description: "Create a structured markdown report",
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

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a helpful assistant. When asked to create a report, use the create-report tool. After the report is created, confirm briefly."},
		{Role: llm.RoleUser, Content: "猫についてのレポートを作成して"},
	}

	t.Log("=== Loop detection: create-report with tools every round ===")

	callCount := map[string]int{}
	for round := 0; round < 6; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.Chat(ctx, messages, tools) // tools EVERY round
		cancel()
		if err != nil {
			t.Fatalf("Round %d: %v", round, err)
		}

		if len(resp.ToolCalls) == 0 {
			t.Logf("Round %d: TEXT | %s", round, truncateChain(resp.Content, 100))
			t.Logf("Ended at round %d without loop", round)
			break
		}

		tc := resp.ToolCalls[0]
		callCount[tc.Name]++
		t.Logf("Round %d: TOOL(%s) call #%d", round, tc.Name, callCount[tc.Name])

		if callCount[tc.Name] > 2 {
			t.Errorf("LOOP DETECTED: %s called %d times", tc.Name, callCount[tc.Name])
			break
		}

		messages = append(messages, llm.Message{
			Role:    llm.RoleAssistant,
			Content: resp.Content,
		})
		messages = append(messages, llm.Message{
			Role:     llm.RoleTool,
			Content:  "SUCCESS: Report created and displayed to the user. Do not explain or describe the report contents. Reply only with a brief confirmation.",
			ToolName: tc.Name,
		})
	}

	t.Logf("Tool call counts: %v", callCount)
}

// TestLMStudio_Chain_NoCallingContamination tests tools every round
// but WITHOUT [Calling:] messages in context. This is the proposed
// safe approach: v1 pattern + v2's [Calling:] exclusion.
func TestLMStudio_Chain_NoCallingContamination(t *testing.T) {
	client := llm.NewLocal(config.LocalConfig{
		Endpoint: "http://localhost:1234/v1",
		Model:    "google/gemma-4-26b-a4b",
	})

	tools := []llm.ToolDef{
		{
			Name:        "get-location",
			Description: "Get the current location of this device",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "weather",
			Description: "Get weather forecast for a region",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"region": map[string]any{"type": "string", "description": "Region name"},
				},
				"required": []string{"region"},
			},
		},
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a helpful assistant. Use tools when needed. Respond in the user's language."},
		{Role: llm.RoleUser, Content: "現在地の天気を教えて"},
	}

	t.Log("=== Tools every round, NO [Calling:] in context ===")

	for round := 0; round < 5; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.Chat(ctx, messages, tools)
		cancel()
		if err != nil {
			t.Fatalf("Round %d: %v", round, err)
		}

		status := "TEXT"
		if len(resp.ToolCalls) > 0 {
			names := make([]string, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				names[i] = tc.Name
			}
			status = fmt.Sprintf("TOOL(%s)", strings.Join(names, ","))
		}
		t.Logf("Round %d: %s | content=%s", round, status, truncateChain(resp.Content, 80))

		if len(resp.ToolCalls) == 0 {
			t.Logf("Final response at round %d", round)
			break
		}

		// Do NOT add [Calling:] assistant message — just add tool results directly
		for _, tc := range resp.ToolCalls {
			var result string
			switch tc.Name {
			case "get-location":
				result = `{"timezone":"JST","country":"Japan","locality":"Tokyo","lat":35.68,"lon":139.77}`
			case "weather":
				result = `{"region":"東京","forecast":"晴れ時々くもり","temperature":"22℃","humidity":"55%"}`
			default:
				result = fmt.Sprintf("Unknown tool: %s", tc.Name)
			}
			messages = append(messages, llm.Message{
				Role:     llm.RoleTool,
				Content:  result,
				ToolName: tc.Name,
			})
			t.Logf("  → %s result: %s", tc.Name, truncateChain(result, 80))
		}
	}
}

func truncateChain(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Dummy to satisfy shared helper build tag
var _ = memory.TierHot
