package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

// Local is an OpenAI-compatible API backend (LM Studio, etc.).
type Local struct {
	cfg    config.LocalConfig
	client *http.Client
}

// NewLocal creates a new local LLM backend.
//
// No http.Client.Timeout is set: per-request timeout is owned by
// the retry/timeout wrapper (internal/llm/retry.go), which uses
// context.WithTimeout against the caller-supplied ctx and
// honours cfg.RequestTimeoutSeconds at the policy level. Setting
// both here and there would just race.
func NewLocal(cfg config.LocalConfig) *Local {
	return &Local{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// Name returns the backend identifier.
func (l *Local) Name() string { return "local" }

// --- request / response types ---

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []requestMessage `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []requestTool    `json:"tools,omitempty"`
}

type requestMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []contentPart for multimodal
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type requestTool struct {
	Type     string          `json:"type"`
	Function requestFunction `json:"function"`
}

type requestFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type chatResponse struct {
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Message      responseMessage `json:"message"`
	Delta        responseMessage `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

type responseMessage struct {
	Role      string             `json:"role"`
	Content   string             `json:"content"`
	ToolCalls []responseToolCall `json:"tool_calls,omitempty"`
}

type responseToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// --- implementation ---

// Chat sends messages and returns the complete response.
func (l *Local) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	body := l.buildRequest(messages, tools, false)
	data, err := l.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	var resp chatResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return &Response{}, nil
	}

	msg := resp.Choices[0].Message
	result := &Response{
		Content:      msg.Content,
		PromptTokens: resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}
	for _, tc := range msg.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return result, nil
}

// ChatStream sends messages and streams the response via callback.
func (l *Local) ChatStream(ctx context.Context, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error) {
	body := l.buildRequest(messages, tools, true)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := l.apiKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var fullContent strings.Builder
	var toolCalls []ToolCall
	toolCallArgs := map[int]*strings.Builder{}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if cb != nil {
				cb("", nil, true)
			}
			break
		}

		var chunk chatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta

		if delta.Content != "" {
			fullContent.WriteString(delta.Content)
			if cb != nil {
				cb(delta.Content, nil, false)
			}
		}

		for i, tc := range delta.ToolCalls {
			if _, ok := toolCallArgs[i]; !ok {
				toolCallArgs[i] = &strings.Builder{}
				toolCalls = append(toolCalls, ToolCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
				})
			}
			toolCallArgs[i].WriteString(tc.Function.Arguments)
		}
	}

	for i, tc := range toolCalls {
		if b, ok := toolCallArgs[i]; ok {
			tc.Arguments = b.String()
			toolCalls[i] = tc
		}
	}

	if len(toolCalls) > 0 && cb != nil {
		cb("", toolCalls, false)
	}

	return &Response{
		Content:   fullContent.String(),
		ToolCalls: toolCalls,
	}, nil
}

func (l *Local) buildRequest(messages []Message, tools []ToolDef, stream bool) []byte {
	req := chatRequest{
		Model:  l.cfg.Model,
		Stream: stream,
	}
	for _, m := range messages {
		// Map application-level roles to LM Studio/OpenAI API roles.
		// Design: docs/en/llm-abstraction.md Section 3.3
		role := string(m.Role)
		switch m.Role {
		case RoleTool:
			role = "user" // gemma-4 stays in tool-calling mode with role="tool"
		case RoleReport:
			role = "assistant"
		case RoleSummary:
			role = "system"
		}

		if len(m.ImageURLs) > 0 {
			// Multimodal: content as array of parts (OpenAI Vision format)
			parts := []contentPart{{Type: "text", Text: m.Content}}
			for _, imgURL := range m.ImageURLs {
				parts = append(parts, contentPart{
					Type:     "image_url",
					ImageURL: &imageURL{URL: imgURL},
				})
			}
			req.Messages = append(req.Messages, requestMessage{
				Role:    role,
				Content: parts,
			})
		} else {
			req.Messages = append(req.Messages, requestMessage{
				Role:    role,
				Content: m.Content,
			})
		}
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, requestTool{
			Type: "function",
			Function: requestFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	data, _ := json.Marshal(req)
	return data
}

func (l *Local) doRequest(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := l.apiKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		truncated := string(data)
		if len(truncated) > 512 {
			truncated = truncated[:512]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncated)
	}
	return data, nil
}

func (l *Local) apiKey() string {
	if l.cfg.APIKeyEnv != "" {
		return os.Getenv(l.cfg.APIKeyEnv)
	}
	return ""
}
