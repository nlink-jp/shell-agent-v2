# Tool-Call Round-Trip — Design Document

> Date: 2026-05-02
> Status: Shipped — `memory.Record.ToolCalls` round-trip
> persistence and parallel-FunctionResponse coalescing both
> merged in v0.1.19. The parallel-call coalescing was the
> finishing piece: Gemini rejected `N calls / 1 response each`
> as HTTP 400 once the model started using parallel tool
> calls in real sessions. Empirical verification harnesses
> ship under `app/cmd/tooltest-vertex` and
> `app/cmd/tooltest-local`.
> Scope: Persist and replay assistant-side tool-call
> structure end-to-end so Vertex (and the OpenAI-format
> local backend) recognise their own function calls. Fixes
> a class of "Vertex re-runs the same successful tool" loops
> plus the parallel-call HTTP 400 regression.
>
> Cited sources (verified before drafting):
>   - [Gemini Function Calling docs](https://ai.google.dev/gemini-api/docs/function-calling)
>   - [Vertex AI Function Calling docs](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/function-calling)
>   - [OpenAI Cookbook — how_to_call_functions_with_chat_models](https://cookbook.openai.com/examples/how_to_call_functions_with_chat_models)
>   - [LM Studio OpenAI-compat tools](https://lmstudio.ai/docs/developer/openai-compat/tools)
>   - [googleapis/python-genai #813 — duplicate function call hallucination](https://github.com/googleapis/python-genai/issues/813)
>   - [Portkey error library 10067 — `tool_calls` must be followed by tool messages](https://portkey.ai/error-library/tool-call-response-error-10067)

## 1. Problem

Real session, 2026-05-02, sandbox-run-python prime sieve:

```
Round 0: assistant content="承知…" + sandbox-run-python   → success "[2,3,…,97]"
Round 1: assistant content=""    + sandbox-run-python (same code) → same result
Round 2: assistant content=""    + sandbox-run-python (same code) → same result
Round 3: assistant content="…素数は以下…" (final)
```

The LLM (Vertex / gemini-2.5-flash) issued the same tool
call three times in a row and only summarised on round 3.
Loop detection (Feature 1, v0.1.16) is gated on
`status=error`; this is all-success, so it doesn't fire.

Tracing why Vertex repeats: we send
`genai.NewPartFromFunctionResponse(toolName, {result: "…"})`
on the user side, but on the assistant side we emit the
prior turn as **plain text**
(`genai.NewContentFromText("[Calling: sandbox-run-python]", RoleModel)`).
The corresponding `FunctionCall` part is never sent.

From Vertex's protocol POV:

```
[user: text "run a prime sieve"]
[model: text "[Calling: sandbox-run-python]"]   ← no FunctionCall part
[user: FunctionResponse(sandbox-run-python, …)] ← orphan
[model: ???]
```

This violates Google's documented round-trip pattern. The
official Gemini docs ([function-calling](https://ai.google.dev/gemini-api/docs/function-calling),
"Step 4 — Create user friendly response") show:

```python
contents.append(response.candidates[0].content)  # the FunctionCall
contents.append(types.Content(role="user", parts=[function_response_part]))
```

The model's content (containing the `functionCall` Part)
must be appended verbatim to history before the
`functionResponse`. Synthesising a text stand-in like
`"[Calling: foo]"` is documented as wrong. The same is
shown verbatim in the Vertex AI mirror docs and in
Google's Python/Go/Node/Java/REST samples.

The user-visible symptom we hit — Gemini re-issuing an
identical `functionCall` after an orphan response — is
the documented failure mode. There are public reports of
the same hallucination class:
[python-genai #813](https://github.com/googleapis/python-genai/issues/813),
[Multimodal Live API duplicate-call bug](https://discuss.ai.google.dev/t/bug-multimodal-live-api-v1beta-triggers-identical-tool-calls-twice-in-rapid-succession/135700).

**The local OpenAI-compatible backend (LM Studio + gemma)
is intentionally NOT in scope.** The codebase already runs
a gemma-friendly workaround: `local.go:241` maps
`RoleTool → role="user"`, with the comment *"gemma-4
stays in tool-calling mode with role=\"user\""*. The
local path doesn't currently use OpenAI's `tool_calls` /
`tool_call_id` round-trip at all; it threads tool results
back as plain user messages, and gemma was apparently
trained to continue tool-calling from that shape. The
duplicate-call symptom described above has been observed
**only on Vertex**.

Switching the local path to canonical OpenAI tool_calls
(`role: "assistant", tool_calls: [...]` followed by
`role: "tool", tool_call_id: ...`) is a separate change
with its own risk surface — it might fix non-gemma
OpenAI-compatible models but could regress gemma's
current working flow. Out of scope until we either
(a) reproduce a duplicate-call bug on local, or (b)
support a non-gemma OpenAI-compat model where the
current shim is the bottleneck.

## 2. Goals / Non-goals

### Goals

1. Persist `resp.ToolCalls` on the assistant `Record`
   when the LLM emits a function call, so reloading and
   replaying a session keeps the assistant→tool pairing
   intact.
2. Propagate ToolCalls through the build pipeline
   (`chat.BuildMessages*`, `contextbuild.Build`) into
   `llm.Message`.
3. Emit assistant tool calls **on Vertex only** as
   `genai.NewPartFromFunctionCall(name, args)` on the
   model role, alongside the optional text part. The
   local backend's existing gemma-friendly shim
   (`RoleTool → role="user"` plain text) stays as-is.
4. Backward compatible: pre-fix session records (no
   ToolCalls field) keep working — the empty case
   degrades gracefully to today's text-only emission.

### Non-goals

- **No local-backend tool_calls migration.** See §1.
  The current `RoleTool → role="user"` shim is left
  alone. `local.go` reads ToolCalls from the new field
  but only to fall through gracefully on tool-call-only
  assistant turns; no protocol change.
- **No retroactive repair of existing sessions.** Sessions
  saved before this change still suffer the bug if
  re-loaded. Acceptable: the bug only matters when the
  agent loop continues from those records, which is rare.
- **No MCP-side tool-call structure work.** MCP guardian
  tool calls already round-trip via Tool result content;
  the assistant-side bookkeeping is the LLM's
  responsibility (same code path as native tools after
  this fix).
- **No streaming-side rework.** Streaming is currently
  disabled (`canStream := false`); tool-call structure on
  streaming partial deltas is out of scope.
- **No "Calling:" placeholder removal.** The text
  "[Calling: foo]" is still emitted as the visible chat
  content when the LLM produces no narrative. The chat UI
  uses it; we just stop sending it as the assistant
  representation to Vertex when ToolCalls is populated
  (Vertex gets the proper FunctionCall part instead).

## 3. Detailed design

### 3.1 Persisted record format

`memory.Record` gains:

```go
type Record struct {
    // … existing fields …

    // ToolCalls captures function calls the assistant
    // issued in this turn. Populated only when Role ==
    // "assistant" and the response carried tool calls.
    // Replayed verbatim on subsequent agent loop runs so
    // the LLM (Vertex FunctionCall / OpenAI tool_calls)
    // sees a well-formed assistant→tool pairing.
    ToolCalls []ToolCallRecord `json:"tool_calls,omitempty"`
}

type ToolCallRecord struct {
    ID        string `json:"id"`
    Name      string `json:"name"`
    Arguments string `json:"arguments"` // raw JSON string the LLM emitted
}
```

`AddAssistantMessage` keeps its current signature; a new
`AddAssistantMessageWithToolCalls(content string, calls
[]ToolCallRecord)` is added for the tool-call path.

Old session JSON files lack the field; `omitempty` keeps
the field out of fresh records that don't have any.

### 3.2 LLM message carrier

`llm.Message`:

```go
type Message struct {
    Role      Role
    Content   string
    ImageURLs []string
    ObjectIDs []string
    ToolName  string
    ToolCalls []ToolCall  // NEW — populated when Role == RoleAssistant
}
```

`llm.ToolCall` is the existing struct (already used for
agent-loop dispatch). The fields line up 1:1 with
`memory.ToolCallRecord`.

### 3.3 Build pipelines

Both `chat.BuildMessages` / `chat.BuildMessagesWithBudget`
and `contextbuild.Build` copy `r.ToolCalls` into the
emitted `llm.Message`. No format change to either build
path; the field is just carried through.

### 3.4 Vertex emit

`vertex.buildContents`:

```go
case RoleAssistant, RoleReport:
    if len(m.ToolCalls) > 0 {
        // FunctionCall(s) — pair with the user-side
        // FunctionResponse on the next message. Optional
        // narrative text comes first as a Part.
        var parts []*genai.Part
        if m.Content != "" && !strings.HasPrefix(m.Content, "[Calling:") {
            parts = append(parts, genai.NewPartFromText(m.Content))
        }
        for _, tc := range m.ToolCalls {
            args := map[string]any{}
            _ = json.Unmarshal([]byte(tc.Arguments), &args)
            parts = append(parts, genai.NewPartFromFunctionCall(tc.Name, args))
        }
        contents = append(contents, &genai.Content{
            Role:  genai.RoleModel,
            Parts: parts,
        })
    } else {
        contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleModel))
    }
```

Skipping the `[Calling: …]` placeholder text is important:
when an assistant has no narrative and only a tool call,
the placeholder duplicates the FunctionCall and confuses
the model further.

**Gemini 3 caveat**: The Gemini docs note that
*"Gemini 3 now always returns a unique id with every
functionCall. Include this exact id in your
functionResponse so the model can accurately map your
result back to the original request."* `genai.ToolCall`
already carries `ID`; we route it into
`agent.session.AddToolResult(tc.ID, …)` as today, and
`memory.Record.ToolCallID` survives the round trip. With
this fix that id flows back via `FunctionResponse` (its
default behaviour in the Go SDK) and is correctly paired
with the `FunctionCall` we now emit on the assistant side.

### 3.5 Local (OpenAI-compatible) emit — unchanged

`local.go`'s `buildRequest` is **not modified by this
change**. The current `RoleTool → role="user"` mapping
plus `[Calling: foo]` placeholder text continues. The
build pipeline now puts `ToolCalls` on `llm.Message`, but
`local.go` ignores the field for now.

Why no change here:
- The duplicate-call symptom we're fixing was observed
  only on Vertex.
- gemma-4 (the user's local model) has a documented
  workaround in our codebase that depends on the current
  shape; switching to canonical OpenAI tool_calls might
  regress it.
- Switching local to the canonical OpenAI tool_calls
  protocol is its own design — out of scope here.

### 3.6 Agent loop wiring

`internal/agent/agent.go` agentLoop, after the LLM call:

```go
if len(resp.ToolCalls) > 0 {
    // … existing toolNames building …
    a.session.AddAssistantMessageWithToolCalls(resp.Content, resp.ToolCalls)
} else {
    a.session.AddAssistantMessage(resp.Content)
}
```

`resp.Content` may be empty when the model emits only a
function call; we still record it (so the chat UI can
substitute "[Calling: …]" at render time). The
substitution stays in place for chat-UI display; only the
LLM-bound message conversion suppresses it.

### 3.7 Chat UI

No frontend change. The placeholder text continues to
appear in the chat for tool-call-only assistant turns; the
fix only affects what we send to the LLM next turn.

## 4. Touched files

| File | Change |
|---|---|
| `internal/memory/memory.go` | `Record.ToolCalls`, `ToolCallRecord`, new `AddAssistantMessageWithToolCalls` |
| `internal/llm/backend.go` | `Message.ToolCalls` |
| `internal/llm/vertex.go` | emit FunctionCall parts in assistant role; skip `[Calling:]` placeholder when ToolCalls is set |
| `internal/llm/local.go` | **no change** (see §3.5) |
| `internal/chat/chat.go` | propagate ToolCalls through BuildMessages and BuildMessagesWithBudget |
| `internal/contextbuild/builder.go` | propagate ToolCalls through Build |
| `internal/agent/agent.go` | call `AddAssistantMessageWithToolCalls` when `resp.ToolCalls` is non-empty |
| `internal/llm/vertex_test.go` (or wherever Vertex unit tests live) | new test: `TestVertex_BuildContents_AssistantToolCalls`, `TestVertex_SkipsCallingPlaceholder` |
| `internal/agent/agent_test.go` | new test: synthetic 3-round loop with mock backend, assert assistant tool calls land in session |

## 5. Test plan

### Unit
- **`TestVertex_BuildContents_AssistantToolCalls`**:
  verify the produced `[]*genai.Content` for an assistant
  with ToolCalls has Parts `[FunctionCall{Name, Args}]`
  (and optional preceding Text part when Content is
  non-placeholder).
- **`TestVertex_SkipsCallingPlaceholder`**: when Content
  starts with `[Calling:`, no Text part is emitted.
- **`TestLocalBuildRequest_PreservesGemmaShim`**: a
  local-backend message round-trip with `RoleTool` still
  produces `role: "user"` (not `role: "tool"`), confirming
  we didn't accidentally regress the gemma shim while
  adding the `Message.ToolCalls` field.

### Manual
1. **Repro before**: load the v0.1.18 build, ask Vertex
   "run a prime sieve". Observe N=2-3 redundant
   `sandbox-run-python` calls.
2. **Repro after**: same prompt on the post-fix build.
   Expect exactly one tool call followed by a
   text-only summary round.
3. **Existing-session compat**: open a saved session that
   used Vertex + a tool call before the fix. Continue the
   conversation. Old turns may still confuse the model
   because the records lack ToolCalls, but new turns work
   correctly.

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Assistant `Content` field ends up duplicated when both narrative text and ToolCalls are present (e.g. Vertex emits both a text Part and a FunctionCall Part — model could re-narrate) | The model sees what it produced last turn; reflecting it back is correct. The bug we're fixing is when *only* the placeholder text was sent without the FunctionCall. |
| `[Calling:]` placeholder filter triggers on a user message that legitimately starts with that string | Filter is scoped to `Role == RoleAssistant`; user content is unaffected. |
| ToolCallRecord.Arguments is invalid JSON (jsonfix-mangled, gemma hallucinated) | Vertex path: `json.Unmarshal` failure leaves `args = {}` and we still emit the FunctionCall — better than nothing. Local path: passes through verbatim (already happens today for the tool dispatch). |
| Existing sessions on disk re-load with empty ToolCalls and continue to confuse Vertex | Out of scope; sessions older than the fix are best abandoned for new turns. The risk is contained to "user resumes a long-running session and asks a follow-up that triggers a tool-call replay" — uncommon. |
| Local backend regression from new `ToolCalls` field on `llm.Message` | Local's `buildRequest` continues to ignore `ToolCalls`. A unit test asserts the gemma shim (RoleTool → role=user) survives. |

## 7. Phasing

Single commit:
**fix(llm): persist and replay assistant tool calls so
Vertex/OpenAI see well-formed function call→response
pairs.**

Touches memory, llm, chat, contextbuild, agent — all
strongly coupled, no value in splitting into phases.

v0.1.19 release after manual verification.

## 8. Out of scope

- Same-args dedupe / loop detection extension. The
  Gemini docs and observed reports
  ([python-genai #813](https://github.com/googleapis/python-genai/issues/813),
  [Multimodal Live API bug](https://discuss.ai.google.dev/t/bug-multimodal-live-api-v1beta-triggers-identical-tool-calls-twice-in-rapid-succession/135700))
  identify orphan-response history as the root cause; the
  protocol fix removes that root cause. Whether residual
  duplicate-call cases remain is empirical — verified
  during manual testing (§5). If they do, the dedupe
  extension can be added later as a separate signal.
- MCP tool-call structure changes. MCP guardian results
  flow through the same `RoleTool` path; once the
  assistant→tool pairing is fixed, MCP tools benefit
  identically. No MCP-specific work needed.
- Streaming partial-delta tool-call assembly.
- Migration utility for old sessions. Records saved
  before this fix have no `ToolCalls` and continue to
  produce orphan responses if their session is resumed.
  Fresh turns from existing sessions still benefit
  (anything appended after the fix is written with the
  new field).
- LM Studio + gemma's
  `[TOOL_REQUEST]…[END_TOOL_REQUEST]` parser limitation
  on the *response* side. We only fix what we send.
