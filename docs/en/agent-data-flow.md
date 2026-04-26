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
              │   ├── Preserve application-level roles (NO mapping here)
              │   └── Latest image: full data URL; older: text reference
              │
              ├── Call LLM: Backend.Chat() (non-streaming for all rounds)
              │   └── Backend handles role mapping internally
              │       (see docs/en/llm-abstraction.md)
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

### 3.3 Context Budget Control

`BuildMessagesWithBudget` provides two mechanisms to keep the LLM
context clean and within operational bounds:

1. **[Calling:] exclusion** (primary) — prevents pattern contamination
2. **Token budget** (optional) — caps context size for resource management

#### Root Cause: [Calling:] Pattern Contamination

Integration testing with gemma-4-26b revealed that tool calling failure
was NOT caused by context length (gemma-4 supports 256K tokens). The
actual cause: when `[Calling: tool_name]` synthetic assistant messages
are included in the LLM context, the model mimics the pattern as text
output instead of making real API tool calls.

**Test results (Before/After):**

| Condition | Last successful tool call | Records at failure |
|-----------|--------------------------|-------------------|
| [Calling:] in context (NoBudget) | Turn 3–5 (non-deterministic) | 10–16 records |
| [Calling:] excluded (WithBudget) | Turn 13+ (all successful) | 52+ records |

The failure is probabilistic — same conditions can succeed or fail
across runs, but `[Calling:]` contamination significantly increases
failure probability.

#### Heavy Analysis Scenario Observations

Tested with: CSV load (30 rows) → queries → analyze-data → JSONL load
→ queries → analyze second table → cross-table query.

Key findings:
- `analyze-data` results inflate context by ~1,800 tokens per call
- Tool calling can degrade at ~3,000 estimated tokens with 14 tool defs
- Synchronous compaction (HotTokenLimit=4096) auto-recovers by reducing
  records from 22 to 11 with warm summary
- After compaction, tool calling resumes normally

#### Message Selection Algorithm

```
1. Build system prompt (existing logic)
2. Collect warm summary records, truncate to MaxWarmTokens
3. Collect hot records in REVERSE chronological order:
   - Skip [Calling: ...] messages (prevents pattern contamination)
   - Truncate tool results to MaxToolResultTokens
   - Stop when token budget exhausted (newest messages preserved)
4. Reverse back to chronological order
5. Assemble: system + warm + hot
```

Applied to ALL backends, not just local.

#### Configuration

```go
type ContextBudgetConfig struct {
    MaxContextTokens    int // default: 0 (unlimited)
    MaxWarmTokens       int // default: 1024
    MaxToolResultTokens int // default: 2048
}
```

#### Recommended Parameters

| Parameter | Default | Rationale |
|-----------|---------|-----------|
| `MaxContextTokens` | 0 (unlimited) | gemma-4 supports 256K; [Calling:] exclusion is the primary fix |
| `MaxWarmTokens` | 1024 | One summary paragraph |
| `MaxToolResultTokens` | 2048 | Enough for query results; analyze-data reports truncated |
| `HotTokenLimit` (compaction) | 4096 | Triggers compaction before context grows too large for tool calling |

For resource-constrained environments or smaller models, set
`MaxContextTokens` to a conservative value (e.g., 8192).

#### Synchronous Compaction

Compaction runs synchronously at agentLoop start and between tool rounds
(round > 0), ensuring the context is compacted BEFORE BuildMessages is
called. The async post-response compaction remains as a safety net.

Post-response tasks (title generation, compaction, pinned extraction)
are synchronized via WaitGroup to prevent race conditions with the
next Send() call.

### 3.4 What NOT to Store

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

## 6. Tool Execution Confirmation (MITL)

### 6.1 Confirmation Categories

| Tool | Confirmation | Rationale |
|------|-------------|-----------|
| query-sql | **SQL Preview** | Show generated SQL before execution |
| analyze-data | **Plan Approval** | Show analysis perspective + target table |
| create-report | None | Read-only output |
| load-data | None | File path already specified by user |
| Shell tools (write/execute) | **MITL Dialog** | Existing approval for dangerous ops |
| Shell tools (read) | None | Safe operations |
| Other builtin tools | None | Low-risk read operations |

### 6.2 SQL Preview Flow (query-sql)

```
LLM calls query-sql with {sql: "SELECT ..."}
  ↓
Agent emits MITL request: type=sql_preview
  - Shows: SQL query text
  - Shows: Target tables
  ↓
User chooses:
  - Approve → Execute SQL, return results to LLM
  - Reject → Return "User rejected" to LLM, LLM re-responds
  - Reject + Feedback → Return user's feedback to LLM as context
    ("User rejected this SQL. Feedback: ...")
    LLM generates new SQL in next round
```

### 6.3 Analysis Plan Approval (analyze-data)

Inspired by data-agent's Planning → Approval → Execution pattern.

```
LLM calls analyze-data with {prompt: "...", table: "..."}
  ↓
Agent emits MITL request: type=analysis_plan
  - Shows: Analysis perspective (prompt)
  - Shows: Target table name + row count
  - Shows: Estimated windows
  ↓
User chooses:
  - Approve → Execute sliding window analysis
  - Reject → Return "User rejected" to LLM
  - Reject + Feedback → Return feedback to LLM
    ("User wants to focus on X instead of Y")
    LLM generates new analyze-data call with revised perspective
```

### 6.4 MITL Response Model

```go
type MITLResponse struct {
    Approved bool   // true = proceed, false = reject
    Feedback string // non-empty only when rejected with reason
}
```

Frontend MITL dialog shows three actions:
- **Approve** button
- **Reject** button (no feedback)
- **Reject + text input** (feedback field + reject button)

When rejected with feedback, the tool result returned to LLM is:
```
User rejected this operation.
Feedback: <user's feedback text>
Please revise your approach based on the feedback.
```

This allows the LLM to adjust its SQL or analysis perspective in the
next round without requiring a new user message.

### 6.5 Shell Tool MITL (existing)

Unchanged from current implementation. Write/execute category shell
tools require approval. Read category shell tools execute directly.

## 7. Report Generation

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

| Event | Direction | Payload | Purpose |
|-------|-----------|---------|---------|
| `agent:stream` | Backend → FE | `{token, done}` | Streaming tokens (final response only) |
| `agent:activity` | Backend → FE | `{type, detail}` | Agent execution status |
| `session:title` | Backend → FE | `{session_id, title}` | Auto-generated title |
| `report:created` | Backend → FE | `{title, content}` | Report content for display |
| `pinned:updated` | Backend → FE | `nil` | Pinned memory changed |
| `mitl:request` | Backend → FE | `{tool_name, arguments, category}` | Tool approval needed |

#### agent:activity Types

| Type | Detail | UI Display |
|------|--------|-----------|
| `tool_start` | Tool name | "Executing: query-sql" (status bar) |
| `tool_end` | Tool name | Clear status bar |
| `thinking` | LLM explanation text | Transient note (NOT a chat message) |

The `thinking` type replaces the former `agent:explanation` event.
LLM explanation text (e.g. "I will calculate the total revenue...")
is shown as a transient indicator, NOT added to the chat message list.
The text is already stored in the session record for persistence.

## 8. Implementation Checklist

### Phase 1: Critical Fixes (completed)
- [x] Call CompactIfNeeded after every response
- [x] Connect Summarizer to LLM backend
- [x] Auto-save session after each record mutation
- [x] Store reports in session records (role="report")
- [x] Remove re-enable tools logic (empty → end loop)
- [x] Strip gemma tags every round

### Phase 2: Object Repository (completed)
- [x] Migrate images from data URLs to objstore IDs in records
- [x] list-objects / get-object tools
- [x] Frontend object: URL resolution
- [x] Report image references via object IDs

### Phase 3: Context Budget & Event Architecture (completed)
- [x] ContextBudgetConfig in config.go
- [x] BuildMessagesWithBudget in chat.go (token budget, tool result truncation, [Calling:] skip)
- [x] Synchronous compaction before BuildMessages in agentLoop
- [x] agent:activity event (consolidate agent:explanation + agent:progress)
- [x] Frontend activity state (transient display, not chat message)
- [x] postResponseTasks WaitGroup synchronization
- [x] LM Studio / Vertex AI integration tests
- [x] Root cause identified: [Calling:] pattern contamination

### Phase 4: Tool Execution Confirmation
- [ ] Extend MITLResponse with Feedback field (Approve / Reject / Reject+Feedback)
- [ ] SQL Preview MITL for query-sql (show SQL before execution)
- [ ] Analysis Plan MITL for analyze-data (show perspective + target)
- [ ] Frontend MITL dialog redesign (feedback text input on reject)
- [ ] Feedback-based tool result for LLM re-generation
- [ ] Shell tool MITL verification (existing code, untested)
- [ ] System prompt update for user-language responses
