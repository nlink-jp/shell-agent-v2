package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"google.golang.org/genai"
)

// Vertex is a Vertex AI (Gemini) backend using ADC.
type Vertex struct {
	cfg config.VertexAIConfig
}

// NewVertex creates a new Vertex AI backend.
func NewVertex(cfg config.VertexAIConfig) *Vertex {
	return &Vertex{cfg: cfg}
}

// Name returns the backend identifier.
func (v *Vertex) Name() string { return "vertex_ai" }

// Chat sends messages and returns the complete response.
func (v *Vertex) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	client, err := v.newClient(ctx)
	if err != nil {
		return nil, err
	}

	sysInstruction := v.buildSystemInstruction(messages)
	contents := v.buildContents(messages)

	resp, err := client.Models.GenerateContent(ctx, v.cfg.Model, contents, &genai.GenerateContentConfig{
		SystemInstruction: sysInstruction,
	})
	if err != nil {
		return nil, fmt.Errorf("vertex AI: %w", err)
	}

	text := extractText(resp)
	result := &Response{Content: text}
	if resp.UsageMetadata != nil {
		result.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
		result.OutputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
	}
	return result, nil
}

// ChatStream sends messages and streams the response via callback.
func (v *Vertex) ChatStream(ctx context.Context, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error) {
	client, err := v.newClient(ctx)
	if err != nil {
		return nil, err
	}

	sysInstruction := v.buildSystemInstruction(messages)
	contents := v.buildContents(messages)

	iter := client.Models.GenerateContentStream(ctx, v.cfg.Model, contents, &genai.GenerateContentConfig{
		SystemInstruction: sysInstruction,
	})

	var fullContent strings.Builder
	var lastUsage *genai.GenerateContentResponseUsageMetadata

	for resp, err := range iter {
		if err != nil {
			return nil, fmt.Errorf("vertex AI stream: %w", err)
		}
		token := extractText(resp)
		if token != "" {
			fullContent.WriteString(token)
			if cb != nil {
				cb(token, nil, false)
			}
		}
		if resp.UsageMetadata != nil {
			lastUsage = resp.UsageMetadata
		}
	}

	if cb != nil {
		cb("", nil, true)
	}

	result := &Response{Content: fullContent.String()}
	if lastUsage != nil {
		result.PromptTokens = int(lastUsage.PromptTokenCount)
		result.OutputTokens = int(lastUsage.CandidatesTokenCount)
	}
	return result, nil
}

func (v *Vertex) newClient(ctx context.Context) (*genai.Client, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  v.cfg.ProjectID,
		Location: v.cfg.Region,
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		return nil, fmt.Errorf("vertex AI client: %w", err)
	}
	return client, nil
}

// buildSystemInstruction collects system + summary messages.
// Design: docs/en/llm-abstraction.md Section 3.3
func (v *Vertex) buildSystemInstruction(messages []Message) *genai.Content {
	var parts []string
	for _, m := range messages {
		switch m.Role {
		case RoleSystem, RoleSummary:
			parts = append(parts, m.Content)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return genai.NewContentFromText(strings.Join(parts, "\n\n"), genai.RoleUser)
}

// buildContents maps application-level messages to genai Content.
// Design: docs/en/llm-abstraction.md Section 3.3
func (v *Vertex) buildContents(messages []Message) []*genai.Content {
	var contents []*genai.Content
	for _, m := range messages {
		switch m.Role {
		case RoleSystem, RoleSummary:
			continue // handled by SystemInstruction
		case RoleAssistant, RoleReport:
			contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleModel))
		case RoleTool:
			// Vertex AI supports tool role natively via FunctionResponse.
			// For now, send as user until Phase 2 (FunctionResponse).
			contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleUser))
		default:
			contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleUser))
		}
	}
	return contents
}

func extractText(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	return sb.String()
}
