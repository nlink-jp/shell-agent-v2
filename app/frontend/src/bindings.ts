// Wails binding surface — `window.go.main.Bindings` global
// declaration. Kept separate from types.ts so the binding API
// can grow without polluting the domain-type leaf module.
//
// The actual implementation lives in the Go side (app/bindings.go);
// Wails generates a JS shim at build time. This file is the
// TypeScript view of that shim.
//
// Importing this file (`import './bindings'`) is sufficient to
// register the global; nothing is exported.

import type {
    Finding,
    LLMStatus,
    MessageData,
    ObjectInfo,
    PinnedMemory,
    SessionInfo,
    Settings,
    ToolInfo,
} from './types'

declare global {
    interface Window {
        go: {
            main: {
                Bindings: {
                    Send(message: string): Promise<string>;
                    Abort(): Promise<void>;
                    GetState(): Promise<string>;
                    GetBackend(): Promise<string>;
                    Version(): Promise<string>;
                    NewSession(): Promise<string>;
                    LoadSession(id: string): Promise<MessageData[]>;
                    ListSessions(): Promise<SessionInfo[]>;
                    RenameSession(id: string, title: string): Promise<void>;
                    DeleteSession(id: string): Promise<void>;
                    HasData(): Promise<boolean>;
                    GetFindings(): Promise<Finding[]>;
                    DeleteFindings(ids: string[]): Promise<number>;
                    DeletePinnedMemories(keys: string[]): Promise<number>;
                    ListObjects(): Promise<ObjectInfo[]>;
                    DeleteObject(id: string): Promise<void>;
                    DeleteObjects(ids: string[]): Promise<number>;
                    ObjectReferences(ids: string[]): Promise<Record<string, number>>;
                    ExportObject(id: string): Promise<void>;
                    GetObjectText(id: string): Promise<string>;
                    GetSettings(): Promise<Settings>;
                    SaveSettings(s: Settings): Promise<void>;
                    ApproveMITL(): Promise<void>;
                    RejectMITL(): Promise<void>;
                    RejectMITLWithFeedback(feedback: string): Promise<void>;
                    SendWithImages(message: string, imageDataURLs: string[]): Promise<string>;
                    SaveImage(dataURL: string): Promise<string>;
                    GetImageDataURL(id: string): Promise<string>;
                    GetTools(): Promise<ToolInfo[]>;
                    GetPinnedMemories(): Promise<PinnedMemory[]>;
                    UpdatePinnedMemory(key: string, content: string): Promise<void>;
                    DeletePinnedMemory(key: string): Promise<void>;
                    GetLLMStatus(): Promise<LLMStatus>;
                    SaveReport(content: string, filename: string): Promise<void>;
                    RestartMCP(): Promise<void>;
                    RestartSandbox(): Promise<void>;
                    RestartLLMBackend(): Promise<void>;
                    GetMCPStatus(): Promise<{name: string; status: string; tool_count: number; error?: string}[]>;
                    GetSessionObjects(sessionID: string): Promise<ObjectInfo[]>;
                    GetSessionTables(sessionID: string): Promise<{name: string; row_count: number; columns: string[]; description?: string}[]>;
                    PreviewTable(name: string, limit: number): Promise<{columns: string[]; rows: any[][]; total: number; truncated: boolean}>;
                    GetWorkFiles(sessionID: string): Promise<{path: string; size: number; mtime: number}[]>;
                    GetSidebarPrefs(): Promise<{width: number; collapsed: boolean}>;
                    SaveSidebarPrefs(width: number, collapsed: boolean): Promise<void>;
                };
            };
        };
        runtime: {
            EventsOn(event: string, callback: (...args: any[]) => void): () => void;
        };
    }
}

// Empty export so this file is a module (required for declare
// global to attach correctly).
export {}
