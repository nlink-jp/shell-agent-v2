# LLM Abstraction Layer — Design Document

> Date: 2026-04-26 (initial), 2026-05-02 (v0.1.19 reality sweep)
> Status: Shipped — all four phases below are merged. Phase 2's
> tool-calling round-trip was tightened in v0.1.19 (parallel
> FunctionResponse coalescing, Gemini Thought-Part filtering, and
> the ToolCalls round-trip on persisted Records).
> Related: [Agent Data Flow](agent-data-flow.md),
> [Object Storage](object-storage.md),
> [Tool-call round-trip](tool-call-roundtrip.md).

## 1. Problem

The current `llm.Backend` interface leaks backend-specific concerns:

1. **Role mapping done in wrong layer** — `BuildMessages` (chat.go) maps
   `tool` → `user` as a gemma-4/LM Studio workaround, but Vertex AI
   (Gemini) handles `tool` role natively. A single mapping breaks one
   backend or the other.

2. **Tool definitions not converted** — `ToolDef` uses OpenAI JSON Schema
   format. Vertex AI requires `genai.FunctionDeclaration`. The conversion
   is marked as TODO.

3. **Multimodal not abstracted** — `Message.ImageURLs` carries data URLs,
   which Local backend sends as OpenAI Vision content parts. Vertex AI
   requires `genai.NewPartFromBytes()` with raw binary. The Vertex backend
   ignores images entirely.

4. **Tool result format not abstracted** — Local (gemma-4) needs tool
   results as `role="user"` to avoid re-invocation loops. Vertex AI
   (Gemini) uses native `FunctionResponse` parts.

Root cause: the abstraction passes raw `Message` structs through, making
each backend responsible for understanding application-level role semantics.
The interface should define **what** to send, not **how** each backend
formats it.

## 2. Current Interface

```go
type Message struct {
    Role      string   // user|assistant|tool|report|summary
    Content   string
    ImageURLs []string // data URLs
}

type Backend interface {
    Chat(ctx, messages []Message, tools []ToolDef) (*Response, error)
    ChatStream(ctx, messages []Message, tools []ToolDef, cb StreamCallback) (*Response, error)
    Name() string
}
```

Problems:
- `Message.Role` is an application domain concept, not an API concept
- Each backend must know all possible roles and how to map them
- `ImageURLs` as data URLs is Local-specific (Vertex needs binary)
- `ToolDef.Parameters` is OpenAI JSON Schema (Vertex needs `genai.Schema`)

## 3. Proposed Interface

### 3.1 Message Model

Keep `Message` as an application-level type with explicit roles:

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"
    RoleReport    Role = "report"
    RoleSummary   Role = "summary"
    RoleSystem    Role = "system"
)

type Message struct {
    Role      Role
    Content   string
    ImageURLs []string // data URLs — backends resolve to their format
    ToolName  string   // for RoleTool: which tool produced this result
}
```

### 3.2 Backend Responsibility

Each backend is responsible for:

| Concern | Local (LM Studio) | Vertex AI (Gemini) |
|---------|-------------------|-------------------|
| System prompt | First message, `role="system"` | `GenerateContentConfig.SystemInstruction` |
| User message | `role="user"` | `genai.RoleUser` |
| Assistant message | `role="assistant"` (with `tool_calls` array when calling tools) | `genai.RoleModel` (with `FunctionCall` Parts when calling tools) |
| Tool result | `role="tool"` + `tool_call_id` (canonical OpenAI shape; verified empirically with gemma-4 in `cmd/tooltest-local`) | `genai.Part{FunctionResponse{Name, Response}}` — parallel calls in one assistant turn must be packed into a **single** user `Content` (Vertex 400 otherwise) |
| Report | `role="assistant"` | `genai.RoleModel` |
| Summary | `role="system"` | `SystemInstruction` (appended) |
| Images | OpenAI Vision content array; one image per user turn for ≥2 images (mmproj slot reuse fix) | `genai.NewPartFromBytes()` packed into a single Content with the text |
| Tool definitions | OpenAI `tools` parameter | `genai.Tool{FunctionDeclarations}` (Parameters as raw JSON Schema via `ParametersJsonSchema`) |
| Tool calls in response | `response.choices[0].message.tool_calls` | `response.Candidates[0].Content.Parts[].FunctionCall` (Parts where `Thought == true` are dropped before content concatenation; see §6 below) |
| Streaming | Available via `ChatStream`, not currently invoked from `agentLoop` (tool chaining precludes knowing the final round) | Same |
| Round-trip persistence | `assistant.tool_calls` reconstructed from `memory.Record.ToolCalls` | `genai.Part{FunctionCall}` reconstructed from the same field — see [tool-call-roundtrip.md](./tool-call-roundtrip.md) |

### 3.3 Conversion Layer

Each backend implements `convertMessages()` internally:

```go
// Local backend
func (l *Local) convertMessages(messages []Message) []requestMessage {
    for _, m := range messages {
        switch m.Role {
        case RoleTool:    role = "user"      // gemma-4 workaround
        case RoleReport:  role = "assistant"
        case RoleSummary: role = "system"
        default:          role = string(m.Role)
        }
        // Handle ImageURLs → OpenAI Vision content parts
    }
}

// Vertex backend
func (v *Vertex) convertMessages(messages []Message) []*genai.Content {
    for _, m := range messages {
        switch m.Role {
        case RoleSystem:  → SystemInstruction (skip from contents)
        case RoleAssistant, RoleReport: → genai.RoleModel
        case RoleTool:    → genai.Part{FunctionResponse{Name, Response}}
        case RoleSummary: → SystemInstruction (append)
        default:          → genai.RoleUser
        }
        // Handle ImageURLs → genai.NewPartFromBytes()
    }
}
```

### 3.4 Tool Definition Conversion

```go
// Local: already in OpenAI format, pass through
func (l *Local) convertTools(tools []ToolDef) []requestTool { ... }

// Vertex: convert to genai format
func (v *Vertex) convertTools(tools []ToolDef) []*genai.Tool {
    var decls []*genai.FunctionDeclaration
    for _, t := range tools {
        decls = append(decls, &genai.FunctionDeclaration{
            Name:        t.Name,
            Description: t.Description,
            Parameters:  convertToGenaiSchema(t.Parameters),
        })
    }
    return []*genai.Tool{{FunctionDeclarations: decls}}
}
```

### 3.5 Tool Call Response Parsing

```go
// Local: parsed from response.choices[0].message.tool_calls
// Already implemented

// Vertex: extract from response.Candidates[0].Content.Parts
func extractToolCalls(resp *genai.GenerateContentResponse) []ToolCall {
    for _, part := range resp.Candidates[0].Content.Parts {
        if part.FunctionCall != nil {
            calls = append(calls, ToolCall{
                Name:      part.FunctionCall.Name,
                Arguments: marshalArgs(part.FunctionCall.Args),
            })
        }
    }
}
```

## 4. BuildMessages Changes

`BuildMessages` (chat.go) should **NOT** do any role mapping. It constructs
`[]Message` with application-level roles and passes them through:

```go
func (e *Engine) BuildMessages(...) []Message {
    // System prompt
    messages = append(messages, Message{Role: RoleSystem, Content: ...})

    // Session records — preserve roles as-is
    for _, r := range session.Records {
        messages = append(messages, Message{
            Role:      Role(r.Role),  // user|assistant|tool|report|summary
            Content:   content,
            ImageURLs: r.ImageURLs,
            ToolName:  r.ToolName,
        })
    }
    return messages
}
```

Each backend then converts these application-level messages to its
API-specific format.

## 5. Multimodal Handling

### 5.1 Local Backend (OpenAI Vision)

Already implemented. data URLs in `Message.ImageURLs` → OpenAI content
array with `image_url` parts.

### 5.2 Vertex AI Backend (genai Parts)

Reference: gem-cli `internal/input/input.go:fileToInlineData()`

```go
func dataURLToGenaiPart(dataURL string) *genai.Part {
    // Parse "data:image/png;base64,..." → mime + bytes
    mime, data := parseDataURL(dataURL)
    return genai.NewPartFromBytes(data, mime)
}
```

Images mixed with text in a single Content:

```go
content := &genai.Content{
    Role: genai.RoleUser,
    Parts: []*genai.Part{
        genai.NewPartFromText(text),
        dataURLToGenaiPart(imageURL1),
        dataURLToGenaiPart(imageURL2),
    },
}
```

## 6. Existing Code to Reuse

| Source | Code | Reuse for |
|--------|------|-----------|
| gem-cli | `internal/input/input.go:fileToInlineData()` | Image bytes → genai.Part |
| gem-cli | `internal/input/input.go:detectMIME()` | MIME type detection |
| gem-cli | `internal/client/client.go:Generate()` | genai client setup pattern |
| data-agent | `internal/llm/vertexai.go` | Tool definition conversion pattern |
| shell-agent v1 | `internal/client/client.go` | OpenAI streaming parser |
| shell-agent v1 | `app/react.go:parseGemmaToolCalls()` | Gemma text tool call parsing |

## 7. Implementation Checklist

### Phase 1: Remove role mapping from chat.go (shipped, v0.1.13)
- [x] Define Role constants in llm package
- [x] Add ToolName field to Message
- [x] BuildMessages passes roles as-is (no switch/case mapping)
- [x] Move tool-result handling into Local backend `convertMessages()`
- [x] Move report→assistant, summary→system into Local backend

### Phase 2: Vertex AI tool calling (shipped; tightened v0.1.19)
- [x] Implement `convertTools()` → `genai.FunctionDeclaration`
- [x] Implement tool call parsing from `Part.FunctionCall`
- [x] Implement `FunctionResponse` for tool results
- [x] **v0.1.18**: persist `assistant.ToolCalls` so the next round
      can reconstruct the protocol-correct prior-turn FunctionCall
      / `tool_calls` (without this, gemma-4 drops the linkage and
      Vertex sometimes re-issues the same call)
- [x] **v0.1.19**: coalesce parallel FunctionResponse Parts into a
      single user Content — Gemini rejects N responses for N
      parallel calls otherwise
- [x] **v0.1.19**: pass `ThinkingConfig{IncludeThoughts: false}`
      and filter `Part.Thought == true` in `parseResponse` /
      `extractText` so chain-of-thought never leaks into
      assistant content
- [x] **v0.1.18**: empirical verification harnesses
      `app/cmd/tooltest-vertex` and `app/cmd/tooltest-local`
      (proper / hack / loop modes) confirm the docs-compliant
      shape works on both backends

### Phase 3: Vertex AI multimodal (shipped, v0.1.16)
- [x] Implement `dataURLToGenaiPart()` conversion
- [x] Handle mixed text+image Content building
- [x] **v0.1.16**: image ID anchor lines (`Image (object ID: x):`)
      so the model can correlate visible image content with the
      object ID it should reference in reports — works around the
      llama.cpp mmproj multi-image slot-reuse bug on Local

### Phase 4: Testing (shipped)
- [x] Vertex AI integration tests
- [x] Backend-switching test (same conversation, different backends)
- [x] Multimodal test with both backends (verified up to N=8 on
      Gemma 3 multimodal in v0.1.17)
