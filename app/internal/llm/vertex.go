package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/logger"
	"google.golang.org/genai"
)

// Vertex is a Vertex AI (Gemini) backend using ADC.
// Design: docs/en/llm-abstraction.md
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

	gcConfig := &genai.GenerateContentConfig{
		SystemInstruction: v.buildSystemInstruction(messages),
	}
	if len(tools) > 0 {
		gcConfig.Tools = v.convertTools(tools)
	}

	contents := v.buildContents(messages)

	resp, err := client.Models.GenerateContent(ctx, v.cfg.Model, contents, gcConfig)
	if err != nil {
		return nil, fmt.Errorf("vertex AI: %w", err)
	}

	return v.parseResponse(resp), nil
}

// ChatStream sends messages and streams the response via callback.
func (v *Vertex) ChatStream(ctx context.Context, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error) {
	client, err := v.newClient(ctx)
	if err != nil {
		return nil, err
	}

	gcConfig := &genai.GenerateContentConfig{
		SystemInstruction: v.buildSystemInstruction(messages),
	}
	if len(tools) > 0 {
		gcConfig.Tools = v.convertTools(tools)
	}

	contents := v.buildContents(messages)

	iter := client.Models.GenerateContentStream(ctx, v.cfg.Model, contents, gcConfig)

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

// --- Message conversion ---

// buildSystemInstruction collects system + summary messages.
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
//
// Special handling: when the assistant turn issued multiple
// FunctionCall parts (parallel tool calls), Gemini requires the
// matching FunctionResponse parts to be packed into a single
// user Content block. Sending one user Content per tool result
// triggers HTTP 400:
//   "Please ensure that the number of function response parts is
//    equal to the number of function call parts of the function
//    call turn."
// We coalesce consecutive RoleTool messages into one Content.
func (v *Vertex) buildContents(messages []Message) []*genai.Content {
	var contents []*genai.Content
	flushTools := func(parts []*genai.Part) []*genai.Part {
		if len(parts) > 0 {
			contents = append(contents, &genai.Content{
				Role:  genai.RoleUser,
				Parts: parts,
			})
		}
		return nil
	}
	var pendingToolParts []*genai.Part
	for _, m := range messages {
		// Any non-tool message flushes the pending tool batch.
		if m.Role != RoleTool {
			pendingToolParts = flushTools(pendingToolParts)
		}
		switch m.Role {
		case RoleSystem, RoleSummary:
			continue // handled by SystemInstruction
		case RoleAssistant, RoleReport:
			// When the assistant turn carries tool calls, emit
			// FunctionCall parts on the model role so the next
			// FunctionResponse pairs correctly. Documented round-
			// trip per ai.google.dev/gemini-api/docs/function-calling
			// Step 4. Without this, the FunctionResponse becomes
			// orphaned and Gemini can re-issue the same call.
			if len(m.ToolCalls) > 0 {
				var parts []*genai.Part
				if m.Content != "" {
					parts = append(parts, genai.NewPartFromText(m.Content))
				}
				for _, tc := range m.ToolCalls {
					var args map[string]any
					if tc.Arguments != "" {
						_ = json.Unmarshal([]byte(tc.Arguments), &args)
					}
					if args == nil {
						args = map[string]any{}
					}
					parts = append(parts, genai.NewPartFromFunctionCall(tc.Name, args))
				}
				contents = append(contents, &genai.Content{
					Role:  genai.RoleModel,
					Parts: parts,
				})
			} else {
				contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleModel))
			}
		case RoleTool:
			// Coalesce: append the FunctionResponse part to the
			// pending batch instead of emitting its own Content.
			// The batch flushes when the next non-tool message
			// arrives (or at end-of-loop, below).
			pendingToolParts = append(pendingToolParts,
				genai.NewPartFromFunctionResponse(m.ToolName, map[string]any{
					"result": m.Content,
				}),
			)
		default:
			// User and any other role — with optional images.
			// Each image is anchored with a one-line prefix naming
			// its object ID so the model can correlate visible
			// image content with the persistent ID it should
			// reference in reports. Format follows Google's
			// recommended Gemma multimodal pattern: short ID
			// label immediately preceding the image, no wrapping.
			if len(m.ImageURLs) > 0 {
				parts := []*genai.Part{genai.NewPartFromText(m.Content)}
				for i, dataURL := range m.ImageURLs {
					parts = append(parts, genai.NewPartFromText(imageIDPrefix(i, m.ObjectIDs)))
					if p := dataURLToGenaiPart(dataURL); p != nil {
						parts = append(parts, p)
					}
				}
				contents = append(contents, &genai.Content{
					Role:  genai.RoleUser,
					Parts: parts,
				})
			} else {
				contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleUser))
			}
		}
	}
	// Flush any trailing tool batch — this is the common case when
	// the agent loop hands us [..., assistant(tool_calls), tool, tool]
	// and re-invokes the model immediately for the next round.
	pendingToolParts = flushTools(pendingToolParts)
	return contents
}

// --- Tool conversion ---

// convertTools converts ToolDef to genai Tool format.
func (v *Vertex) convertTools(tools []ToolDef) []*genai.Tool {
	var decls []*genai.FunctionDeclaration
	for _, t := range tools {
		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJsonSchema: t.Parameters,
		})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// --- Response parsing ---

// parseResponse extracts text and tool calls from genai response.
//
// Gemini emits chain-of-thought as separate parts with
// part.Thought == true (see genai/types.go). These are internal
// reasoning that the model wants to keep but the user shouldn't
// see — without filtering, the assistant bubble shows raw
// "THOUGHT\n..." text alongside the final answer.
func (v *Vertex) parseResponse(resp *genai.GenerateContentResponse) *Response {
	result := &Response{}
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return result
	}

	parts := resp.Candidates[0].Content.Parts
	logger.Debug("vertex parseResponse: %d parts", len(parts))

	var textParts []string
	for i, part := range parts {
		// Diagnostic: dump enough about each part to tell whether
		// "思考"/"THOUGHT" preambles arrive in their own Part with
		// Thought=true (which we already filter) or get fused into
		// a single text Part with the rest of the reply (which the
		// filter cannot reach). Truncate to keep logs bounded.
		head := part.Text
		if len(head) > 80 {
			head = head[:80]
		}
		hasFC := part.FunctionCall != nil
		logger.Debug("vertex parseResponse part[%d]: thought=%v textLen=%d funcCall=%v textHead=%q",
			i, part.Thought, len(part.Text), hasFC, head)

		if part.Thought {
			continue
		}
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			args, _ := json.Marshal(part.FunctionCall.Args)
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        part.FunctionCall.ID,
				Name:      part.FunctionCall.Name,
				Arguments: string(args),
			})
		}
	}
	result.Content = strings.Join(textParts, "")

	if resp.UsageMetadata != nil {
		result.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
		result.OutputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
	}
	return result
}

// dataURLToGenaiPart converts a data URL to a genai Part for multimodal input.
// Format: "data:image/png;base64,iVBOR..."
// Reuses gem-cli pattern: genai.NewPartFromBytes(data, mime)
func dataURLToGenaiPart(dataURL string) *genai.Part {
	// Parse "data:image/png;base64,..." → mime + bytes
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return nil
	}
	header := parts[0] // "data:image/png;base64"
	mime := ""
	if strings.HasPrefix(header, "data:") {
		mime = strings.TrimPrefix(header, "data:")
		mime = strings.TrimSuffix(mime, ";base64")
	}
	if mime == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	return genai.NewPartFromBytes(data, mime)
}

func extractText(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Thought {
			continue // chain-of-thought is internal; never stream it
		}
		if part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	return sb.String()
}
