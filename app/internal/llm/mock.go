package llm

import (
	"context"
	"encoding/json"
)

// MockBackend is a programmable LLM backend for testing.
type MockBackend struct {
	responses []MockResponse
	calls     []MockCall
	callIdx   int
}

// MockResponse defines what the mock returns for a given call.
type MockResponse struct {
	Content   string
	ToolCalls []ToolCall
	Err       error
}

// MockCall records what was sent to the mock.
type MockCall struct {
	Messages []Message
	Tools    []ToolDef
}

// NewMockBackend creates a mock backend with predefined responses.
// Responses are returned in order; cycles back to the last one if exhausted.
func NewMockBackend(responses ...MockResponse) *MockBackend {
	return &MockBackend{responses: responses}
}

// NewMockWithToolCall creates a mock that returns a tool call on the first
// call and a text response on the second.
func NewMockWithToolCall(toolName, toolArgs, finalResponse string) *MockBackend {
	return &MockBackend{
		responses: []MockResponse{
			{
				ToolCalls: []ToolCall{{
					ID:        "tc-mock-1",
					Name:      toolName,
					Arguments: toolArgs,
				}},
			},
			{Content: finalResponse},
		},
	}
}

func (m *MockBackend) Name() string { return "mock" }

func (m *MockBackend) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	return m.nextResponse(messages, tools)
}

func (m *MockBackend) ChatStream(ctx context.Context, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error) {
	resp, err := m.nextResponse(messages, tools)
	if err != nil {
		return nil, err
	}
	if cb != nil {
		if resp.Content != "" {
			cb(resp.Content, nil, false)
		}
		if len(resp.ToolCalls) > 0 {
			cb("", resp.ToolCalls, false)
		}
		cb("", nil, true)
	}
	return resp, nil
}

// Calls returns all recorded calls.
func (m *MockBackend) Calls() []MockCall {
	return m.calls
}

// LastCall returns the most recent call, or an empty MockCall if none.
func (m *MockBackend) LastCall() MockCall {
	if len(m.calls) == 0 {
		return MockCall{}
	}
	return m.calls[len(m.calls)-1]
}

// ToolNames returns the tool names from the last call.
func (m *MockBackend) ToolNames() []string {
	last := m.LastCall()
	names := make([]string, len(last.Tools))
	for i, t := range last.Tools {
		names[i] = t.Name
	}
	return names
}

func (m *MockBackend) nextResponse(messages []Message, tools []ToolDef) (*Response, error) {
	m.calls = append(m.calls, MockCall{Messages: messages, Tools: tools})

	idx := m.callIdx
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}
	if idx < 0 {
		return &Response{Content: "(no mock responses configured)"}, nil
	}
	m.callIdx++

	r := m.responses[idx]
	if r.Err != nil {
		return nil, r.Err
	}
	return &Response{
		Content:   r.Content,
		ToolCalls: r.ToolCalls,
	}, nil
}

// ParseToolCallArgs is a test helper to unmarshal tool call arguments.
func ParseToolCallArgs(argsJSON string) map[string]any {
	var args map[string]any
	json.Unmarshal([]byte(argsJSON), &args)
	return args
}
