# ADR-0019: LLM-driven memory tool + extraction toggle

- Status: Implemented in v0.13.2 (2026-05-20)
- Deciders: magi
- Related: ADR-0015 (deferred extraction), ADR-0017 (prompt prefix stability), ADR-0018 (guard nonce stability)

## 1. Context

Sessions on local LM Studio (gemma-4-26b-a4b) showed ~28 s per turn on
~15 K-token prompts even after ADR-0017 and ADR-0018 made the prompt
byte-stable. Direct curl against LM Studio confirmed the cache **does**
fire on identical bodies (26.9 s cold → 0.44 s warm, 98 % speedup).

The smoking gun came from `SHELL_AGENT_DEBUG_LLM=1` debug dumps and a
follow-up curl experiment:

| Step                                     | Body              | Wall-clock |
|------------------------------------------|-------------------|-----------:|
| A — production body                      | req1.json (15 K)  |     0.70 s |
| B — small unrelated request (extraction-mimic) | extract.json (~150 tok) | 0.81 s |
| C — production body again                | req1.json (15 K)  |    25.90 s |

Step C re-warmed cold despite step A having primed the cache, because
step B overwrote llama.cpp's single prefix-cache slot. In real sessions
shell-agent-v2 runs an auto-extraction LLM call after every turn (added
in ADR-0015 to defer extraction off the response path) — this call
plays the role of step B and evicts the conversation prefix between
every turn. ADR-0017 and ADR-0018 had moved volatility out of the
prompt prefix, but the cache they were protecting was being wiped by
the very next LLM call. Production turn-2 latency therefore matched a
cold run.

Cloud backends (Vertex AI) do not exhibit this: their KV-cache is
shared per project/model and isolated per request stream, so an
auxiliary extraction call against the same model does not evict the
main session's cache. The problem is local-specific.

## 2. Decision

Two coupled changes:

**(a) Per-backend auto-extraction toggle.** Add `AutoExtractEnabled`
to `LocalConfig` and `VertexAIConfig`. Default: `local=false`,
`vertex=true`. When disabled, `postResponseTasks` skips the
`extractMemories` call entirely (no goroutine, no `agent:extraction:*`
events). The deferred-extraction send-queue (ADR-0015) becomes a
no-op trivially because there is no in-flight extraction to wait on.

**(b) Built-in `remember-fact` tool.** A new builtin in
`builtinDescriptors()` that lets the assistant explicitly route facts
to the memory stores during a turn, replacing what the auto-extractor
used to do. Schema mirrors the existing extraction taxonomy so the
routing logic in `session/global memory` can be reused unchanged:

```json
{
  "name": "remember-fact",
  "description": "Save a fact about the user to memory ...",
  "parameters": {
    "fact":     { "type": "string", "description": "Concise statement" },
    "category": { "type": "string", "enum": ["preference","decision","fact","context"] }
  }
}
```

Routing: `preference` / `decision` → `GlobalMemory`, `fact` / `context`
→ `SessionMemory` (identical to `extractMemories`). Same
`IsSelfReferential` filter is applied as a defence against the THINK-
leakage class of fact.

**(c) Exclusivity.** When `AutoExtractEnabled = true`, the
`remember-fact` builtin is **omitted** from the tool list presented to
the LLM. The two paths address the same need (route facts into memory)
and offering both creates duplication risk and prompt clutter. The
auto-extractor's prompt is also tuned for higher recall while the tool
is tuned for assistant judgement; mixing them muddies both.

The exclusion is at descriptor-build time (`builtinDescriptors()`),
not a runtime gate. The user can still disable the builtin manually
via the existing per-tool toggle in Settings.

## 3. Design details

### 3.1 Config schema additions

```go
type LocalConfig struct {
    // ... existing fields ...

    // AutoExtractEnabled gates the after-turn memory-extraction LLM
    // call (ADR-0015). Default false for local backends because the
    // extraction call evicts llama.cpp's single prefix-KV-cache slot
    // and forces the next turn into a cold re-encode of the whole
    // history (see ADR-0019 §1). Users who prefer recall over latency
    // can opt in.
    AutoExtractEnabled bool `json:"auto_extract_enabled"`
}

type VertexAIConfig struct {
    // ... existing fields ...

    // AutoExtractEnabled — see LocalConfig. Default true for Vertex
    // because its server-side KV cache is per-request-stream and not
    // evicted by auxiliary calls.
    AutoExtractEnabled bool `json:"auto_extract_enabled"`
}
```

Defaults are applied in `config.Load` when the field is absent from
the on-disk JSON (zero value of `bool` is `false`, so the local
default is "free"; the Vertex section needs an explicit `if !present
{ ... = true }` fix-up in the loader).

### 3.2 Agent integration

In `Agent.postResponseTasks` (agent.go:2123):

```go
extractFn := a.extractMemoriesOverride
if extractFn == nil {
    if !a.autoExtractEnabled() {
        return // skip entirely; no goroutine, no events
    }
    extractFn = a.extractMemories
}
```

`autoExtractEnabled()` resolves the flag from the **active profile's**
backend section (ADR-0016) so per-profile overrides work correctly. A
session that switched from local to Vertex mid-conversation flips the
behaviour at switch time, not at session start.

### 3.3 Builtin tool implementation

Add `remember-fact` to `builtinDescriptors()` only when
`!autoExtractEnabled()`. Handler:

1. Parse `fact` (required, non-empty after trim) and `category`
   (required, must be in `ValidExtractionCategories`).
2. Apply `IsSelfReferential` filter — reject with a tool error if
   matched, so the LLM gets feedback and can rephrase if it had a
   legitimate user fact mis-classified.
3. Route to `GlobalMemory` (preference/decision) or `SessionMemory`
   (fact/context) using the same writers `extractMemories` uses.
4. Return a one-line success message including the stored ID so the
   LLM can refer to it in subsequent turns if it wants to update.

System-prompt guidance (appended to the existing builtin doc block):

```
When the user states a stable preference, makes an explicit decision,
or shares a fact about themselves that will matter in later sessions,
use the remember-fact tool to persist it. Do NOT use it for transient
context, intermediate reasoning, or anything that will be obvious from
the conversation history. Aim for at most a few calls per session.
```

### 3.4 Settings UI

Profile editor gets one extra row per backend section:

> **Auto-extract memories after each turn**  (toggle)
> *Local default: off. When enabled, the assistant cannot use the
> `remember-fact` tool (the auto-extractor handles it instead).*

The exclusivity copy is important so users understand why toggling
this also hides a tool.

## 4. Alternatives considered

- **Run extraction on a second LM Studio instance.** Eliminates cache
  eviction but doubles model RAM and complicates setup. Rejected as
  default; users with the hardware budget can already do this by
  pointing the secondary profile at a different endpoint.
- **Run extraction on a small cloud model (Gemini Flash) regardless
  of main backend.** Adds external dependency and breaks the
  privacy-by-default story for local users. Rejected.
- **Run extraction at session-close instead of per-turn.** Preserves
  cache during the session but loses cross-session facts whenever the
  session is killed without a clean close (Wails app force-quit,
  crash). Considered for a future ADR but not in scope here.
- **Keep auto-extraction on for local with a separate-prompt
  workaround** that re-primes the conversation cache after each
  extraction. Fragile and adds ~5 K tokens of throwaway processing per
  turn. Rejected.

## 5. Consequences

**Positive:**

- Local turn-2+ latency drops from ~28 s to ~hundreds of ms on warm
  prompts (estimate: the same 98 % speedup the curl experiment showed,
  per ADR-0017 §3 mechanism).
- LLM-driven memory is auditable from the conversation transcript —
  every fact ever stored has a visible tool call, in contrast to the
  background auto-extractor whose decisions appear only in the memory
  store.
- Two extraction paths with explicit exclusivity is simpler than one
  path with mode flags.

**Negative:**

- LLM may forget to call `remember-fact` for facts the auto-extractor
  would have caught. Mitigated by system-prompt guidance + the
  auto-extract fallback for users who care more about recall than
  latency (toggle on).
- LLM may over-call `remember-fact` (gemma-class models are eager
  tool-callers). No rate limit in v0.13.2 — the storage cost is low
  and over-saved facts are easier to clean up via the UI than under-
  saved ones are to recover. Revisit only if production logs show
  pathological call counts.
- Behaviour now differs between backends in a user-visible way. The
  Settings copy and CHANGELOG must call this out.

## 6. Implementation plan

1. Config schema + loader defaults (Commit 1)
2. Gate `extractMemories` call in `postResponseTasks` (Commit 2)
3. Add `remember-fact` builtin + handler + descriptor gating
   (Commit 3)
4. Settings UI toggle, both profiles (Commit 4)
5. Tests: extraction-off path, descriptor exclusivity, tool handler
   routing + self-ref filter (Commit 5)
6. CHANGELOG v0.13.2, ADR status → Implemented, JA mirror,
   docs-mirror-check (Commit 6)

Verification: replay the §1 curl experiment against the GUI with
local profile; confirm turn-2 latency is sub-second on the same 15 K
prompt. Vertex profile regression check: 3-turn conversation with
factual content, confirm auto-extracted records still appear.
