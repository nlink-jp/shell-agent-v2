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
    // 'running' while a tool is in flight; on completion the
    // backend now reports 'success' or 'error' (Phase A — wired
    // up but every tool currently reports 'success' until Phase
    // B classification per tool family lands). 'done' is kept as
    // a backward-compat fallback for older event payloads.
    status?: 'running' | 'success' | 'error' | 'done';
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

export interface BackendBudget {
    hot_token_limit: number;
    max_context_tokens: number;
    max_warm_tokens: number;
    max_tool_result_tokens: number;
}

export interface SandboxSettings {
    enabled: boolean;
    engine: string;
    image: string;
    network: boolean;
    cpu_limit: string;
    memory_limit: string;
    timeout_seconds: number;
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
