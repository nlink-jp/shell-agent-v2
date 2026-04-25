# Agent Data Flow & State Control — Design Document

> Date: 2026-04-25
> Status: Draft — addresses issues found during v2 implementation testing
> Scope: Agent loop, session records, memory compaction, object repository

## 1. Problem Summary

v2 implementation has multiple data flow and state control issues discovered
during testing:

1. **Tool call loop** — LLM re-calls tools after execution despite tools
   being removed from the request (gemma text-based tool calls bypass API)
2. **Session records pollution** — empty assistant messages, tool results
   accumulate in records, pollute multi-turn LLM context
3. **No memory compaction** — CompactIfNeeded exists but is never called;
   hot messages grow unbounded until context overflow
4. **No session persistence** — session.Save() never called during agent loop;
   crash = data loss
5. **Report not persisted** — reports delivered via event but not stored in
   session records; lost on reload
6. **Image handling** — images stored as raw data URLs in records (bloat);
   no object ID-based referencing; LLM cannot reference session images
7. **Frontend/backend filtering mismatch** — tool results filtered in
   frontend but not backend; inconsistent

Root cause: incremental patches without holistic design.

## 2. Agent Loop State Machine

### 2.1 LM Studio Tool Calling Specification

Per LM Studio docs, the recommended flow is:

```
1. Call LLM WITH tools → get response
2. If tool_calls present:
   a. Execute tools
   b. Add assistant tool_call message + tool results to messages
   c. Call LLM WITHOUT tools → get final text response
3. If no tool_calls: return text response
```

Key rule: **After tool execution, the next LLM call MUST be WITHOUT tools.**
This forces the LLM to generate text instead of calling tools again.

### 2.2 v2 Agent Loop Design

```
SendWithImages(ctx, message, imageURLs)
  │
  ├── State: Idle → Busy
  ├── Handle /commands (return immediately)
  │
  └── agentLoop(ctx, message, imageURLs)
        │
        ├── Save images to objstore → get IDs
        ├── Add user record to session (with image IDs, not data URLs)
        ├── Auto-save session
        │
        └── LOOP (max 10 rounds):
              │
              ├── Build tools list:
              │   ├── If toolsExecutedLastRound: tools = nil
              │   └── Else: tools = buildToolDefs()
              │
              ├── Build messages from session records
              │   ├── System prompt + temporal context + pinned + findings
              │   ├── Warm/Cold summaries first
              │   ├── Hot records (skip empty assistant records)
              │   ├── Guard-wrap user and tool content
              │   └── Latest image: full data URL; older: text reference
              │
              ├── Call LLM (ChatStream with tools or nil)
              │
              ├── Clean response:
              │   ├── strip.ThinkTags()
              │   └── stripGemmaToolCallTags() (always, not just no-tools rounds)
              │
              ├── IF no tool_calls AND content non-empty:
              │   ├── Store assistant record in session
              │   ├── Auto-save session
              │   ├── Background: generateTitle, extractPinned, compactMemory
              │   └── RETURN content
              │
              ├── IF no tool_calls AND content empty:
              │   └── RETURN "" (end loop, don't re-enable tools)
              │
              └── IF tool_calls present:
                    ├── Store assistant record ONLY IF content non-empty
                    ├── Execute each tool call:
                    │   ├── MITL check for write/execute
                    │   ├── Execute tool
                    │   ├── Store tool result record
                    │   └── If tool produces artifact → save to objstore
                    ├── Auto-save session
                    └── Set toolsExecutedLastRound = true
```

### 2.3 Critical Rules

1. **Never re-enable tools after empty response.** If LLM returns empty
   after a no-tools round, the loop ends. The earlier re-enable logic
   caused infinite create-report loops.

2. **Always strip gemma text tool calls.** Applied every round, not just
   no-tools rounds. Prevents gemma format from being treated as content.

3. **Always record assistant tool call requests.** When the LLM responds
   with tool_calls, the assistant message MUST be recorded even if content
   is empty. Use `[Calling: tool_name]` as synthetic content. This is
   required for valid conversation history — without it, the LLM doesn't
   know tools were already called and tries to call them again.
   Only skip truly empty assistant messages (no tool calls AND no content).

4. **Auto-save after every mutation.** User message, assistant message,
   tool result — each save to disk immediately.

## 3. Session Records Data Model

### 3.1 Record Structure

```go
type Record struct {
    Timestamp    time.Time   `json:"timestamp"`
    Role         string      `json:"role"`      // user|assistant|tool|report|summary
    Content      string      `json:"content"`
    Tier         Tier        `json:"tier"`       // hot|warm|cold
    ToolCallID   string      `json:"tool_call_id,omitempty"`
    ToolName     string      `json:"tool_name,omitempty"`
    ObjectIDs    []string    `json:"object_ids,omitempty"`  // references to objstore
    SummaryRange *TimeRange  `json:"summary_range,omitempty"`
    InTokens     int         `json:"in_tokens,omitempty"`
    OutTokens    int         `json:"out_tokens,omitempty"`
}
```

### 3.2 Roles

| Role | Stored by | Sent to LLM | Shown in UI | Notes |
|------|-----------|-------------|-------------|-------|
| `user` | agentLoop | Yes (guarded) | Yes | User messages |
| `assistant` | agentLoop | Yes | Yes | LLM responses (non-empty only) |
| `tool` | agentLoop | Yes (guarded) | No | Tool results, formatted as `[Tool: name]\nOutput:\n...` |
| `report` | toolCreateReport | Yes | Yes (special) | Report content, stored in session |
| `summary` | CompactIfNeeded | Yes | No | Warm/Cold memory summaries |

### 3.3 What NOT to Store

- Empty assistant messages (content == "" after cleaning)
- Intermediate streaming content
- Event-only data (frontend handles display separately)

### 3.4 Tool Result Format

```
[Tool: resolve-date]
Output:
2026-04-24 (Thursday)
```

Not the raw tool output — prefixed with tool name for LLM context.

## 4. Memory Compaction

### 4.1 When to Compact

Called as post-response background task, after every non-empty response:

```
agentLoop returns
  → go generateTitleIfNeeded(ctx)
  → go compactMemoryIfNeeded(ctx)    // NEW: must be called
  → go extractPinnedMemories(ctx)
  → session.Save()                   // synchronous final save
```

### 4.2 Compaction Flow

```
compactMemoryIfNeeded(ctx):
  1. Calculate total hot tokens: sum(EstimateTokens(r.Content) for hot records)
  2. If hotTokens <= cfg.Memory.HotTokenLimit: return (no compaction needed)
  3. Target: reduce to 75% of limit
  4. Select oldest hot records (keep at least latest 2: user + assistant)
  5. Build conversation text from selected records
  6. LLM call: "Summarize this conversation segment" (no tools)
  7. Create warm summary record:
     - Role: "summary"
     - Tier: TierWarm
     - SummaryRange: {From: first.Timestamp, To: last.Timestamp}
  8. Replace selected hot records with warm summary
  9. Save session
  10. Emit "memory:compacted" event to frontend
```

### 4.3 Token Estimation

```go
func EstimateTokens(s string) int {
    charBased := len(s) / 2   // CJK: ~2 chars per token
    wordBased := len(s) / 4   // English: ~4 chars per token
    return max(charBased, wordBased)
}
```

### 4.4 BuildMessages with Tiers

Message construction order:

```
1. System prompt (always first)
2. Warm/Cold summary records (oldest first)
3. Hot records (chronological order)
   - Skip role="summary" (already included above)
   - Skip empty content
   - Guard-wrap user and tool content
```

## 5. Object Repository

### 5.1 Unified Object Model

All artifacts stored in objstore with typed IDs:

```go
type ObjectMeta struct {
    ID       string `json:"id"`        // 12-char hex
    Type     string `json:"type"`      // "image", "report", "result"
    MimeType string `json:"mime_type"` // "image/png", "text/markdown", etc.
    OrigName string `json:"orig_name"`
    Size     int64  `json:"size"`
}
```

### 5.2 Image Flow

```
User drops image
  → Frontend reads as data URL
  → SendWithImages(text, [dataURL])
    → objstore.SaveDataURL(dataURL) → ID "abc123"
    → Record.ObjectIDs = ["abc123"]
    → LLM context: "[Image attached, ID: abc123]" + data URL (latest only)
    → Older images: "[Past image ID: abc123 — use get-object tool to view]"
```

### 5.3 Report Flow

```
LLM calls create-report(title, content)
  → Save report markdown to objstore → ID "def456"
  → Store Record: role="report", ObjectIDs=["def456"]
  → Emit report:created event with content (for immediate display)
  → Session reload: load report content from objstore by ID
```

### 5.4 LLM Tools for Objects

```
list-objects    — List all objects in current session (type, ID, name)
get-object      — Retrieve object content by ID (text or data URL)
```

These allow the LLM to:
- Reference earlier images by ID in reports: `![desc](object:abc123)`
- Retrieve analysis results from earlier in the session
- Build reports that reference multiple session artifacts

### 5.5 Frontend Object Resolution

ReactMarkdown `img` component:
- `src` starts with `object:` → resolve via `GetImageDataURL(id)`
- `src` starts with `data:` → use directly
- Other → render as-is (external URL)

## 6. Report Generation

### 6.1 Flow

```
User: "Create a report"
  → LLM calls create-report tool
    → Save content to objstore → report ID
    → Store as session record (role="report", ObjectIDs=[reportID])
    → Emit report:created event to frontend
    → Return "Report created" (short, prevents loop)
  → LLM gets short confirmation
  → tools=nil next round → LLM generates text response or empty → end
```

### 6.2 Image References in Reports

LLM writes: `![description](object:abc123)`

- `abc123` = objstore ID of an image from the session
- Frontend resolves `object:abc123` → data URL via GetImageDataURL
- On save-to-file: resolve all `object:` refs to inline base64

### 6.3 Report Persistence

Reports are stored in session records (role="report") AND objstore.
Session reload renders them from records. Content loaded from objstore
if not inline.

## 7. Frontend Display Rules

| Record Role | Display | Style | Notes |
|------------|---------|-------|-------|
| user | Yes | Right-aligned bubble | With image thumbnails |
| assistant | Yes | Left-aligned bubble | Markdown rendered |
| tool | Hidden | — | Filtered in frontend |
| report | Yes | Full-width, special header | Title + save button |
| summary | Hidden | — | Filtered in backend |

### 7.1 Tool Execution Indicators

During tool execution (Busy state):
- Show spinner with tool name
- Show formatted arguments (if safe)
- On completion: hide indicator, continue streaming

### 7.2 Events

| Event | Direction | Purpose |
|-------|-----------|---------|
| agent:stream | Backend → FE | Streaming tokens |
| session:title | Backend → FE | Auto-generated title |
| report:created | Backend → FE | Report content for display |
| pinned:updated | Backend → FE | Pinned memory changed |
| memory:compacted | Backend → FE | Memory compaction occurred |
| mitl:request | Backend → FE | Tool approval needed |

## 8. Implementation Checklist

### Phase 1: Critical Fixes
- [ ] Call CompactIfNeeded after every response
- [ ] Connect Summarizer to LLM backend
- [ ] Auto-save session after each record mutation
- [ ] Store reports in session records (role="report")
- [ ] Remove re-enable tools logic (empty → end loop)
- [ ] Strip gemma tags every round

### Phase 2: Object Repository
- [ ] Migrate images from data URLs to objstore IDs in records
- [ ] list-objects / get-object tools
- [ ] Frontend object: URL resolution
- [ ] Report image references via object IDs

### Phase 3: Cleanup
- [ ] Consistent role filtering (backend only, not frontend)
- [ ] Tool execution progress events
- [ ] Context cancellation propagation to tool LLM calls
- [ ] Token tracking in records
