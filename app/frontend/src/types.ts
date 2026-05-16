// Shared TypeScript types for the frontend. Anything used by
// more than one component lives here; component-local types
// stay in their respective .tsx files.
//
// This file MUST NOT import from any component file (App,
// Sidebar, dialogs, etc.) — it is the leaf of the type
// dependency graph.

export interface ChatMessage {
    role: 'user' | 'assistant' | 'system' | 'tool' | 'report' | 'tool-event' | 'summary';
    content: string;
    timestamp: string;
    imageUrls?: string[];
    // 'running' while a tool is in flight; the backend reports
    // 'success' or 'error' on completion. Tool-family-specific
    // classification was finalised in v0.1.13 (sandbox via
    // ExecResult, MCP via result.isError, etc.).
    status?: 'running' | 'success' | 'error';
    // Populated for `tool-event` rows so the chat pane can fetch
    // full args + result on click via GetToolCallDetails. Empty for
    // legacy rows that predate the field.
    toolCallId?: string;
    /** v0.5: markdown / text attachments on a user bubble. Each
     *  entry can carry the objstore id (set for restored
     *  messages — used by the click-to-preview handler via
     *  GetObjectText) and/or the live data URL (set when the
     *  message was just sent — content is locally decodable so
     *  preview doesn't need a backend round-trip). name is the
     *  filename to render in the bubble label. */
    documents?: Array<{id?: string; name: string; dataURL?: string}>;
}

export interface ToolCallDetails {
    tool_call_id: string;
    tool_name: string;
    arguments: string;
    result: string;
    status: string;
    call_timestamp: string;
    result_timestamp: string;
}

export interface SessionInfo {
    id: string;
    title: string;
    updated_at: string;
    /** v0.3.0: when true, the session opted out of Global Memory
     *  promotion. Sidebar shows a 🔒 indicator; chat pane shows
     *  a 🔒 banner; ★ Pin buttons are hidden. */
    private?: boolean;
}

export interface MessageData {
    role: string;
    content: string;
    timestamp: string;
    // Populated when the backend rebuilds tool-event rows from a
    // restored session. "success" / "error" — never "running",
    // because restored bubbles are always terminal.
    status?: 'success' | 'error';
    // Populated for restored 'user' rows that originally attached
    // images. Frontend converts these to `object:<id>` URLs and
    // routes them through ObjectImage so the restored chat shows
    // the same images the user originally attached.
    object_ids?: string[];
    // Populated for restored `tool-event` rows so the click-to-
    // inspect overlay can fetch full args + result.
    tool_call_id?: string;
    /** v0.5: restored markdown / text attachments. Each entry
     *  carries the objstore id and a display name (resolved
     *  server-side from objstore.OrigName so the bubble can
     *  render without an extra round-trip). */
    documents?: Array<{id: string; name: string}>;
}

export interface Finding {
    id: string;
    content: string;
    tags: string[];
    created_label: string;
    /** v0.2.0: "llm_promoted" (promote-finding tool) or
     *  "analyze_data" (sliding-window auto-emit). Legacy entries
     *  may have empty source. SourceManual is gone — the /finding
     *  slash command was removed. */
    source?: string;
    tool_originated?: boolean;
}

export interface ToolInfo {
    name: string;
    description: string;
    category: string;
    source: string;
    /** Backend-computed default for the MITL gate, ignoring any
     *  current MITLOverrides entry. The Settings UI uses this so
     *  the toggle's "default" state matches what the dispatcher
     *  will actually do. */
    mitl_default: boolean;
}

/** Cross-session memory entry (preference / decision categories
 *  only). Renamed from PinnedMemory in v0.2.0. */
export interface GlobalMemory {
    fact: string;
    native_fact: string;
    category: string;
    /** "user_turn" / "manual" / "promoted_from_*" → user-stated
     *  (high trust); "assistant_turn" or empty (legacy) → derived
     *  (lower trust; content traces back through the LLM and may
     *  be attacker-influenced). See docs/en/memory-model.md. */
    source?: string;
    session_id?: string;
    tool_originated?: boolean;
    /** RFC3339 timestamp when learned, or empty for legacy entries. */
    created_at?: string;
}

/** Per-session memory entry (fact / context categories only).
 *  Dies with the session unless promoted to GlobalMemory via the
 *  Pin to Global Memory action. */
export interface SessionMemory {
    fact: string;
    native_fact: string;
    category: string;
    /** "user_turn" → user-stated; "assistant_turn" → derived. */
    source?: string;
    tool_originated?: boolean;
    created_at?: string;
}

export interface LLMStatus {
    backend: string;
    hot_messages: number;
    warm_summaries: number;
    session_id: string;
    prompt_tokens: number;
    output_tokens: number;
}

export interface MCPProfile {
    name: string;
    binary: string;
    profile_path: string;
    enabled: boolean;
}

export interface MCPStatus {
    name: string;
    status: string;
    tool_count: number;
    error?: string;
}

export interface MITLRequest {
    tool_name: string;
    arguments: string;
    category: string;
}

export interface ExpandedReport {
    title: string;
    content: string;
}

export interface BackendBudget {
    max_context_tokens: number;
    max_warm_tokens: number;
    max_tool_result_tokens: number;
    output_reserve: number;
}

export interface SandboxSettings {
    enabled: boolean;
    engine: string;
    image: string;          // active tag (set by Build / Use)
    dockerfile: string;     // user-edited Dockerfile body (empty = recommended)
    network: boolean;
    cpu_limit: string;
    memory_limit: string;
    timeout_seconds: number;
}

// SandboxImageInfo is one entry in the built-images library
// shown on the Sandbox Settings tab.
export interface SandboxImageInfo {
    tag: string;
    created: string;     // ISO8601 (empty when unknown)
    size_bytes: number;
    active: boolean;
}

// SandboxImageStatus is the snapshot the Settings dialog
// reads on open and after each build event.
export interface SandboxImageStatus {
    active_tag: string;
    active_ready: boolean;
    /** True when the Active image reference is digest-pinned or
     *  locally content-addressed (TagPrefix:<sha>). False for
     *  mutable upstream tags like `python:3.12-slim` — Settings
     *  surfaces a warning banner in that case
     *  (security-hardening-2.md H5). */
    active_pinned_by_digest: boolean;
    building: boolean;
    recommended_dockerfile: string;
    current_dockerfile: string;
    images: SandboxImageInfo[];
}

export interface Settings {
    default_backend: string;
    local_endpoint: string;
    local_model: string;
    local_budget: BackendBudget;
    local_timeout_seconds: number;
    /** Total LLM call attempts (1 = no retries). Default 3.
     *  Set in Settings → Local LLM. Backoff timing knobs
     *  (base/max/jitter) are config-only — see README. */
    local_retry_max_attempts: number;
    vertex_project: string;
    vertex_region: string;
    vertex_model: string;
    vertex_budget: BackendBudget;
    vertex_timeout_seconds: number;
    /** Same as local_retry_max_attempts but for the Vertex backend. */
    vertex_retry_max_attempts: number;
    theme: string;
    location: string;
    mcp_profiles: MCPProfile[];
    disabled_tools: string[];
    mitl_overrides: Record<string, boolean>;
    sandbox: SandboxSettings;
    max_tool_rounds: number;
    /** app.log verbosity: "debug" | "info" | "warn" | "error".
     *  Default "info" keeps user messages, LLM responses, and
     *  tool arguments out of the log file. See
     *  docs/en/privacy-controls.md §3. */
    log_level: string;
}

export type SidebarPanel = 'sessions' | 'memory';

export interface ObjectInfo {
    id: string;
    type: string;
    mime_type: string;
    orig_name: string;
    created_at: string;
    session_id: string;
    size: number;
    lines?: number;
    tokens?: number;
}
