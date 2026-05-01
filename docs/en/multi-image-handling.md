# Multi-Image Handling — Design Document

> Date: 2026-05-01
> Status: Draft — pending implementation
> Scope: how the agent layer and LLM backends pack
> multi-image user messages so each image is reliably
> correlated with its persistent object ID.

## 1. Problem

A user sends a chat message with N attached images. Today the
agent stitches them into a single multimodal message:

```
[text("question"),
 text("=== BEGIN IMAGE 1 (id: a) ==="), image_a, text("=== END IMAGE 1 ==="),
 text("=== BEGIN IMAGE 2 (id: b) ==="), image_b, text("=== END IMAGE 2 ==="),
 …]
```

Symptom on `local` backend (LM Studio + llama.cpp + Gemma 3
multimodal): with 3 images, the descriptions for image 1 and
image 3 are consistently swapped in the model's report —
section 1 of the report has image-3's content paired with
image-1's correct object ID, and vice versa. Section 2 is
fine. The same prompt sent to Vertex AI (gemini-2.5-flash)
works correctly.

Diagnostics already done:
- Frontend `FileReader` race fixed (Promise.all preserves
  attach order).
- ObjectIDs are propagated through `chat.go` /
  `contextbuild/builder.go` so the LLM-bound `Message` has
  matching `ImageURLs[i]` ↔ `ObjectIDs[i]`.
- Per-image `BEGIN/END` text anchors added — Vertex now
  produces correct image↔ID correlation; the local backend
  still swaps.

Conclusion: this is a **multi-image inference bug in
llama.cpp's mmproj path**, not an ordering bug in our code.
Gemma 3 itself is trained on multi-image-per-turn (the
technical report evaluates up to ~8); Vertex / vLLM / HF
Transformers handle it correctly. llama.cpp's mmproj cache
reuses slots across images in one prompt and the positional
binding between `<start_of_image>` markers and embedding
tensors gets reordered (issues #12344-ish, fixed
intermittently late 2025; LM Studio builds vary).

The `BEGIN/END` text wrappers we added probably make it
worse: they push tokens between the ID label and the
following image, widening the window the buggy slot reuse
can mis-attribute.

## 2. Goals & Non-goals

### Goals

1. On `local` backend with ≥2 images attached to a single
   user record, send each image in its own user turn so
   llama.cpp's per-prompt mmproj slot bug can't reorder
   them. Each per-image turn carries the matching object ID
   as a short prefix, in Google's recommended Gemma format.
2. Drop the verbose `=== BEGIN/END IMAGE N ===` wrappers in
   favour of a single short prefix line directly before the
   image — both backends benefit, and we stop fighting
   llama.cpp's tokenization.
3. Vertex behaviour stays semantically identical: a single
   user turn carrying interleaved text-prefix + image parts
   in attach order. (The wrapper change is the only Vertex
   diff.)
4. Persisted `Record.ImageURLs` / `Record.ObjectIDs` stay
   parallel arrays — the split happens at backend-convert
   time, not at session-record time. UI rendering of past
   messages is unaffected.

### Non-goals

- **No frontend changes.** `pendingImages` ordering and the
  `Bindings.SendWithImages` arg shape stay as today.
- **No new tools or system-prompt rewrite beyond removing
  the `BEGIN/END` paragraph.**
- **No cross-backend feature flags.** The split is a
  permanent local-backend behaviour; we don't expose a
  toggle.
- **No fix for ≥9 images** — Gemma 3's multi-image training
  caps around 8; we won't artificially limit but we also
  won't claim correctness past that.

## 3. Detailed design

### 3.1 New per-backend image-packing rule

The split happens inside each backend's request builder
(`internal/llm/local.go`, `internal/llm/vertex.go`),
because the data shape differs:

| Backend | When N images in one Message | Output |
|---|---|---|
| `local` (OpenAI Vision) | always | N+1 `requestMessage`s: N user turns each carrying `[text(prefix), image_url]`, then a final user turn carrying the original `Content` text |
| `vertex_ai` | always | One `Content` block: `[text(Content), text(prefix), image, text(prefix), image, …]` (current pattern, just simpler prefix) |

For local with N=1 the output is still a single user turn
identical to today's single-image path — no behaviour
change for the common case.

### 3.2 Prefix format

Replace `=== BEGIN IMAGE N (object ID: x) ===` … `=== END
IMAGE N (object ID: x) ===` with a single line:

```
Image (object ID: <id>):
```

Rationale: matches the pattern Google's docs recommend for
Gemma multimodal, keeps the ID adjacent to the image with
no intervening tokens, and is short enough that the
text-encoder doesn't overweight it.

### 3.3 Local backend — split into multiple user turns

Pseudo-code for `local.buildRequest`:

```go
case (multimodal user message, len(ImageURLs) >= 1):
    for i, imgURL := range m.ImageURLs {
        req.Messages = append(req.Messages, requestMessage{
            Role: "user",
            Content: []contentPart{
                {Type: "text", Text: fmt.Sprintf("Image (object ID: %s):", m.ObjectIDs[i])},
                {Type: "image_url", ImageURL: &imageURL{URL: imgURL}},
            },
        })
    }
    if m.Content != "" {
        req.Messages = append(req.Messages, requestMessage{
            Role:    "user",
            Content: m.Content,
        })
    }
```

Edge cases:
- `len(ObjectIDs) < len(ImageURLs)`: fall back to
  positional `Image %d:` for the missing tail (legacy
  records).
- `m.Content == ""`: skip the trailing question turn (some
  LLM-built messages carry only images).
- N=1: still produces 1 image-turn + 1 question-turn; the
  cost is one extra empty-content turn, which gemma
  handles fine. Acceptable simplicity / determinism trade-
  off.

### 3.4 Vertex backend — single turn, simpler prefix

Pseudo-code for `vertex.convertMessages`:

```go
case (multimodal user message):
    parts := []*genai.Part{genai.NewPartFromText(m.Content)}
    for i, dataURL := range m.ImageURLs {
        if i < len(m.ObjectIDs) {
            parts = append(parts, genai.NewPartFromText(
                fmt.Sprintf("Image (object ID: %s):", m.ObjectIDs[i])))
        }
        if p := dataURLToGenaiPart(dataURL); p != nil {
            parts = append(parts, p)
        }
    }
    contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
```

Vertex doesn't suffer the llama.cpp slot bug, so packing
all images into one `Content` is fine. We only simplify
the prefix.

### 3.5 System-prompt update

In `agent.go`'s `defaultSystemPrompt`, replace the
`BEGIN/END` paragraph with:

> When the user shares images, each image in the
> conversation is preceded by a short text line of the
> form `Image (object ID: xxxxxxxxxxxx):`. The ID
> immediately before an image is THAT image's persistent
> object ID — describe each image based ONLY on the
> content directly following its ID line, and reference
> images in reports using `![alt](object:ID)` with that
> exact ID. Do not call `list-objects` to identify
> currently attached images.

### 3.6 Touched files

| File | Change |
|---|---|
| `internal/llm/local.go` | split multi-image into per-image user turns; new prefix |
| `internal/llm/vertex.go` | drop BEGIN/END, single-line prefix |
| `internal/agent/agent.go` | system-prompt paragraph rewrite |
| `internal/llm/backend_test.go` | rewrite `TestLocalBuildRequest_ImageAnchors` for the split shape; add a multi-image case asserting N+1 messages |

No changes to `chat.go`, `contextbuild/builder.go`,
frontend, bindings, or session record format.

## 4. Test plan

### Unit
- `TestLocalBuildRequest_SingleImage`: 1 image → 2
  `requestMessage`s (image + question).
- `TestLocalBuildRequest_MultipleImages`: 3 images → 4
  `requestMessage`s (image, image, image, question), each
  image turn has exactly `[text(prefix-with-id), image_url]`,
  question turn carries original `m.Content` only.
- `TestLocalBuildRequest_NoObjectIDs`: legacy record with
  ImageURLs but empty ObjectIDs → fall back to `Image N:`
  positional prefix; still split into per-image turns.
- `TestVertexConvertMessages_MultiImage`: single Content
  block, parts ordered `[text(content), prefix1, image1,
  prefix2, image2, …]`, no BEGIN/END markers.

### Manual
- Local Gemma session with 3 distinct images, ask for a
  per-image description. Confirm image 1 / 3 swap is gone.
  Repeat with 5 images.
- Vertex session with 3 images, confirm behaviour is
  unchanged (still correct).

## 5. Risk & rollout

| Risk | Mitigation |
|---|---|
| Splitting into N user turns confuses the model on cross-image tasks ("compare image 2 with image 3") | The final question turn explicitly references images by their object IDs (the prefix lines train the model on this convention). Gemma 3 is trained on multi-turn user content, so consecutive user turns are not exotic. |
| Increased token count for the local request (one extra prefix + role envelope per image) | ~30 tokens per image; negligible vs the image's 256 SigLIP tokens. |
| Older sessions reloaded after the change have records without `ObjectIDs` populated alongside `ImageURLs` | The "no ObjectIDs" branch keeps them working with a positional `Image N:` prefix. |
| Some llama.cpp builds don't accept consecutive `user` messages | Verify against the LM Studio version we ship against; the OpenAI-compat shim does accept them per spec. If a build rejects, fall back to a single user turn with all parts (today's behaviour) — the bug we're working around is build-dependent anyway. |

Single commit: `feat(llm): per-image user turns for local
backend, simpler image-ID prefix`. No phasing — the
backend is uncoupled from frontend / session storage, so
either it works on the first session after deploy or we
revert.

## 6. Out of scope

- Cross-image reasoning quality on llama.cpp builds older
  than the multi-image fixes — users who want strong
  multi-image analysis should use Vertex.
- Detecting and warning when the local model isn't
  multimodal (already handled at LLM-call time by the
  model returning text-only acknowledgements).
- Image down-sampling / resolution capping (separate
  TODO; affects token cost more than correctness).
