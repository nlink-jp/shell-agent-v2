# Preserving Gemini 3 thought signatures across tool-use turns — Design Note

**Status:** Design draft (2026-05-13); pending approval.
**Target:** v0.6.2 (point release on top of v0.6.1)
**Reported by:** User — Vertex AI 400 error after migrating Vertex
backend to a Gemini 3 family model.

This note specifies how the Vertex AI backend captures and replays
the opaque "thought signature" tokens that Gemini 3 attaches to
response parts. Without this, any multi-step tool-use turn against
a Gemini 3 model fails with a HTTP 400 INVALID_ARGUMENT on the
second LLM round.

---

## 1. Problem

After the user switched the Vertex backend to a Gemini 3 family
model, the chat worked for plain Q&A but failed mid-tool-loop with:

```
Error: LLM: vertex AI: Error 400, Message: Unable to submit request
because function call `default_api:weather` in the 6. content block
is missing a `thought_signature`. Learn more:
https://docs.cloud.google.com/vertex-ai/generative-ai/docs/thought-signatures,
Status: INVALID_ARGUMENT, Details: []
```

### Chronology (from `app.log`, 2026-05-13 20:19)

1. User asks a question that triggers parallel weather lookups.
2. Vertex returns a response that emits five `weather` function
   calls in one turn. `parseResponse` (vertex.go:309-327) lifts
   `FunctionCall.Name` and `FunctionCall.Args` into `ToolCall` and
   discards the rest of the Part (including `Part.ThoughtSignature`,
   the opaque continuation blob Gemini 3 attaches per part).
3. Agent dispatches all five calls (each ≤ 22 args bytes), gathers
   the results, and feeds them back via `buildContents`.
4. `buildContents` (vertex.go:191-209) rebuilds the assistant turn's
   `model`-role Content with five `genai.NewPartFromFunctionCall`
   parts. `NewPartFromFunctionCall` does **not** set
   `ThoughtSignature`; the constructor only wires Name and Args
   (genai v1.54 `types.go:1452-1460`). The reconstructed turn is
   structurally identical to the original *except* the signatures
   are zero-valued.
5. Vertex rejects the next `GenerateContent` request because the
   model's reasoning continuity has been broken — the function call
   in content block 6 (the agent's replay of the assistant's tool
   turn) carries no signature.

### Why Gemini 3 enforces this

Per the Vertex AI thought-signatures documentation, Gemini 3
returns its reasoning state as opaque tokens attached to each Part
in a response. When the client replays the turn back as
conversation history (e.g. between tool-use rounds), those
signatures must travel with their parts. Sending a function call
back without its signature is a protocol violation — the model
treats it as a tampered or fabricated turn and 400s.

This requirement is **new in Gemini 3**. Gemini 2.5 (the previous
default for shell-agent-v2) attached no signatures, so the
existing parse-and-rebuild round-trip worked. The bug is dormant
until a user migrates the Vertex model setting.

---

## 2. Goals

- **Multi-step tool-use against Gemini 3 works.** Parallel and
  sequential tool calls round-trip through the agent loop without
  the 400 reported above.
- **No regression on Gemini 2.5 or earlier.** Older models emit no
  signatures; the captured slice stays empty and the new code path
  is a no-op.
- **No regression on the local backend.** Signature fields are
  Vertex-specific and the local OpenAI-compatible client never
  touches them.
- **Session export / import preserves signatures.** A session
  exported during a Gemini 3 conversation reimports without losing
  reasoning continuity.

Non-goals:

- Surfacing the (opaque) signature bytes in the UI.
- Decoding or inspecting signature contents. The Vertex docs are
  explicit that signatures are an opaque continuation token; we
  treat them as bytes-in, bytes-out.
- Optimising signature payload size. Signatures from Gemini 3 are
  tens-of-bytes per part; even at 10 parts per turn and 100 turns
  per session this is < 100 KB, negligible vs. message text and
  attachments.

---

## 3. Where signatures live in the response

Per the Vertex AI thought-signatures docs, `Part.ThoughtSignature`
(`genai/types.go:1397`) may be set on **three Part shapes** in a
single response:

| Part shape | Identifying field | Notes |
|------------|------------------|-------|
| Thought | `Thought == true` | Internal reasoning. `vertex.go:303` currently filters these out of user-visible content. |
| Text | `Text != ""`, no FunctionCall | The assistant's prose reply. `parseResponse` joins these into `m.Content`. |
| Function call | `FunctionCall != nil` | Tool invocations. `parseResponse` lifts these into `m.ToolCalls`. |

We configure `ThinkingConfig.IncludeThoughts = false`
(`vertex.go:47-49`), which suppresses *most* thought parts —
but a thought-style part may still arrive (the existing code
comment notes Gemini 2.5 Flash returned text-with-"THOUGHT" prefix
even with that flag off). Gemini 3 may continue to surface
opaque-signature-bearing parts even when no thought text is
exposed.

The reported error is specifically about a missing function-call
signature, but the documented contract is "echo back signatures
from all parts." Designing for only the function-call case would
leave the same class of bug latent for text-signature and
thought-signature cases.

---

## 4. Design

### 4.1 Data model — chosen approach (Option A)

Add three minimal fields to existing structs:

```go
// In llm.ToolCall
type ToolCall struct {
    ID               string
    Name             string
    Arguments        string
    ThoughtSignature []byte `json:"thought_signature,omitempty"`
}

// In llm.Message
type Message struct {
    // ... existing fields ...

    // ThoughtSignatures preserves opaque per-part Gemini 3+
    // continuation tokens from the model's response, grouped by
    // the part type that carried them. Each slice is ordered as
    // the original parts arrived. Vertex backend only; other
    // backends ignore.
    ThoughtSigsThought [][]byte `json:"thought_sigs_thought,omitempty"`
    ThoughtSigText     []byte   `json:"thought_sig_text,omitempty"`
}
```

Three buckets:

1. **Function-call signatures** — one per `ToolCall`, paired by
   position. The most common and the case that triggered this ADR.
2. **Thought-part signatures** — a slice on `Message` because a
   single assistant turn can have multiple thought parts. We
   discard the thought *text* (per existing privacy filtering) but
   capture the signature(s).
3. **Text-part signature** — single value because the existing
   parser concatenates all text parts into `m.Content`; we keep
   only the last-seen text-part signature. (Multiple text parts
   in one turn are rare; if we discover they matter, escalate to
   `[]byte` slice.)

### 4.2 Capture (parseResponse, vertex.go:279)

Walk parts as before. For each part:

- `part.Thought == true` → if `ThoughtSignature` non-empty, append
  to a local `thoughtSigs [][]byte`. Then `continue` (existing
  filter behaviour preserved).
- `part.Text != ""` → append text to `textParts` (existing); if
  `ThoughtSignature` non-empty, overwrite `textSig`.
- `part.FunctionCall != nil` → build `ToolCall` as before; set
  `tc.ThoughtSignature = part.ThoughtSignature`.

Attach `thoughtSigs` and `textSig` to the returned `Response`. The
response object becomes the assistant `Message` written into
session state, so the fields persist through serialisation.

### 4.3 Replay (buildContents, vertex.go:164)

When emitting the `RoleAssistant` model Content:

```go
case RoleAssistant, RoleReport:
    if len(m.ToolCalls) > 0 {
        var parts []*genai.Part

        // 1. Replay each captured thought signature as a thought
        //    part. Empty text is acceptable per the Vertex docs;
        //    the signature is the load-bearing payload.
        for _, sig := range m.ThoughtSigsThought {
            p := &genai.Part{Thought: true, ThoughtSignature: sig}
            parts = append(parts, p)
        }

        // 2. Replay text content with its signature (if any).
        if m.Content != "" {
            p := genai.NewPartFromText(m.Content)
            if len(m.ThoughtSigText) > 0 {
                p.ThoughtSignature = m.ThoughtSigText
            }
            parts = append(parts, p)
        }

        // 3. Replay each function call with its signature.
        for _, tc := range m.ToolCalls {
            var args map[string]any
            if tc.Arguments != "" {
                _ = json.Unmarshal([]byte(tc.Arguments), &args)
            }
            if args == nil { args = map[string]any{} }
            p := genai.NewPartFromFunctionCall(tc.Name, args)
            if len(tc.ThoughtSignature) > 0 {
                p.ThoughtSignature = tc.ThoughtSignature
            }
            parts = append(parts, p)
        }

        contents = append(contents, &genai.Content{
            Role:  genai.RoleModel,
            Parts: parts,
        })
    }
```

Order: thoughts → text → function calls. This matches the
typical Gemini 3 part order; rare reorderings may not be
preserved (see §6 risks).

### 4.4 Persistence

The new fields ride on `Message`, which is serialised through
`memory.Record` into session storage and `.shellagent` export
bundles. Go's encoding/json emits `[]byte` as base64 — automatic,
no custom marshaller needed. `omitempty` keeps older sessions and
non-Gemini-3 conversations clean (zero bytes added to JSON).

Inbound side (Load / Import): missing fields default to nil. Older
sessions resume on Gemini 3 with empty signature state, which is
correct — the agent will re-prompt and Gemini 3 will start fresh
signature emission from that turn forward.

### 4.5 ChatStream

The streaming path (`ChatStream`, vertex.go:65) currently returns
text only; it never surfaces tool calls. The agent loop uses
`Chat` (non-streaming) when tools are involved, so the signature
plumbing belongs there exclusively. ChatStream is unaffected by
this ADR.

---

## 5. Testing

### 5.1 Unit tests

Add to `vertex_test.go` (or a new `vertex_thought_signatures_test.go`):

- **`TestParseResponse_CapturesFunctionCallSignature`** — synthesise
  a `genai.GenerateContentResponse` with one function-call part
  carrying `ThoughtSignature: []byte{0x01, 0x02, 0x03}`; assert
  the returned `ToolCall.ThoughtSignature` equals the input.
- **`TestParseResponse_CapturesTextSignature`** — text-only part
  with a signature; assert `result.ThoughtSigText`.
- **`TestParseResponse_CapturesThoughtSignatures`** — two thought
  parts (`Thought: true`) with distinct signatures; assert
  `result.ThoughtSigsThought` length 2 in order.
- **`TestBuildContents_ReplaysFunctionCallSignature`** — input
  `Message` with one tool call carrying a signature; assert the
  resulting `[]*genai.Content` has a model Content whose
  function-call Part's `ThoughtSignature` matches.
- **`TestBuildContents_ReplaysThoughtSignatures`** — input message
  with `ThoughtSigsThought = [[a,b],[c,d]]`; assert two thought
  parts emitted in order with matching signatures and `Thought: true`.
- **`TestBuildContents_EmptySignaturesNoop`** — input with all
  signature fields empty; assert the emitted Content has no
  thought parts and zero-value `ThoughtSignature` on function-call
  parts. Regression test for Gemini 2.5 compatibility.

### 5.2 Integration

Mocking a full Vertex tool-use loop in tests is infeasible without
network. Manual smoke test required (see §8 rollout).

### 5.3 Session round-trip

- **`TestMessageJSONRoundTrip_PreservesSignatures`** in `llm/`
  (or `memory/`): marshal a `Message` carrying all three signature
  buckets to JSON, unmarshal, compare. Asserts the base64 encoding
  is symmetric.

---

## 6. Risks & open questions

- **Part order in re-emission.** Vertex docs do not explicitly
  state that the model requires the *exact* original part order
  on replay. Our implementation emits in canonical
  thoughts → text → function-calls order. If Gemini 3 turns out
  to be sensitive to the relative position of text vs function-call
  parts (e.g. text appears between two function calls), this design
  loses that ordering and may produce subtle responses. Mitigation:
  if observed, escalate to Option B (parts-array snapshot, see §7).
- **`ChatStream` divergence.** Streaming doesn't capture signatures
  (the streaming iterator surface in genai SDK doesn't expose them
  cleanly per part). For the foreseeable future, ChatStream is
  used only for tool-free conversational replies, so signatures
  don't matter there. If we extend ChatStream to tool use, revisit.
- **Backwards compatibility with older sessions.** Sessions
  recorded before this ADR have no signatures. Loading such a
  session and continuing it against Gemini 3 will fail the next
  tool-use round because the historical assistant turns lack
  signatures. **Mitigation**: documented limitation — restart the
  conversation when migrating an old session to Gemini 3. Auto-
  truncation is rejected as too invasive.
- **Privacy implications of signature persistence.** Signatures
  are opaque blobs but encode reasoning state. Persisting them in
  session JSON / `.shellagent` exports means anyone who reads the
  export sees them. Private sessions (per ADR-0003) keep these
  inside the user's data directory; we make no additional
  guarantees about their contents.
- **Bigger context window cost?** Signatures replayed on every
  request consume some prompt tokens (the model bills the encoded
  signature against the input budget). Best estimate from Google
  docs is a few tokens per signature; at typical conversation
  depths the cost is < 1% of the context budget. Not optimising.

---

## 7. Rejected alternatives

### 7.1 Option B — store the full Parts array snapshot

Instead of three per-field signature buckets, store an
ordered `AssistantParts []AssistantPart` where each carries
`{Kind, Text, ToolCall, Signature}`. parseResponse builds the
array verbatim; buildContents iterates and rebuilds.

Rejected for v0.6.2 because:

- Bigger data-model change touching every serialised `Message`
  in the wild.
- The order risk noted in §6 has not been observed in any reported
  Gemini 3 response; the canonical thoughts → text → calls order
  appears sufficient.
- Easy to migrate to later — Option A's three fields can be
  derived from Option B's array — so the choice is reversible.

### 7.2 Fix only the function-call signature

Add `ToolCall.ThoughtSignature` and ignore text / thought
signatures. This is the *minimum* fix that addresses the
reported error string.

Rejected because:

- The reported error is the first encounter; the next Gemini 3
  conversation could surface a text- or thought-signature failure
  that would require another follow-up fix.
- The data-model expansion to cover all three locations is
  trivial — three fields rather than one.
- We pay zero performance cost for capturing signatures we never
  end up needing.

### 7.3 Stay on Gemini 2.5

Pin the default Vertex model to Gemini 2.5 and document Gemini 3
as "unsupported for tool use."

Rejected. The user explicitly migrated to Gemini 3 to take
advantage of its capabilities; refusing the upgrade defeats the
point. Gemini 2.5 is also approaching end-of-life (`feedback_gemini3_migration`
notes the Gemini 2 → 3 migration deadline as 2026-10-16+).

### 7.4 Strip tool calls on retry

When a 400 with "thought_signature" appears, fall back to a retry
without the assistant turn's tool calls. Crude — loses the model's
prior tool plan and may loop indefinitely. Rejected as a hack.

---

## 8. Compatibility & rollout

- LLM-observable: none. Signatures are opaque server-side state;
  the user-visible chat content is unchanged.
- API: `ToolCall` and `Message` gain new optional fields. Field
  type `[]byte` serialises as base64-encoded JSON string under
  `omitempty`. Existing serialised sessions and exports parse
  cleanly (fields default to nil).
- Bindings / frontend: no changes.
- Local backend: no changes; signature fields are ignored.
- Session export / import: signatures travel with the bundle.
  Cross-machine import works for any user with Vertex access.

### Rollout

Per the same pattern as ADR-0008 (mcp-abort), this work splits
into bisect-friendly commits:

1. `feat(llm): preserve Gemini 3 thought signatures on ToolCall and Message`
   — backend.go field additions + unit tests for the JSON
   round-trip.
2. `feat(llm/vertex): capture and replay Gemini 3 thought signatures`
   — vertex.go parseResponse capture, buildContents replay, and
   the vertex_test.go assertions in §5.1.
3. `docs: describe Gemini 3 thought-signature preservation`
   — CHANGELOG v0.6.2 entry + AGENTS / README pointers + INDEX
   ADR row.
4. `chore: release v0.6.2`.

A maintainer-side manual smoke is required before tagging: switch
the Vertex model to a Gemini 3 family ID, prompt a multi-tool
question (the `weather` flow that originally failed), confirm the
agent loop completes without a 400.
