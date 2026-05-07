import {useState, useEffect, useRef, useCallback} from 'react'
import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import ChatInput from './ChatInput'
import ObjectImage, {clearObjectCache} from './ObjectImage'
import DataDisclosure from './DataDisclosure'
import FindingsDisclosure from './FindingsDisclosure'
import MessageItem from './components/MessageItem'
import Sidebar from './sidebar/Sidebar'
import SettingsDialog from './dialogs/SettingsDialog'
import MITLDialog from './dialogs/MITLDialog'
import Lightbox from './dialogs/Lightbox'
import ReportViewer from './dialogs/ReportViewer'
import BlobPreview, {type BlobPreviewData} from './dialogs/BlobPreview'
import PinToGlobalDialog, {type PinSource} from './dialogs/PinToGlobalDialog'
import ToolCallDetailsDialog from './dialogs/ToolCallDetailsDialog'
import 'highlight.js/styles/github-dark.css'
import 'katex/dist/katex.min.css'
import './themes.css'
import './App.css'
import './bindings'
import type {
    ChatMessage,
    ExpandedReport,
    Finding,
    GlobalMemory,
    LLMStatus,
    MCPStatus,
    MessageData,
    MITLRequest,
    ObjectInfo,
    SessionInfo,
    SessionMemory,
    Settings,
    SidebarPanel,
    ToolInfo,
} from './types'

// Module-scope stable references avoid re-instantiating plugin
// arrays on every render — used by the report-viewer overlay's
// ReactMarkdown. MessageItem keeps its own copy.
const MD_REMARK_PLUGINS = [remarkGfm, remarkMath]
const MD_REHYPE_PLUGINS = [rehypeHighlight, rehypeKatex]

// Allow object: protocol through ReactMarkdown URL sanitization.
// MessageItem has its own copy because it lives in a separate
// module; this one is for App-internal markdown blocks (cmd-popup,
// report-viewer overlay).
function urlTransform(url: string): string {
    if (url.startsWith('object:')) return url
    return defaultUrlTransform(url)
}

function nowTime(): string {
    return new Date().toLocaleTimeString('en-GB', {hour: '2-digit', minute: '2-digit', second: '2-digit'})
}

function formatSize(bytes: number): string {
    if (bytes < 1024) return bytes + ' B'
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
    return (bytes / (1024 * 1024)).toFixed(1) + ' MB'
}

function App() {
    const [state, setState] = useState<'idle' | 'busy'>('idle')
    const [messages, setMessages] = useState<ChatMessage[]>([])
    const [streaming, setStreaming] = useState('')
    const [backend, setBackend] = useState('')
    const [sidebarPanel, setSidebarPanel] = useState<SidebarPanel>('sessions')
    const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
    const [sidebarWidth, setSidebarWidth] = useState(280)
    // True once GetSidebarPrefs has resolved on mount. Used to
    // skip the persistence effect on the very first render
    // (otherwise we'd overwrite the saved state with the
    // initial useState defaults before the load completes).
    const sidebarPrefsLoaded = useRef(false)
    const resizingRef = useRef(false)
    const [sessions, setSessions] = useState<SessionInfo[]>([])
    const [currentSessionId, setCurrentSessionId] = useState<string>('')
    // Derived: is the active session private? Used by Sidebar to
    // hide ★ Pin buttons and by the chat-pane to show a 🔒 banner.
    // Reading from the sessions list keeps this in sync after a
    // refresh without an extra binding round-trip.
    const currentSessionPrivate = sessions.find(s => s.id === currentSessionId)?.private ?? false
    // Bumped to force the chat-pane Data disclosure to refetch.
    // Bumped on (a) session switch, (b) agent turn completion.
    const [dataRefreshTick, setDataRefreshTick] = useState(0)
    const [findings, setFindings] = useState<Finding[]>([])
    const [showSettings, setShowSettings] = useState(false)
    // editingSession / editTitle state moved into Sidebar (rename
    // is a sidebar-local concern). Same for the bulk-select sets
    // for findings and global / session memory.
    const [mitlRequest, setMitlRequest] = useState<MITLRequest | null>(null)
    // mitlFeedback state moved into MITLDialog (local concern).
    const [cmdResult, setCmdResult] = useState<string | null>(null)
    const [tools, setTools] = useState<ToolInfo[]>([])
    const [mcpStatus, setMcpStatus] = useState<MCPStatus[]>([])
    const [globalMemories, setGlobalMemories] = useState<GlobalMemory[]>([])
    const [sessionMemories, setSessionMemories] = useState<SessionMemory[]>([])
    // Objects panel state was removed in info-display redesign Phase 3 —
    // bulk selection, confirmation, and the global object list now live
    // inside the per-session DataDisclosure component.
    const [llmStatus, setLLMStatus] = useState<LLMStatus | null>(null)
    const [lightboxImage, setLightboxImage] = useState<string | null>(null)
    const [blobPreview, setBlobPreview] = useState<BlobPreviewData | null>(null)
    const [expandedReport, setExpandedReport] = useState<ExpandedReport | null>(null)
    // Tool-event detail overlay: holds the tool_call_id for the
    // bubble the user just clicked. The dialog itself fetches the
    // args + result via GetToolCallDetails, so App only needs the id.
    const [inspectingToolCallId, setInspectingToolCallId] = useState<string | null>(null)
    // Pin to Global Memory dialog state. The dialog is reused for
    // both Session Memory rows and Findings; the kind discriminates
    // which backend binding to call on confirm.
    const [pinDialog, setPinDialog] = useState<{
        kind: PinSource;
        id: string;       // fact text for session_memory; finding ID for finding
        preview: string;  // shown verbatim in the dialog body
    } | null>(null)
    const [settings, setSettings] = useState<Settings | null>(null)
    // settingsTab state moved into SettingsDialog (Phase 3 of
    // frontend-decomposition: it's a local-only concern).
    const [progressTool, setProgressTool] = useState('')
    // retryStatus shows a transient footer badge while the LLM
    // backend is between attempts (retry-backoff). Cleared as soon
    // as the next tool_start / tool_end arrives.
    const [retryStatus, setRetryStatus] = useState('')
    // Post-response background tasks (title generation, memory
    // compaction, pinned-fact extraction). bgTasks is the set of
    // names currently running; bgFailure is the most recent task
    // that returned an error and is still being flashed (5 s).
    const [bgTasks, setBgTasks] = useState<string[]>([])
    const [bgFailure, setBgFailure] = useState<{name: string; error: string} | null>(null)
    const messagesEndRef = useRef<HTMLDivElement>(null)
    const messagesContainerRef = useRef<HTMLDivElement>(null)
    // scrollMessagesToBottom forces the .messages container to its
    // true scroll bottom. Using scrollTop = scrollHeight (rather
    // than messagesEndRef.scrollIntoView) ensures the container's
    // padding-bottom and the last bubble's margin-bottom are both
    // honoured — scrollIntoView on a 0-height anchor stops short
    // of those by ~28 px (one visual line).
    const scrollMessagesToBottom = (smooth: boolean) => {
        const el = messagesContainerRef.current
        if (!el) return
        if (smooth) {
            el.scrollTo({top: el.scrollHeight, behavior: 'smooth'})
        } else {
            el.scrollTop = el.scrollHeight
        }
    }

    useEffect(() => {
        if (window.runtime) {
            const cleanupStream = window.runtime.EventsOn('agent:stream', (data: any) => {
                if (data.done) {
                    setStreaming('')
                } else {
                    setProgressTool('') // clear progress when streaming starts
                    setStreaming(prev => prev + data.token)
                }
            })
            const cleanupActivity = window.runtime.EventsOn('agent:activity', (data: any) => {
                if (data.type === 'tool_end') {
                    setProgressTool('')
                    setRetryStatus('')
                    // Refresh the Data panel after every tool — any
                    // tool can register an object, load a table, or
                    // write a /work file, and waiting until the
                    // turn ends is too late for streaming UX.
                    setDataRefreshTick(t => t + 1)
                    // Phase A: backend reports 'success' or 'error'.
                    // Old event payloads without status fall back to
                    // success so older runs still render.
                    const endStatus: 'success' | 'error' = data.status === 'error' ? 'error' : 'success'
                    setMessages(prev => {
                        // Prefer matching by tool_call_id (more robust:
                        // independent of any tool_progress text updates
                        // the running bubble may have undergone, e.g.
                        // analyze-data's per-window updates). Fall back
                        // to content equality for legacy events that
                        // arrive without a call ID. See
                        // docs/en/tool-progress-events.md §4.1.
                        const eventID = data.tool_call_id || ''
                        let idx = -1
                        for (let i = prev.length - 1; i >= 0; i--) {
                            const m = prev[i]
                            if (m.role !== 'tool-event' || m.status !== 'running') continue
                            if (eventID) {
                                if (m.toolCallId === eventID) { idx = i; break }
                            } else if (m.content === data.detail) {
                                idx = i; break
                            }
                        }
                        if (idx === -1) return prev
                        const next = prev.slice()
                        // Stamp toolCallId from the tool_end event so
                        // the bubble becomes clickable for inspection
                        // (tool_start usually arrives with the same id
                        // already, but tool_end is authoritative).
                        next[idx] = {...next[idx], status: endStatus, toolCallId: data.tool_call_id || next[idx].toolCallId}
                        return next
                    })
                } else if (data.type === 'tool_start') {
                    setProgressTool(data.detail || '')
                    setRetryStatus('')
                    setMessages(prev => [...prev, {role: 'tool-event', content: data.detail || '', status: 'running', timestamp: nowTime(), toolCallId: data.tool_call_id || undefined}])
                } else if (data.type === 'tool_progress') {
                    // In-place update for the running tool-event whose
                    // tool_call_id matches. Lets long-running tools
                    // (e.g. analyze-data sliding-window summarisation)
                    // surface per-window progress without spawning a
                    // fresh "running" bubble per tick that would
                    // otherwise stay stuck (#5). Match by id, not by
                    // displayed text, so multiple parallel tools
                    // (future capability) can't cross-contaminate.
                    // See docs/en/tool-progress-events.md.
                    if (!data.tool_call_id) return
                    setProgressTool(data.detail || '')
                    setMessages(prev => {
                        let idx = -1
                        for (let i = prev.length - 1; i >= 0; i--) {
                            const m = prev[i]
                            if (m.role === 'tool-event' && m.status === 'running' && m.toolCallId === data.tool_call_id) {
                                idx = i
                                break
                            }
                        }
                        if (idx === -1) return prev
                        const next = prev.slice()
                        next[idx] = {...next[idx], content: data.detail || ''}
                        return next
                    })
                } else if (data.type === 'thinking') {
                    setProgressTool(data.detail || '')
                } else if (data.type === 'assistant_text') {
                    // Intermediate assistant text accompanying a
                    // tool call. The system prompt asks the model
                    // to explain what it's about to do; surface
                    // that explanation as a real chat bubble in
                    // the live conversation. Reload from disk
                    // already shows the same content via
                    // session.Records — this just brings the
                    // live UX in line with what's persisted.
                    const txt = (data.detail || '').trim()
                    if (txt) {
                        setMessages(prev => [...prev, {role: 'assistant', content: txt, timestamp: nowTime()}])
                    }
                } else if (data.type === 'retry_backoff') {
                    setRetryStatus(data.detail || 'retrying...')
                }
            })
            const cleanupGlobalMemory = window.runtime.EventsOn('global_memory:updated', () => {
                if (window.go) window.go.main.Bindings.GetGlobalMemories().then(setGlobalMemories)
            })
            const cleanupSessionMemory = window.runtime.EventsOn('session_memory:updated', () => {
                if (window.go) window.go.main.Bindings.GetSessionMemories().then(setSessionMemories)
            })
            // Mirror of *_memory:updated for findings — emitted by
            // the backend after promote-finding and analyze-data
            // auto-promote so the panel reflects new findings
            // without waiting for a session switch.
            const cleanupFindings = window.runtime.EventsOn('findings:updated', () => {
                if (window.go) window.go.main.Bindings.GetFindings().then(setFindings)
            })
            const cleanupReport = window.runtime.EventsOn('report:created', (data: any) => {
                setMessages(prev => [...prev, {
                    role: 'report' as const,
                    content: data.content,
                    timestamp: nowTime(),
                }])
            })
            const cleanupMitl = window.runtime.EventsOn('mitl:request', (data: any) => {
                setMitlRequest(data)
            })
            const cleanupTitle = window.runtime.EventsOn('session:title', (data: any) => {
                setSessions(prev => prev.map(s =>
                    s.id === data.session_id ? {...s, title: data.title} : s
                ))
            })
            const cleanupBgStart = window.runtime.EventsOn('bg-task:start', (data: any) => {
                const name = String(data.name || '')
                if (!name) return
                setBgTasks(prev => prev.includes(name) ? prev : [...prev, name])
            })
            const cleanupBgEnd = window.runtime.EventsOn('bg-task:end', (data: any) => {
                const name = String(data.name || '')
                const error = String(data.error || '')
                if (!name) return
                setBgTasks(prev => prev.filter(n => n !== name))
                if (error) {
                    setBgFailure({name, error})
                    // Clear the same failure 5 s later. Guarded by
                    // name comparison so a newer failure isn't
                    // wiped by an older timer.
                    setTimeout(() => {
                        setBgFailure(f => (f && f.name === name && f.error === error) ? null : f)
                    }, 5000)
                }
            })
            return () => { cleanupStream(); cleanupActivity(); cleanupGlobalMemory(); cleanupSessionMemory(); cleanupFindings(); cleanupReport(); cleanupMitl(); cleanupTitle(); cleanupBgStart(); cleanupBgEnd() }
        }
    }, [])

    // isPreviewableTextMime returns true for blob mimes whose
    // bytes are reasonable to dump as text in the BlobPreview
    // dialog. Anything else (image/*, application/octet-stream,
    // unknown binary) stays a click no-op so we don't surprise
    // the user with megabytes of mojibake.
    const isPreviewableTextMime = (mime: string | undefined): boolean => {
        if (!mime) return false
        if (mime.startsWith('text/')) return true
        return (
            mime === 'application/json' ||
            mime === 'application/xml' ||
            mime === 'application/javascript' ||
            mime === 'application/x-ndjson'
        )
    }

    // Friendly label for each background task code. Centralised so
    // a future i18n pass can swap to a hook without hunting through
    // JSX. The codes match agent.BgTaskEvent.Name.
    const bgTaskLabel = (code: string): string => {
        switch (code) {
            case 'title': return 'Title generation'
            case 'memory-compaction': return 'Memory compaction'
            case 'pinned-extraction': return 'Memory extraction'
            default: return code
        }
    }

    useEffect(() => {
        scrollMessagesToBottom(true)
    }, [messages, streaming])

    // Session-switch jump: when the user opens a different session
    // we want the view to start at the latest message, not the
    // beginning of history. The smooth-scroll effect above handles
    // incremental message append fine, but for a full restore the
    // long animation is interrupted by markdown/image layout
    // settling and ends up parked somewhere in the middle (or at
    // the top, since that's where the layout starts). Force an
    // instant jump on session change, plus a delayed retry to
    // catch late-rendering markdown/images that grew the page
    // after the first jump.
    useEffect(() => {
        if (!currentSessionId) return
        scrollMessagesToBottom(false)
        const t1 = window.setTimeout(() => scrollMessagesToBottom(false), 50)
        const t2 = window.setTimeout(() => scrollMessagesToBottom(false), 250)
        return () => { window.clearTimeout(t1); window.clearTimeout(t2) }
    }, [currentSessionId])

    // Persist sidebarCollapsed whenever it changes after the
    // initial load. Width is persisted at resize-end inside
    // startResize so we don't write on every drag pixel.
    useEffect(() => {
        if (!sidebarPrefsLoaded.current || !window.go) return
        window.go.main.Bindings.SaveSidebarPrefs(sidebarWidth, sidebarCollapsed).catch(() => {})
    }, [sidebarCollapsed])  // eslint-disable-line react-hooks/exhaustive-deps

    useEffect(() => {
        // Wails populates window.go and Go startup runs asynchronously.
        // Retry until backend reports a non-empty name (agent ready).
        let cancel = false
        function load() {
            if (cancel) return
            if (!window.go) { setTimeout(load, 50); return }
            window.go.main.Bindings.GetBackend().then(b => {
                if (cancel) return
                if (b) setBackend(b)
                else setTimeout(load, 100)
            })
            window.go.main.Bindings.GetSettings().then(s => {
                if (!cancel && s.theme) document.documentElement.setAttribute('data-theme', s.theme)
            })
            // Restore the user's saved sidebar layout (GitHub #4
            // — width and collapsed flag were ephemeral until now).
            window.go.main.Bindings.GetSidebarPrefs().then(p => {
                if (cancel || !p) return
                if (p.width > 0) setSidebarWidth(p.width)
                setSidebarCollapsed(p.collapsed)
                sidebarPrefsLoaded.current = true
            })
            // Footer strip needs initial values too — Phase 4 moved
            // Tokens out of the Memory panel, so the on-mount fetch
            // can't piggy-back on that panel's effect any more.
            window.go.main.Bindings.GetLLMStatus().then(s => {
                if (!cancel && s) setLLMStatus(s)
            })
        }
        load()
        return () => { cancel = true }
    }, [])

    const refreshSessions = useCallback(async () => {
        if (window.go) {
            const s = await window.go.main.Bindings.ListSessions()
            setSessions(s || [])
        }
    }, [])

    const handleNewSession = useCallback(async () => {
        // Block on bgTasks too so a click during the
        // post-response window can't race a session swap into the
        // middle of title generation / pinned-fact extraction.
        // Sidebar already disables the button via busy=isBusy, but
        // keep the guard here in case the path is reached through
        // a keyboard shortcut or programmatic call.
        if (window.go && state === 'idle' && bgTasks.length === 0) {
            const id = await window.go.main.Bindings.NewSession()
            setCurrentSessionId(id)
            setMessages([])
            setStreaming('')
            await refreshSessions()
        }
    }, [state, bgTasks, refreshSessions])

    // Private session: same flow as handleNewSession but routes
    // through NewPrivateSession so the backend marks Session.Private
    // and Global Memory promotion is suppressed for this conversation.
    const handleNewPrivateSession = useCallback(async () => {
        if (window.go && state === 'idle' && bgTasks.length === 0) {
            const id = await window.go.main.Bindings.NewPrivateSession()
            setCurrentSessionId(id)
            setMessages([])
            setStreaming('')
            await refreshSessions()
        }
    }, [state, bgTasks, refreshSessions])

    // Export the named session via a native save dialog. No-op /
    // returns empty path if the user cancels. Errors are surfaced
    // via the same alert pattern as other failed binding calls so
    // the user sees the message rather than a silent fail.
    const handleExportSession = useCallback(async (sessionID: string) => {
        if (!window.go || state !== 'idle' || bgTasks.length > 0) {
            return
        }
        try {
            await window.go.main.Bindings.ExportSession(sessionID)
            // No success toast — the file dialog itself is the
            // confirmation. Refresh in case the active session's
            // state was touched (analysis engine close+reopen).
            await refreshSessions()
        } catch (e) {
            alert('Export failed: ' + String(e))
        }
    }, [state, bgTasks, refreshSessions])

    // Import a .shellagent bundle and auto-switch to the new
    // session. The backend has already called LoadSession so we
    // just need to mirror handleNewSession's UI plumbing
    // (currentSessionId + restoredMessages).
    const handleImportSession = useCallback(async () => {
        if (!window.go || state !== 'idle' || bgTasks.length > 0) {
            return
        }
        try {
            const newID = await window.go.main.Bindings.ImportSession()
            if (!newID) {
                return // user cancelled
            }
            // Pull the freshly loaded session's messages for display.
            const msgs = await window.go.main.Bindings.LoadSession(newID)
            setCurrentSessionId(newID)
            setMessages(restoredMessages(msgs))
            setStreaming('')
            await refreshSessions()
        } catch (e) {
            alert('Import failed: ' + String(e))
        }
    }, [state, bgTasks, refreshSessions])

    // restoredMessages converts the backend's MessageData[] into
    // the ChatMessage[] the chat pane consumes. The mapping is
    // mostly a 1:1 — the only field that needs care is `status`,
    // which the backend now populates for `tool-event` rows on
    // session restore so the success / error styling matches the
    // live experience. Design: docs/en/tool-event-restore.md.
    const restoredMessages = (msgs: MessageData[] | null | undefined): ChatMessage[] =>
        (msgs || []).map(m => ({
            role: m.role as ChatMessage['role'],
            content: m.content,
            timestamp: m.timestamp,
            ...(m.status ? {status: m.status} : {}),
            // Restored user messages with attached images get
            // imageUrls populated as `object:<id>` URLs. The
            // MessageItem image renderer detects the prefix and
            // routes through ObjectImage so the original images
            // re-appear in the bubble.
            ...(m.object_ids && m.object_ids.length > 0
                ? {imageUrls: m.object_ids.map(id => `object:${id}`)}
                : {}),
            // Restored tool-event rows carry tool_call_id so the
            // bubble is clickable for the inspect overlay.
            ...(m.tool_call_id ? {toolCallId: m.tool_call_id} : {}),
        }))

    const handleLoadSession = useCallback(async (id: string) => {
        if (window.go && state === 'idle' && bgTasks.length === 0) {
            clearObjectCache() // clear image cache on session switch
            const msgs = await window.go.main.Bindings.LoadSession(id)
            setCurrentSessionId(id)
            setMessages(restoredMessages(msgs))
            setStreaming('')
        }
    }, [state, bgTasks])

    const handleDeleteSession = useCallback(async (id: string) => {
        if (!window.go) return
        // Backend (agent.DeleteSession, v0.4.2) drains post-tasks
        // and gates on the Idle/Busy state machine — let it
        // reject with ErrBusy rather than silently no-opping
        // here. Surfacing the error keeps the Sidebar's
        // Deleting indicator accurate (it clears in finally
        // regardless of resolution).
        try {
            await window.go.main.Bindings.DeleteSession(id)
        } catch (e) {
            alert('Delete failed: ' + String(e))
            return
        }
        const remaining = await window.go.main.Bindings.ListSessions()
        if (!remaining || remaining.length === 0) {
            // No sessions left — auto-create one
            const newId = await window.go.main.Bindings.NewSession()
            setCurrentSessionId(newId)
            setMessages([])
            const refreshed = await window.go.main.Bindings.ListSessions()
            setSessions(refreshed || [])
        } else {
            setSessions(remaining)
            if (id === currentSessionId) {
                // Switch to the first remaining session
                const msgs = await window.go.main.Bindings.LoadSession(remaining[0].id)
                setCurrentSessionId(remaining[0].id)
                setMessages(restoredMessages(msgs))
            }
        }
    }, [currentSessionId])

    // startRename / commitRename moved into Sidebar (rename is a
    // sidebar-local concern; the rename binding call is plumbed
    // back via the onRenameSession prop).

    // On startup: load sessions, auto-create if none exist
    useEffect(() => {
        (async () => {
            if (!window.go) return
            const s = await window.go.main.Bindings.ListSessions()
            setSessions(s || [])
            if (!s || s.length === 0) {
                const id = await window.go.main.Bindings.NewSession()
                setCurrentSessionId(id)
                const refreshed = await window.go.main.Bindings.ListSessions()
                setSessions(refreshed || [])
            } else {
                // Load the most recent session
                const msgs = await window.go.main.Bindings.LoadSession(s[0].id)
                setCurrentSessionId(s[0].id)
                setMessages(restoredMessages(msgs))
            }
        })()
    }, [])

    const refreshFindings = useCallback(async () => {
        if (window.go) {
            const f = await window.go.main.Bindings.GetFindings()
            setFindings(f || [])
        }
    }, [])

    useEffect(() => {
        if (sidebarPanel === 'memory' && window.go) {
            refreshFindings()
            window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            window.go.main.Bindings.GetGlobalMemories().then(setGlobalMemories)
            window.go.main.Bindings.GetSessionMemories().then(setSessionMemories)
        }
    }, [sidebarPanel, refreshFindings])

    // Auto-save settings on change
    const updateSetting = useCallback(async (patch: Partial<Settings>) => {
        if (!settings) return
        const updated = {...settings, ...patch}
        setSettings(updated)
        if (window.go) {
            await window.go.main.Bindings.SaveSettings(updated)
            // Sandbox config is read at engine construction time; ask the
            // agent to rebuild the engine when any sandbox field has
            // changed so the user doesn't have to restart the app.
            if (patch.sandbox && JSON.stringify(patch.sandbox) !== JSON.stringify(settings.sandbox)) {
                window.go.main.Bindings.RestartSandbox()
            }
            // Per-request timeout / per-attempt retry policy is captured
            // when the LLM backend wrapper is built. Rebuild on change.
            if (patch.local_timeout_seconds !== undefined || patch.vertex_timeout_seconds !== undefined) {
                window.go.main.Bindings.RestartLLMBackend()
            }
        }
    }, [settings])

    const openSettings = useCallback(async () => {
        if (window.go) {
            const s = await window.go.main.Bindings.GetSettings()
            setSettings(s)
            window.go.main.Bindings.GetTools().then(setTools)
            window.go.main.Bindings.GetMCPStatus().then(s => setMcpStatus(s || []))
        }
        setShowSettings(true)
    }, [])

    // bgTasks holds names of post-response tasks the backend is
    // still running (title gen / memory compaction / pinned-fact
    // extraction). The agent stays Busy on the backend until they
    // all finish, so we treat them as part of "busy" for the input
    // gate. Without this, ChatInput re-enables the moment Send
    // returns, the user types, the backend rejects with ErrBusy,
    // and an "Error: agent is busy" toast appears.
    const postBusy = bgTasks.length > 0
    const isBusy = state === 'busy' || postBusy
    const canChat = state === 'idle' && !postBusy && currentSessionId !== ''

    const handleSend = useCallback(async (text: string, images: string[]) => {
        if ((!text && images.length === 0) || isBusy || !currentSessionId) return

        // Slash commands that need a native file dialog are routed
        // through the bindings directly rather than agent.Send: the
        // file dialog must originate from the binding context, and
        // the agent's slash dispatcher has no way to surface a
        // dialog. Intercept BEFORE the optimistic user-message
        // append so the input doesn't echo the command into chat.
        const trimmed = text.trim()
        if (trimmed === '/export') {
            await handleExportSession(currentSessionId)
            return
        }
        if (trimmed === '/import') {
            await handleImportSession()
            return
        }

        setMessages(prev => [...prev, {
            role: 'user', content: text, timestamp: nowTime(),
            imageUrls: images.length > 0 ? images : undefined,
        }])
        setState('busy')
        setStreaming('')

        try {
            const response = images.length > 0
                ? await window.go.main.Bindings.SendWithImages(text, images)
                : await window.go.main.Bindings.Send(text)
            if (response && response.startsWith('[CMD]')) {
                // Command result: show as popup, remove user message from chat
                setCmdResult(response.slice(5))
                setMessages(prev => prev.slice(0, -1)) // remove the optimistic user message
            } else if (response && response.trim()) {
                setMessages(prev => [...prev, {role: 'assistant', content: response, timestamp: nowTime()}])
            }
        } catch (err: any) {
            setMessages(prev => [...prev, {role: 'system', content: `Error: ${err.message || err}`, timestamp: nowTime()}])
        } finally {
            setState('idle')
            setStreaming('')
            setProgressTool('')
            setRetryStatus('')
            // Mark any leftover running tool-events as completed
            // (e.g. on agent error or abort). We can't know whether
            // they actually succeeded, but leaving them in 'running'
            // forever is worse — fall back to 'success' so the
            // bubble at least stops pulsing.
            setMessages(prev => prev.map(m => m.role === 'tool-event' && m.status === 'running' ? {...m, status: 'success'} : m))
            if (window.go) {
                window.go.main.Bindings.GetBackend().then(setBackend)
                window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            }
            // The agent may have produced new objects, loaded data,
            // or written /work files — refresh the Data disclosure.
            setDataRefreshTick(t => t + 1)
        }
    }, [isBusy, currentSessionId, handleExportSession, handleImportSession])

    function startResize(e: React.MouseEvent) {
        e.preventDefault()
        resizingRef.current = true
        const startX = e.clientX
        const startW = sidebarWidth
        let lastW = startW
        function onMove(ev: MouseEvent) {
            if (!resizingRef.current) return
            lastW = Math.max(180, Math.min(500, startW + ev.clientX - startX))
            setSidebarWidth(lastW)
        }
        function onUp() {
            resizingRef.current = false
            document.removeEventListener('mousemove', onMove)
            document.removeEventListener('mouseup', onUp)
            // Persist the final width so it survives app restart.
            // The drag itself only updates React state; the
            // backend save fires once on release.
            if (window.go) {
                window.go.main.Bindings.SaveSidebarPrefs(lastW, sidebarCollapsed).catch(() => {})
            }
        }
        document.addEventListener('mousemove', onMove)
        document.addEventListener('mouseup', onUp)
    }

    const handleAbort = useCallback(async () => {
        if (window.go) await window.go.main.Bindings.Abort()
    }, [])

    return (
        <div className="app">
            <div className="titlebar-drag" />
            <Sidebar
                sidebarPanel={sidebarPanel}
                setSidebarPanel={setSidebarPanel}
                sidebarCollapsed={sidebarCollapsed}
                setSidebarCollapsed={setSidebarCollapsed}
                sidebarWidth={sidebarWidth}
                onStartResize={startResize}
                sessions={sessions}
                currentSessionId={currentSessionId}
                currentSessionPrivate={currentSessionPrivate}
                busy={isBusy}
                onLoadSession={handleLoadSession}
                onNewSession={handleNewSession}
                onNewPrivateSession={handleNewPrivateSession}
                onExportSession={handleExportSession}
                onImportSession={handleImportSession}
                onDeleteSession={handleDeleteSession}
                onRenameSession={async (id, title) => {
                    if (!window.go) return
                    await window.go.main.Bindings.RenameSession(id, title)
                    await refreshSessions()
                }}
                globalMemories={globalMemories}
                onGlobalMemoryDelete={async facts => {
                    await window.go.main.Bindings.DeleteGlobalMemories(facts)
                    const updated = await window.go.main.Bindings.GetGlobalMemories()
                    setGlobalMemories(updated)
                }}
                onGlobalMemoryDeleteOne={async fact => {
                    await window.go.main.Bindings.DeleteGlobalMemory(fact)
                    const updated = await window.go.main.Bindings.GetGlobalMemories()
                    setGlobalMemories(updated)
                }}
                sessionMemories={sessionMemories}
                onSessionMemoryDelete={async facts => {
                    await window.go.main.Bindings.DeleteSessionMemories(facts)
                    const updated = await window.go.main.Bindings.GetSessionMemories()
                    setSessionMemories(updated)
                }}
                onPinSessionMemory={(fact) => {
                    // Open the category-picker dialog. The actual
                    // PinSessionMemory call lives in the dialog
                    // confirm handler so both kinds (session-memory /
                    // finding) take the same path.
                    setPinDialog({kind: 'session_memory', id: fact, preview: fact})
                }}
                onOpenSettings={openSettings}
            />
            <div className="main">
                {currentSessionId && currentSessionPrivate && (
                    <div className="private-session-banner" title="Global Memory promotion is suppressed for this session. Session Memory and Findings work normally and are deleted with the session.">
                        <span className="private-session-icon">🔒</span>
                        <span className="private-session-label">Private session — facts won't be promoted to Global Memory</span>
                    </div>
                )}
                {currentSessionId && (
                    <DataDisclosure
                        sessionId={currentSessionId}
                        refreshTick={dataRefreshTick}
                        sandboxEnabled={!!settings?.sandbox?.enabled}
                        onObjectsChanged={() => clearObjectCache()}
                        onPreviewObject={async obj => {
                            try {
                                if (obj.type === 'image') {
                                    const url = await window.go.main.Bindings.GetImageDataURL(obj.id)
                                    setLightboxImage(url)
                                } else if (obj.type === 'report') {
                                    const text = await window.go.main.Bindings.GetObjectText(obj.id)
                                    const title = (text.split('\n')[0] || '').replace(/^#\s*/, '') || obj.orig_name || obj.id
                                    setExpandedReport({title, content: text})
                                } else if (obj.type === 'blob' && isPreviewableTextMime(obj.mime_type)) {
                                    // Text-shaped blobs (CSV, JSON, plain
                                    // text, etc.) get an in-app preview;
                                    // CSV/TSV in particular is rendered as
                                    // a simple table so the data-analysis
                                    // pitch holds up. Binary blobs still
                                    // fall through to the no-op.
                                    const text = await window.go.main.Bindings.GetObjectText(obj.id)
                                    const limit = 100 * 1024
                                    const truncated = text.length > limit
                                    setBlobPreview({
                                        title: obj.orig_name || obj.id,
                                        mime: obj.mime_type || 'text/plain',
                                        content: truncated ? text.slice(0, limit) : text,
                                        sizeBytes: obj.size,
                                        truncated,
                                    })
                                }
                                // Other blob types (binary) intentionally
                                // remain a no-op — Export still works.
                            } catch {}
                        }}
                    />
                )}
                {currentSessionId && (
                    <FindingsDisclosure
                        sessionId={currentSessionId}
                        refreshTick={dataRefreshTick}
                        sessionPrivate={currentSessionPrivate}
                        onPinFinding={(f) => {
                            setPinDialog({kind: 'finding', id: f.id, preview: f.content})
                        }}
                    />
                )}
                <div className="messages" ref={messagesContainerRef}>
                    {messages.filter(msg => msg.role !== 'tool').map((msg, i) => (
                        <div key={i} className={`message ${msg.role}`}>
                            <MessageItem msg={msg} onLightbox={setLightboxImage} onExpandReport={setExpandedReport} onToolEventClick={setInspectingToolCallId} />
                        </div>
                    ))}
                    {streaming && (
                        <div className="message assistant streaming">
                            <div className="message-header">
                                <span className="message-role">assistant</span>
                            </div>
                            <div className="message-content">
                                <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform}>
                                    {streaming}
                                </ReactMarkdown>
                            </div>
                        </div>
                    )}
                    <div ref={messagesEndRef} />
                </div>
                {cmdResult && (
                    <div className="cmd-popup">
                        <div className="cmd-popup-header">
                            <span>Command Result</span>
                            <button onClick={() => setCmdResult(null)}>&#x2715;</button>
                        </div>
                        <div className="cmd-popup-body">
                            <ReactMarkdown remarkPlugins={[remarkGfm, remarkMath]} rehypePlugins={[rehypeHighlight, rehypeKatex]}>
                                {cmdResult}
                            </ReactMarkdown>
                        </div>
                    </div>
                )}
                <div className="input-status-bar">
                    <span className={`backend-badge ${backend}`}>{backend || '...'}</span>
                    {state === 'busy' && retryStatus && (
                        <span className="retry-badge" title="LLM backend hit a transient failure and is retrying">
                            {retryStatus}
                        </span>
                    )}
                    {llmStatus && (
                        <>
                            <span className="status-msg-counts" title="Recent messages kept verbatim · older messages condensed into summaries">
                                Messages: {llmStatus.hot_messages}
                                {llmStatus.warm_summaries > 0 && ` (+${llmStatus.warm_summaries} summarized)`}
                            </span>
                            <span className="status-tokens" title="Prompt / output tokens of the most recent LLM call">
                                Tokens: {llmStatus.prompt_tokens.toLocaleString()} in / {llmStatus.output_tokens.toLocaleString()} out
                            </span>
                        </>
                    )}
                    {state === 'busy' && <span className="tool-progress">{progressTool || 'Thinking...'}</span>}
                    {bgTasks.length > 0 && (
                        <span className="bg-task-badge" title="Background work running after the previous reply. Input is disabled until these finish; click Abort to interrupt.">
                            Background: {bgTasks.map(bgTaskLabel).join(', ')}
                        </span>
                    )}
                    {bgFailure && (
                        <span className="bg-task-failure" title={bgFailure.error}>
                            Failed: {bgTaskLabel(bgFailure.name)}
                        </span>
                    )}
                </div>
                {isBusy ? (
                    <div className="input-area">
                        <div className="input-row">
                            <textarea
                                disabled
                                rows={3}
                                placeholder={state === 'busy' ? 'Agent is busy...' : 'Finishing up background tasks...'}
                            />
                            <button className="abort-btn" onClick={handleAbort}>Abort</button>
                        </div>
                    </div>
                ) : (
                    <ChatInput onSend={handleSend} disabled={!canChat} />
                )}
            </div>

            {mitlRequest && (
                <MITLDialog
                    request={mitlRequest}
                    onApprove={() => {
                        setMitlRequest(null)
                        window.go.main.Bindings.ApproveMITL()
                    }}
                    onReject={() => {
                        setMitlRequest(null)
                        window.go.main.Bindings.RejectMITL()
                    }}
                    onRejectWithFeedback={fb => {
                        setMitlRequest(null)
                        window.go.main.Bindings.RejectMITLWithFeedback(fb)
                    }}
                />
            )}

            {showSettings && settings && (
                <SettingsDialog
                    settings={settings}
                    tools={tools}
                    mcpStatus={mcpStatus}
                    onUpdate={updateSetting}
                    onClose={() => setShowSettings(false)}
                    onRestartMCP={async () => {
                        if (!window.go) return
                        await window.go.main.Bindings.RestartMCP()
                        const status = await window.go.main.Bindings.GetMCPStatus()
                        setMcpStatus(status || [])
                        window.go.main.Bindings.GetTools().then(setTools)
                    }}
                />
            )}

            {lightboxImage && (
                <Lightbox src={lightboxImage} onClose={() => setLightboxImage(null)} />
            )}

            {blobPreview && (
                <BlobPreview data={blobPreview} onClose={() => setBlobPreview(null)} />
            )}

            {expandedReport && (
                <ReportViewer
                    report={expandedReport}
                    onClose={() => setExpandedReport(null)}
                    onLightbox={setLightboxImage}
                    onSaveReport={(content, filename) => window.go?.main.Bindings.SaveReport(content, filename)}
                />
            )}

            {inspectingToolCallId && (
                <ToolCallDetailsDialog
                    toolCallId={inspectingToolCallId}
                    onClose={() => setInspectingToolCallId(null)}
                />
            )}

            {pinDialog && (
                <PinToGlobalDialog
                    factPreview={pinDialog.preview}
                    sourceKind={pinDialog.kind}
                    onCancel={() => setPinDialog(null)}
                    onConfirm={async (category) => {
                        const target = pinDialog
                        setPinDialog(null)
                        try {
                            if (target.kind === 'session_memory') {
                                await window.go.main.Bindings.PinSessionMemory(target.id, category)
                            } else {
                                await window.go.main.Bindings.PinFinding(target.id, category)
                            }
                            const updatedG = await window.go.main.Bindings.GetGlobalMemories()
                            setGlobalMemories(updatedG)
                        } catch (err) {
                            alert('Pin failed: ' + ((err as any)?.message ?? String(err)))
                        }
                    }}
                />
            )}
        </div>
    )
}

export default App
