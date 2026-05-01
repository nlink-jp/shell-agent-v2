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
}

export interface SessionInfo {
    id: string;
    title: string;
    updated_at: string;
}

export interface MessageData {
    role: string;
    content: string;
    timestamp: string;
}

export interface Finding {
    id: string;
    content: string;
    session_id: string;
    session_title: string;
    tags: string[];
    created_label: string;
}

export interface ToolInfo {
    name: string;
    description: string;
    category: string;
    source: string;
}

export interface PinnedMemory {
    fact: string;
    native_fact: string;
    category: string;
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
    hot_token_limit: number;
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
    vertex_project: string;
    vertex_region: string;
    vertex_model: string;
    vertex_budget: BackendBudget;
    vertex_timeout_seconds: number;
    theme: string;
    location: string;
    mcp_profiles: MCPProfile[];
    disabled_tools: string[];
    mitl_overrides: Record<string, boolean>;
    memory_use_v2: boolean;
    sandbox: SandboxSettings;
    max_tool_rounds: number;
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
}
