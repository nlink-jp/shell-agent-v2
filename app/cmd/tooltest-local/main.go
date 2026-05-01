// tooltest-local — empirical verification harness for the local
// OpenAI-compatible Chat Completions tool-calling round-trip
// (LM Studio + gemma-4 in our setup).
//
// Three modes:
//
//   proper — follows the OpenAI Cookbook + LM Studio docs:
//            assistant message carries `tool_calls`, tool result is
//            sent with role="tool" and a matching tool_call_id.
//
//   hack   — reproduces what shell-agent-v2 production currently does:
//            assistant turn replayed as plain text "[Calling: …]" with
//            no `tool_calls`, tool result sent with role="user".
//
//   nocall — sanity check: assistant has no tool_calls, tool result
//            sent with role="tool" anyway (orphan). Most servers should
//            reject this with HTTP 400; LM Studio's behaviour is what
//            we want to learn empirically.
//
// Tool: add_numbers(a, b int). Test query: "what is 7+5?"; expected
// final answer: text mentioning 12.
//
// Usage:
//   LMSTUDIO_ENDPOINT=http://localhost:1234/v1 go run ./cmd/tooltest-local proper
//   LMSTUDIO_ENDPOINT=http://localhost:1234/v1 go run ./cmd/tooltest-local hack
//
// Reads env LMSTUDIO_ENDPOINT (default http://localhost:1234/v1) and
// TOOLTEST_MODEL (default uses whatever the server has loaded).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	systemInstr = "You are a calculator. When the user asks for arithmetic, " +
		"call the add_numbers function. After the result is returned, " +
		"give the answer as plain text."
	userQuery = "what is 7+5?"
)

type chatReq struct {
	Model    string    `json:"model,omitempty"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

type message struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []messageToolCall `json:"tool_calls,omitempty"`
}

type messageToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResp struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tooltest-local {proper|hack|nocall}")
		os.Exit(2)
	}
	mode := os.Args[1]
	switch mode {
	case "proper", "hack", "nocall":
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}

	endpoint := os.Getenv("LMSTUDIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:1234/v1"
	}
	model := os.Getenv("TOOLTEST_MODEL")

	tools := []tool{{
		Type: "function",
		Function: toolFunction{
			Name:        "add_numbers",
			Description: "Add two integers and return the sum.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "integer"},
					"b": map[string]any{"type": "integer"},
				},
				"required": []string{"a", "b"},
			},
		},
	}}

	history := []message{
		{Role: "system", Content: systemInstr},
		{Role: "user", Content: userQuery},
	}

	fmt.Printf("=== tooltest-local mode=%s endpoint=%s model=%q ===\n", mode, endpoint, model)

	// Turn 1: expect tool_calls.
	fmt.Println("\n--- Turn 1 (expecting tool_calls) ---")
	resp1, err := chat(endpoint, model, history, tools)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: turn 1: %v\n", err)
		os.Exit(1)
	}
	if len(resp1.Choices) == 0 {
		fmt.Println("ERROR — no choices in turn 1")
		os.Exit(1)
	}
	dumpMsg("RESPONSE", resp1.Choices[0].Message)

	asst := resp1.Choices[0].Message
	if len(asst.ToolCalls) == 0 {
		fmt.Println("ERROR — turn 1 produced no tool_calls (model may not be tool-trained)")
		// Some gemma builds emit `[TOOL_REQUEST]…[END_TOOL_REQUEST]` text;
		// LM Studio is supposed to parse that into tool_calls, but if it
		// doesn't, the test cannot proceed.
		os.Exit(1)
	}
	tc := asst.ToolCalls[0]
	fmt.Printf("\nModel called: %s(%s) id=%s\n", tc.Function.Name, tc.Function.Arguments, tc.ID)

	// Append assistant turn — modes diverge here.
	var assistantTurn message
	switch mode {
	case "proper":
		// Echo the assistant message verbatim, including tool_calls.
		assistantTurn = message{
			Role:      "assistant",
			Content:   asst.Content, // typically null/empty
			ToolCalls: asst.ToolCalls,
		}
	case "hack":
		// Production-style placeholder text, no tool_calls.
		placeholder := fmt.Sprintf("[Calling: %s]", tc.Function.Name)
		assistantTurn = message{Role: "assistant", Content: placeholder}
	case "nocall":
		// Plain assistant with empty content, no tool_calls — sanity case.
		assistantTurn = message{Role: "assistant", Content: ""}
	}
	history = append(history, assistantTurn)

	// Append the tool result. Modes diverge again on role/id.
	switch mode {
	case "proper":
		history = append(history, message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    "12",
		})
	case "hack":
		// Production-style: role=user with the result inline (no tool_call_id).
		history = append(history, message{
			Role:    "user",
			Content: "12",
		})
	case "nocall":
		// Try the orphan path: role=tool with tool_call_id but no
		// preceding tool_calls.
		history = append(history, message{
			Role:       "tool",
			ToolCallID: "orphan-id",
			Name:       tc.Function.Name,
			Content:    "12",
		})
	}

	// Turn 2: expect a text answer mentioning 12.
	fmt.Println("\n--- Turn 2 (expecting text answer mentioning 12) ---")
	resp2, err := chat(endpoint, model, history, tools)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: turn 2: %v\n", err)
		os.Exit(1)
	}
	if len(resp2.Choices) == 0 {
		fmt.Println("ERROR — no choices in turn 2")
		os.Exit(1)
	}
	dumpMsg("RESPONSE", resp2.Choices[0].Message)

	final := resp2.Choices[0].Message
	if len(final.ToolCalls) > 0 {
		fmt.Printf("\nFAIL — model re-issued tool_calls: %s\n", final.ToolCalls[0].Function.Name)
		os.Exit(1)
	}
	text, _ := final.Content.(string)
	if text == "" {
		fmt.Println("\nFAIL — model produced no text answer")
		os.Exit(1)
	}
	fmt.Printf("\nPASS — model produced text answer: %q\n", text)
}

func chat(endpoint, model string, msgs []message, tools []tool) (*chatResp, error) {
	body, _ := json.Marshal(chatReq{
		Model:    model,
		Messages: msgs,
		Tools:    tools,
	})
	dumpReq(body)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", httpResp.StatusCode, string(raw))
	}
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var out chatResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse: %w (body=%s)", err, string(raw))
	}
	return &out, nil
}

func dumpReq(body []byte) {
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, body, "  ", "  ")
	fmt.Printf("[REQUEST]\n  %s\n", pretty.String())
}

func dumpMsg(label string, m message) {
	pretty, _ := json.MarshalIndent(m, "  ", "  ")
	fmt.Printf("[%s]\n  %s\n", label, pretty)
}
