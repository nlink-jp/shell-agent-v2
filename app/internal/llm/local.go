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
	Role       string            `json:"role"`
	Content    any               `json:"content"` // string or []contentPart for multimodal
	ToolName   string            `json:"name,omitempty"`         // for role="tool"
	ToolCallID string            `json:"tool_call_id,omitempty"` // for role="tool"
	ToolCalls  []requestToolCall `json:"tool_calls,omitempty"`   // for role="assistant"
}

type requestToolCall struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"` // always "function"
	Function requestToolCallFunction `json:"function"`
}

type requestToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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
		if err := validateToolCallArgs(tc.Function.Name, tc.Function.Arguments, l.maxToolArgsBytes()); err != nil {
			return nil, err
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return result, nil
}

// MaxToolCallArgsBytesDefault is the default per-call cap on
// LLM-emitted tool arguments. 1 MiB is a garbage-detection threshold,
// not a tight resource limit — sandbox-write-file with a chunky CSV /
// HTML payload routinely reaches a few hundred KB. The 16 MiB
// response-body cap (MaxLocalResponseBytes / H12) is the actual
// memory defence (security-hardening-2.md H6).
const MaxToolCallArgsBytesDefault = 1024 * 1024

func (l *Local) maxToolArgsBytes() int {
	if l.cfg.MaxToolCallArgsBytes > 0 {
		return l.cfg.MaxToolCallArgsBytes
	}
	return MaxToolCallArgsBytesDefault
}

func validateToolCallArgs(name, args string, maxBytes int) error {
	if len(args) > maxBytes {
		return fmt.Errorf("tool call %q arguments exceed %d bytes", name, maxBytes)
	}
	if args == "" {
		// Empty args are accepted by some upstream model variants for
		// no-parameter tools; the agent dispatcher already json.Unmarshals
		// into a per-tool struct, which tolerates an empty string when
		// no fields are required.
		return nil
	}
	if !json.Valid([]byte(args)) {
		return fmt.Errorf("tool call %q arguments are not valid JSON", name)
	}
	return nil
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

	// Cap streaming bytes too — tag the body with a LimitReader so a
	// runaway endpoint can't stream forever. +1 lets us detect that
	// we hit the cap rather than a clean EOF
	// (security-hardening-2.md H12).
	limitedBody := io.LimitReader(resp.Body, MaxLocalResponseBytes+1)
	scanner := bufio.NewScanner(limitedBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1 MiB max line; LM Studio JSON-line chunks are small
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
		if err := validateToolCallArgs(tc.Name, tc.Arguments, l.maxToolArgsBytes()); err != nil {
			return nil, err
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
		// Design: docs/en/history/llm-abstraction.md, docs/en/history/tool-call-roundtrip.md
		role := string(m.Role)
		switch m.Role {
		case RoleTool:
			role = "tool" // canonical OpenAI tool-result role
		case RoleReport:
			role = "assistant"
		case RoleSummary:
			role = "system"
		}

		// Tool-result message: role=tool, content=<result>, plus
		// the matching tool_call_id (OpenAI Cookbook + LM Studio
		// docs). Verified empirically with cmd/tooltest-local
		// proper-mode against gemma-4.
		if m.Role == RoleTool {
			req.Messages = append(req.Messages, requestMessage{
				Role:       role,
				Content:    m.Content,
				ToolName:   m.ToolName,
				ToolCallID: m.ToolCallID,
			})
			continue
		}

		// Assistant turn that issued tool calls: emit
		// `tool_calls` array with id/type/function. Content may
		// be empty (model emitted no narrative) — pass nil to
		// drop the key, matching OpenAI's null content semantics
		// for tool-call-only assistant messages.
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			calls := make([]requestToolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				calls[i] = requestToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: requestToolCallFunction{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				}
			}
			var content any = m.Content
			if m.Content == "" {
				content = nil
			}
			req.Messages = append(req.Messages, requestMessage{
				Role:      role,
				Content:   content,
				ToolCalls: calls,
			})
			continue
		}

		if len(m.ImageURLs) > 0 {
			// Multimodal: emit one user turn per image, then a
			// final user turn carrying the original text. This
			// works around llama.cpp's mmproj multi-image slot
			// reuse bug (see docs/{en,ja}/multi-image-handling
			// {,.ja}.md): multiple image_url parts in one prompt
			// can have their positional binding to
			// <start_of_image> markers reordered. Splitting into
			// separate user turns gives each image its own prompt
			// region.
			//
			// For Vertex (which has no such bug) the same
			// llm.Message is packed into a single Content block
			// in vertex.go; the split lives only here.
			for i, imgURL := range m.ImageURLs {
				req.Messages = append(req.Messages, requestMessage{
					Role: role,
					Content: []contentPart{
						{Type: "text", Text: imageIDPrefix(i, m.ObjectIDs)},
						{Type: "image_url", ImageURL: &imageURL{URL: imgURL}},
					},
				})
			}
			if m.Content != "" {
				req.Messages = append(req.Messages, requestMessage{
					Role:    role,
					Content: m.Content,
				})
			}
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

// MaxLocalResponseBytes caps the success-path response body the
// local backend will read. A misconfigured / hostile endpoint
// returning a multi-gigabyte body would otherwise OOM the app —
// real LM Studio responses sit comfortably under a few MiB even
// with large tool-call args (security-hardening-2.md H12).
const MaxLocalResponseBytes = 16 * 1024 * 1024

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

	// Cap at MaxLocalResponseBytes+1 so we can detect overflow:
	// reading exactly cap+1 means the body is at least cap+1 long.
	limited := io.LimitReader(resp.Body, MaxLocalResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > MaxLocalResponseBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", MaxLocalResponseBytes)
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
