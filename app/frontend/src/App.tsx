import {useState, useEffect, useRef, useCallback, memo, useMemo} from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import ChatInput from './ChatInput'
import ObjectImage, {clearObjectCache} from './ObjectImage'
import {defaultUrlTransform} from 'react-markdown'
import 'highlight.js/styles/github-dark.css'
import 'katex/dist/katex.min.css'
import './themes.css'
import './App.css'

// Allow object: protocol through ReactMarkdown URL sanitization.
// ReactMarkdown v10 defaultUrlTransform strips non-http/https/mailto protocols.
// object: URLs are resolved by ObjectImage component via img component override.
function urlTransform(url: string): string {
    if (url.startsWith('object:')) return url
    return defaultUrlTransform(url)
}

// Module-scope stable references avoid re-instantiating plugin arrays on every
// render — otherwise ReactMarkdown sees new props and re-parses every message.
const MD_REMARK_PLUGINS = [remarkGfm, remarkMath]
const MD_REHYPE_PLUGINS = [rehypeHighlight, rehypeKatex]

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
                    GetMCPStatus(): Promise<{name: string; status: string; tool_count: number; error?: string}[]>;
                };
            };
        };
        runtime: {
            EventsOn(event: string, callback: (...args: any[]) => void): () => void;
        };
    }
}

interface ChatMessage {
    role: 'user' | 'assistant' | 'system' | 'tool' | 'report' | 'tool-event' | 'summary';
    content: string;
    timestamp: string;
    imageUrls?: string[];
    status?: 'running' | 'done';
}

interface SessionInfo {
    id: string;
    title: string;
    updated_at: string;
}

interface MessageData {
    role: string;
    content: string;
    timestamp: string;
}

function nowTime(): string {
    return new Date().toLocaleTimeString('en-GB', {hour: '2-digit', minute: '2-digit', second: '2-digit'})
}

interface Finding {
    id: string;
    content: string;
    session_id: string;
    session_title: string;
    tags: string[];
    created_label: string;
}

interface ToolInfo {
    name: string;
    description: string;
    category: string;
    source: string;
}

interface PinnedMemory {
    fact: string;
    native_fact: string;
    category: string;
}

interface LLMStatus {
    backend: string;
    hot_messages: number;
    warm_summaries: number;
    session_id: string;
    prompt_tokens: number;
    output_tokens: number;
}

interface MCPProfile {
    name: string;
    binary: string;
    profile_path: string;
    enabled: boolean;
}

interface BackendBudget {
    hot_token_limit: number;
    max_context_tokens: number;
    max_warm_tokens: number;
    max_tool_result_tokens: number;
}

interface Settings {
    default_backend: string;
    local_endpoint: string;
    local_model: string;
    local_budget: BackendBudget;
    vertex_project: string;
    vertex_region: string;
    vertex_model: string;
    vertex_budget: BackendBudget;
    theme: string;
    location: string;
    mcp_profiles: MCPProfile[];
    disabled_tools: string[];
    mitl_overrides: Record<string, boolean>;
    memory_use_v2: boolean;
    sandbox: SandboxSettings;
}

interface SandboxSettings {
    enabled: boolean;
    engine: string;
    image: string;
    network: boolean;
    cpu_limit: string;
    memory_limit: string;
    timeout_seconds: number;
}

type SidebarPanel = 'sessions' | 'status' | 'objects';

interface ObjectInfo {
    id: string;
    type: string;
    mime_type: string;
    orig_name: string;
    created_at: string;
    session_id: string;
    size: number;
}

function formatSize(bytes: number): string {
    if (bytes < 1024) return bytes + ' B'
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
    return (bytes / (1024 * 1024)).toFixed(1) + ' MB'
}

// MessageItem renders a single message. Memoized so that pushing a new message
// (e.g. tool-event) does not re-render the entire history — important when the
// history contains huge ReactMarkdown blocks that are slow to re-parse.
interface MessageItemProps {
    msg: ChatMessage;
    onLightbox: (url: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
}

const MessageItem = memo(function MessageItem({msg, onLightbox, onExpandReport}: MessageItemProps) {
    const components = useMemo(() => ({
        img: ({src, alt}: {src?: string; alt?: string}) => {
            if (src?.startsWith('object:')) {
                const id = src.slice(7)
                return <ObjectImage id={id} alt={alt || ''} onClick={onLightbox} />
            }
            return <img src={src} alt={alt || ''} className="message-image" onClick={() => src && onLightbox(src)} />
        },
    }), [onLightbox])

    if (msg.role === 'tool-event') {
        return (
            <div className={`tool-event ${msg.status === 'running' ? 'running' : 'done'}`}>
                <span className="tool-event-icon">{msg.status === 'running' ? '\u25CF' : '\u2713'}</span>
                <span className="tool-event-name">{msg.content}</span>
            </div>
        )
    }
    if (msg.role === 'summary') {
        return (
            <div className="summary-block">
                <div className="summary-block-header">Summarized earlier turns</div>
                <div className="summary-block-body">
                    <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform} components={components}>
                        {msg.content}
                    </ReactMarkdown>
                </div>
            </div>
        )
    }
    if (msg.role === 'report') {
        const title = msg.content.split('\n')[0].replace(/^#\s*/, '')
        return (
            <div className="report-container">
                <div className="report-header">
                    <span className="report-title">{title}</span>
                    <div className="report-actions">
                        <button onClick={() => onExpandReport({title, content: msg.content})}>Expand</button>
                        <button onClick={(e) => { navigator.clipboard.writeText(msg.content); const b = e.currentTarget; b.textContent = 'Copied!'; setTimeout(() => b.textContent = 'Copy', 1000) }}>Copy</button>
                        <button onClick={() => window.go?.main.Bindings.SaveReport(msg.content, 'report.md')}>Save</button>
                    </div>
                </div>
                <div className="report-content" onClick={() => onExpandReport({title, content: msg.content})}>
                    <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform} components={components}>
                        {msg.content}
                    </ReactMarkdown>
                </div>
            </div>
        )
    }
    return (
        <>
            <div className="message-header">
                <span className="message-role">{msg.role}</span>
                <span className="message-time">{msg.timestamp || ''}</span>
            </div>
            {msg.imageUrls && msg.imageUrls.length > 0 && (
                <div className="message-images">
                    {msg.imageUrls.map((url, j) => (
                        <img key={j} src={url} alt="" className="message-image" onClick={() => onLightbox(url)} />
                    ))}
                </div>
            )}
            <div className="message-content">
                <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform} components={components}>
                    {msg.content}
                </ReactMarkdown>
            </div>
            <div className="message-footer">
                <button className="message-copy" onClick={(e) => {
                    navigator.clipboard.writeText(msg.content)
                    const b = e.currentTarget; b.classList.add('copied')
                    setTimeout(() => b.classList.remove('copied'), 1000)
                }} title="Copy">
                    <span className="copy-icon">{'\u2398'}</span>
                    <span className="copy-check">{'\u2713'}</span>
                </button>
            </div>
        </>
    )
})

// BulkActions renders the small toolbar above a selectable list.
// Delete uses a two-click confirm pattern (the Wails webview may not
// surface native window.confirm dialogs reliably, so we keep the
// confirmation in-component).
//
// onPrepareConfirm, if provided, is awaited on the first click and its
// returned string overrides the confirming-state button text — used by
// the Objects panel to surface "N still referenced" before the user
// commits to deletion.
function BulkActions({total, selectedCount, onSelectAll, onClear, onDelete, onPrepareConfirm}: {
    total: number;
    selectedCount: number;
    onSelectAll: () => void;
    onClear: () => void;
    onDelete: () => void;
    onPrepareConfirm?: () => Promise<string>;
}) {
    const [confirming, setConfirming] = useState(false)
    const [confirmLabel, setConfirmLabel] = useState('Confirm')
    useEffect(() => {
        if (!confirming) return
        const t = setTimeout(() => setConfirming(false), 6000)
        return () => clearTimeout(t)
    }, [confirming])
    useEffect(() => { if (selectedCount === 0) setConfirming(false) }, [selectedCount])

    if (total === 0) return null
    const allSelected = selectedCount === total && total > 0
    return (
        <div className="bulk-actions">
            {selectedCount > 0 ? (
                <>
                    <span className="bulk-count">{selectedCount} selected</span>
                    <button
                        className={`bulk-btn bulk-btn-danger ${confirming ? 'confirming' : ''}`}
                        onClick={async () => {
                            if (confirming) { onDelete(); setConfirming(false); return }
                            if (onPrepareConfirm) {
                                const label = await onPrepareConfirm()
                                setConfirmLabel(label || 'Confirm')
                            } else {
                                setConfirmLabel('Confirm')
                            }
                            setConfirming(true)
                        }}
                        title={confirming ? `Click again to delete ${selectedCount} item(s)` : `Delete ${selectedCount} selected`}
                    >
                        {confirming ? confirmLabel : 'Delete'}
                    </button>
                    <button className="bulk-btn" onClick={onClear}>Clear</button>
                </>
            ) : (
                <button className="bulk-btn" onClick={onSelectAll} disabled={allSelected}>Select all</button>
            )}
        </div>
    )
}

function BackendBudgetEditor({budget, onChange}: {budget: BackendBudget; onChange: (b: BackendBudget) => void}) {
    const num = (v: string) => Math.max(0, parseInt(v, 10) || 0)
    return (
        <div className="budget-editor">
            <label>
                <span>Hot Token Limit (compaction trigger)</span>
                <input type="number" min={0} value={budget.hot_token_limit} onChange={e => onChange({...budget, hot_token_limit: num(e.target.value)})} />
            </label>
            <label>
                <span>Max Context Tokens (0 = unlimited)</span>
                <input type="number" min={0} value={budget.max_context_tokens} onChange={e => onChange({...budget, max_context_tokens: num(e.target.value)})} />
            </label>
            <label>
                <span>Max Warm Summary Tokens</span>
                <input type="number" min={0} value={budget.max_warm_tokens} onChange={e => onChange({...budget, max_warm_tokens: num(e.target.value)})} />
            </label>
            <label>
                <span>Max Tool-Result Tokens (per call)</span>
                <input type="number" min={0} value={budget.max_tool_result_tokens} onChange={e => onChange({...budget, max_tool_result_tokens: num(e.target.value)})} />
            </label>
        </div>
    )
}

function App() {
    const [state, setState] = useState<'idle' | 'busy'>('idle')
    const [messages, setMessages] = useState<ChatMessage[]>([])
    const [streaming, setStreaming] = useState('')
    const [backend, setBackend] = useState('')
    const [sidebarPanel, setSidebarPanel] = useState<SidebarPanel>('sessions')
    const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
    const [sidebarWidth, setSidebarWidth] = useState(280)
    const resizingRef = useRef(false)
    const [sessions, setSessions] = useState<SessionInfo[]>([])
    const [currentSessionId, setCurrentSessionId] = useState<string>('')
    const [findings, setFindings] = useState<Finding[]>([])
    const [showSettings, setShowSettings] = useState(false)
    const [editingSession, setEditingSession] = useState<string | null>(null)
    const [editTitle, setEditTitle] = useState('')
    const [mitlRequest, setMitlRequest] = useState<{tool_name: string; arguments: string; category: string} | null>(null)
    const [mitlFeedback, setMitlFeedback] = useState('')
    const [cmdResult, setCmdResult] = useState<string | null>(null)
    const [tools, setTools] = useState<ToolInfo[]>([])
    const [mcpStatus, setMcpStatus] = useState<{name: string; status: string; tool_count: number; error?: string}[]>([])
    const [pinnedMemories, setPinnedMemories] = useState<PinnedMemory[]>([])
    const [selectedFindingIds, setSelectedFindingIds] = useState<Set<string>>(new Set())
    const [selectedPinnedKeys, setSelectedPinnedKeys] = useState<Set<string>>(new Set())
    const [objects, setObjects] = useState<ObjectInfo[]>([])
    const [selectedObjectIds, setSelectedObjectIds] = useState<Set<string>>(new Set())
    const [confirmingObjectDelete, setConfirmingObjectDelete] = useState<string | null>(null)
    const [llmStatus, setLLMStatus] = useState<LLMStatus | null>(null)
    const [lightboxImage, setLightboxImage] = useState<string | null>(null)
    const [expandedReport, setExpandedReport] = useState<{title: string; content: string} | null>(null)
    const [settings, setSettings] = useState<Settings | null>(null)
    const [settingsTab, setSettingsTab] = useState<'general' | 'tools' | 'mcp'>('general')
    const [progressTool, setProgressTool] = useState('')
    const messagesEndRef = useRef<HTMLDivElement>(null)
    const composingRef = useRef(false)

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
                    setMessages(prev => {
                        let idx = -1
                        for (let i = prev.length - 1; i >= 0; i--) {
                            const m = prev[i]
                            if (m.role === 'tool-event' && m.status === 'running' && m.content === data.detail) { idx = i; break }
                        }
                        if (idx === -1) return prev
                        const next = prev.slice()
                        next[idx] = {...next[idx], status: 'done'}
                        return next
                    })
                } else if (data.type === 'tool_start') {
                    setProgressTool(data.detail || '')
                    setMessages(prev => [...prev, {role: 'tool-event', content: data.detail || '', status: 'running', timestamp: nowTime()}])
                } else if (data.type === 'thinking') {
                    setProgressTool(data.detail || '')
                }
            })
            const cleanupPinned = window.runtime.EventsOn('pinned:updated', () => {
                if (window.go) window.go.main.Bindings.GetPinnedMemories().then(setPinnedMemories)
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
            return () => { cleanupStream(); cleanupActivity(); cleanupPinned(); cleanupReport(); cleanupMitl(); cleanupTitle() }
        }
    }, [])

    useEffect(() => {
        messagesEndRef.current?.scrollIntoView({behavior: 'smooth'})
    }, [messages, streaming])

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
        if (window.go && state === 'idle') {
            const id = await window.go.main.Bindings.NewSession()
            setCurrentSessionId(id)
            setMessages([])
            setStreaming('')
            await refreshSessions()
        }
    }, [state, refreshSessions])

    const handleLoadSession = useCallback(async (id: string) => {
        if (window.go && state === 'idle') {
            clearObjectCache() // clear image cache on session switch
            const msgs = await window.go.main.Bindings.LoadSession(id)
            setCurrentSessionId(id)
            setMessages((msgs || []).map(m => ({
                role: m.role as ChatMessage['role'],
                content: m.content,
                timestamp: m.timestamp,
            })))
            setStreaming('')
        }
    }, [state])

    const handleDeleteSession = useCallback(async (id: string) => {
        if (window.go && state === 'idle') {
            await window.go.main.Bindings.DeleteSession(id)
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
                    setMessages((msgs || []).map(m => ({
                        role: m.role as ChatMessage['role'],
                        content: m.content,
                        timestamp: m.timestamp,
                    })))
                }
            }
        }
    }, [state, currentSessionId])

    const startRename = useCallback((id: string, currentTitle: string) => {
        setEditingSession(id)
        setEditTitle(currentTitle)
    }, [])

    const commitRename = useCallback(async () => {
        if (!editingSession || !editTitle.trim()) {
            setEditingSession(null)
            return
        }
        if (window.go) {
            await window.go.main.Bindings.RenameSession(editingSession, editTitle.trim())
            await refreshSessions()
        }
        setEditingSession(null)
    }, [editingSession, editTitle, refreshSessions])

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
                setMessages((msgs || []).map(m => ({
                    role: m.role as ChatMessage['role'],
                    content: m.content,
                    timestamp: m.timestamp,
                })))
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
        if (sidebarPanel === 'status' && window.go) {
            refreshFindings()
            window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            window.go.main.Bindings.GetPinnedMemories().then(setPinnedMemories)
        }
        if (sidebarPanel === 'objects' && window.go) {
            window.go.main.Bindings.ListObjects().then(setObjects)
        }
    }, [sidebarPanel, refreshFindings])

    // Auto-save settings on change
    const updateSetting = useCallback(async (patch: Partial<Settings>) => {
        if (!settings) return
        const updated = {...settings, ...patch}
        setSettings(updated)
        if (window.go) {
            await window.go.main.Bindings.SaveSettings(updated)
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

    const canChat = state === 'idle' && currentSessionId !== ''

    const handleSend = useCallback(async (text: string, images: string[]) => {
        if ((!text && images.length === 0) || state === 'busy' || !currentSessionId) return

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
            // Mark any leftover running tool-events as done (e.g. on error or abort)
            setMessages(prev => prev.map(m => m.role === 'tool-event' && m.status === 'running' ? {...m, status: 'done'} : m))
            if (window.go) {
                window.go.main.Bindings.GetBackend().then(setBackend)
                window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            }
        }
    }, [state, currentSessionId])

    function startResize(e: React.MouseEvent) {
        e.preventDefault()
        resizingRef.current = true
        const startX = e.clientX
        const startW = sidebarWidth
        function onMove(ev: MouseEvent) {
            if (!resizingRef.current) return
            setSidebarWidth(Math.max(180, Math.min(500, startW + ev.clientX - startX)))
        }
        function onUp() {
            resizingRef.current = false
            document.removeEventListener('mousemove', onMove)
            document.removeEventListener('mouseup', onUp)
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
            {sidebarCollapsed && (
                <div className="sidebar-collapsed">
                    <div className="sidebar-collapsed-top">
                        <button className="sidebar-nav-btn" onClick={() => { setSidebarCollapsed(false); setSidebarPanel('sessions') }} title="Sessions">
                            <span className="sidebar-nav-ic">&#x2630;</span>
                        </button>
                    </div>
                    <div className="sidebar-collapsed-bottom">
                        <button className="sidebar-nav-btn" onClick={handleNewSession} disabled={state === 'busy'} title="New Chat">
                            <span className="sidebar-nav-ic">+</span>
                        </button>
                        <div className="sidebar-nav-divider" />
                        <button className="sidebar-nav-btn" onClick={() => { setSidebarCollapsed(false); setSidebarPanel('status') }} title="Status">
                            <span className="sidebar-nav-ic">&#x2261;</span>
                        </button>
                        <button className="sidebar-nav-btn" onClick={() => { setSidebarCollapsed(false); setSidebarPanel('objects') }} title="Objects">
                            <span className="sidebar-nav-ic">&#x25A3;</span>
                        </button>
                        <button className="sidebar-nav-btn" onClick={() => { setSidebarCollapsed(false); openSettings() }} title="Settings">
                            <span className="sidebar-nav-ic">&#x2699;</span>
                        </button>
                        <div className="sidebar-nav-divider" />
                        <button className="sidebar-nav-btn" onClick={() => setSidebarCollapsed(false)} title="Expand sidebar">
                            <span className="sidebar-nav-ic">&#x25B6;</span>
                        </button>
                    </div>
                </div>
            )}
            <div className="sidebar" style={{width: sidebarCollapsed ? 0 : sidebarWidth, display: sidebarCollapsed ? 'none' : undefined}}>
                <div className="sidebar-top">
                    <button className="sidebar-nav-btn active" onClick={() => setSidebarPanel('sessions')}>
                        <span className="sidebar-nav-ic">{sidebarPanel === 'sessions' ? '\u2630' : '\u2261'}</span> {sidebarPanel === 'sessions' ? 'Sessions' : 'Status'}
                    </button>
                </div>
                <div className="sidebar-panel">
                    {sidebarPanel === 'sessions' && (<>
                        {sessions.length === 0 ? (
                            <p className="sidebar-hint">No sessions yet</p>
                        ) : sessions.map(s => (
                            <div key={s.id} className={`session-item ${s.id === currentSessionId ? 'active' : ''}`} onClick={() => handleLoadSession(s.id)}>
                                {editingSession === s.id ? (
                                    <input
                                        className="session-title-edit"
                                        value={editTitle}
                                        onChange={e => setEditTitle(e.target.value)}
                                        onBlur={commitRename}
                                        onKeyDown={e => {
                                            if (e.key === 'Enter' && !composingRef.current) commitRename()
                                            if (e.key === 'Escape') setEditingSession(null)
                                        }}
                                        onCompositionStart={() => { composingRef.current = true }}
                                        onCompositionEnd={() => { setTimeout(() => { composingRef.current = false }, 50) }}
                                        autoFocus
                                        onClick={e => e.stopPropagation()}
                                    />
                                ) : (
                                    <div className="session-title" onDoubleClick={(e) => { e.stopPropagation(); startRename(s.id, s.title) }}>{s.title}</div>
                                )}
                                <div className="session-meta">
                                    <span className="session-date">{s.updated_at}</span>
                                    <div className="session-actions">
                                        <button onClick={(e) => { e.stopPropagation(); startRename(s.id, s.title) }} title="Rename">&#x270E;</button>
                                        <button onClick={(e) => { e.stopPropagation(); handleDeleteSession(s.id) }} title="Delete">&#x2715;</button>
                                    </div>
                                </div>
                            </div>
                        ))}
                    </>)}

                    {sidebarPanel === 'status' && (<>
                        {findings.length > 0 && (
                            <div className={`status-section ${selectedFindingIds.size > 0 ? 'bulk-active' : ''}`}>
                                <div className="bulk-section-header">
                                    <h3>Findings</h3>
                                    <BulkActions
                                        total={findings.length}
                                        selectedCount={selectedFindingIds.size}
                                        onSelectAll={() => setSelectedFindingIds(new Set(findings.map(f => f.id)))}
                                        onClear={() => setSelectedFindingIds(new Set())}
                                        onDelete={async () => {
                                            const ids = Array.from(selectedFindingIds)
                                            if (ids.length === 0) return
                                            await window.go.main.Bindings.DeleteFindings(ids)
                                            setSelectedFindingIds(new Set())
                                            const updated = await window.go.main.Bindings.GetFindings()
                                            setFindings(updated)
                                        }}
                                    />
                                </div>
                                {findings.map(f => (
                                    <div key={f.id} className={`finding-card ${selectedFindingIds.has(f.id) ? 'selected' : ''}`}>
                                        <input
                                            type="checkbox"
                                            className="bulk-check"
                                            checked={selectedFindingIds.has(f.id)}
                                            onChange={e => {
                                                const next = new Set(selectedFindingIds)
                                                if (e.target.checked) next.add(f.id); else next.delete(f.id)
                                                setSelectedFindingIds(next)
                                            }}
                                        />
                                        <div className="finding-body">
                                            <div className="finding-content">{f.content}</div>
                                            <div className="finding-meta">
                                                <span className="finding-date">{f.created_label}</span>
                                                {f.session_title && (
                                                    <span className="finding-origin" title={`Session: ${f.session_id}`}>{f.session_title}</span>
                                                )}
                                            </div>
                                            {f.tags && f.tags.length > 0 && (
                                                <div className="finding-tags">
                                                    {f.tags.map(tag => {
                                                        const sevClass = ['critical','high','medium','low','info'].includes(tag) ? ` severity-${tag}` : ''
                                                        return <span key={tag} className={`tag${sevClass}`}>{tag}</span>
                                                    })}
                                                </div>
                                            )}
                                        </div>
                                    </div>
                                ))}
                            </div>
                        )}
                        <div className={`status-section ${selectedPinnedKeys.size > 0 ? 'bulk-active' : ''}`}>
                            <div className="bulk-section-header">
                                <h3>Pinned Memory</h3>
                                {pinnedMemories.length > 0 && (
                                    <BulkActions
                                        total={pinnedMemories.length}
                                        selectedCount={selectedPinnedKeys.size}
                                        onSelectAll={() => setSelectedPinnedKeys(new Set(pinnedMemories.map(p => p.fact)))}
                                        onClear={() => setSelectedPinnedKeys(new Set())}
                                        onDelete={async () => {
                                            const keys = Array.from(selectedPinnedKeys)
                                            if (keys.length === 0) return
                                            await window.go.main.Bindings.DeletePinnedMemories(keys)
                                            setSelectedPinnedKeys(new Set())
                                            const updated = await window.go.main.Bindings.GetPinnedMemories()
                                            setPinnedMemories(updated)
                                        }}
                                    />
                                )}
                            </div>
                            {pinnedMemories.length === 0 ? (
                                <p className="sidebar-hint">No pinned facts</p>
                            ) : pinnedMemories.map((p, i) => (
                                <div key={i} className={`pinned-item ${selectedPinnedKeys.has(p.fact) ? 'selected' : ''}`}>
                                    <input
                                        type="checkbox"
                                        className="bulk-check"
                                        checked={selectedPinnedKeys.has(p.fact)}
                                        onChange={e => {
                                            const next = new Set(selectedPinnedKeys)
                                            if (e.target.checked) next.add(p.fact); else next.delete(p.fact)
                                            setSelectedPinnedKeys(next)
                                        }}
                                    />
                                    <span className={`pinned-category ${p.category}`}>{p.category}</span>
                                    <div className="pinned-content">
                                        <span className="pinned-fact">{p.native_fact || p.fact}</span>
                                        {p.native_fact && p.native_fact !== p.fact && (
                                            <span className="pinned-fact-en">{p.fact}</span>
                                        )}
                                    </div>
                                    <button className="pinned-delete" onClick={async () => {
                                        await window.go.main.Bindings.DeletePinnedMemory(p.fact)
                                        const updated = await window.go.main.Bindings.GetPinnedMemories()
                                        setPinnedMemories(updated)
                                    }}>&#x2715;</button>
                                </div>
                            ))}
                        </div>
                        {llmStatus && (
                            <div className="status-section">
                                <h3>Tokens</h3>
                                <div className="status-row"><span>Hot messages</span><span>{llmStatus.hot_messages}</span></div>
                                <div className="status-row"><span>Warm summaries</span><span>{llmStatus.warm_summaries}</span></div>
                                <div className="status-row"><span>Prompt tokens</span><span>{llmStatus.prompt_tokens.toLocaleString()}</span></div>
                                <div className="status-row"><span>Output tokens</span><span>{llmStatus.output_tokens.toLocaleString()}</span></div>
                            </div>
                        )}
                    </>)}
                    {sidebarPanel === 'objects' && (
                        <div className={`status-section ${selectedObjectIds.size > 0 ? 'bulk-active' : ''}`}>
                            <div className="bulk-section-header">
                                <h3>Objects ({objects.length})</h3>
                                <BulkActions
                                    total={objects.length}
                                    selectedCount={selectedObjectIds.size}
                                    onSelectAll={() => setSelectedObjectIds(new Set(objects.map(o => o.id)))}
                                    onClear={() => setSelectedObjectIds(new Set())}
                                    onPrepareConfirm={async () => {
                                        const ids = Array.from(selectedObjectIds)
                                        const refs = await window.go.main.Bindings.ObjectReferences(ids)
                                        const referenced = ids.filter(id => (refs[id] || 0) > 0)
                                        if (referenced.length === 0) return `Confirm delete ${ids.length}`
                                        return `${referenced.length}/${ids.length} still in use — confirm`
                                    }}
                                    onDelete={async () => {
                                        const ids = Array.from(selectedObjectIds)
                                        if (ids.length === 0) return
                                        await window.go.main.Bindings.DeleteObjects(ids)
                                        setSelectedObjectIds(new Set())
                                        const updated = await window.go.main.Bindings.ListObjects()
                                        setObjects(updated)
                                        clearObjectCache()
                                    }}
                                />
                            </div>
                            {objects.length === 0 ? (
                                <p className="sidebar-hint">No objects stored yet</p>
                            ) : objects.map(o => (
                                <div key={o.id} className={`object-item ${selectedObjectIds.has(o.id) ? 'selected' : ''}`}>
                                    <input
                                        type="checkbox"
                                        className="bulk-check"
                                        checked={selectedObjectIds.has(o.id)}
                                        onChange={e => {
                                            const next = new Set(selectedObjectIds)
                                            if (e.target.checked) next.add(o.id); else next.delete(o.id)
                                            setSelectedObjectIds(next)
                                        }}
                                    />
                                    <div className="object-thumb">
                                        {o.type === 'image' ? (
                                            <ObjectImage id={o.id} alt={o.orig_name || o.id} onClick={async () => {
                                                const url = await window.go.main.Bindings.GetImageDataURL(o.id)
                                                setLightboxImage(url)
                                            }} />
                                        ) : o.type === 'report' ? (
                                            <button
                                                className="object-icon type-report object-icon-btn"
                                                title="Open report"
                                                onClick={async () => {
                                                    try {
                                                        const text = await window.go.main.Bindings.GetObjectText(o.id)
                                                        const title = (text.split('\n')[0] || '').replace(/^#\s*/, '') || o.orig_name || o.id
                                                        setExpandedReport({title, content: text})
                                                    } catch {}
                                                }}
                                            >&#x1F4C4;</button>
                                        ) : (
                                            <span className={`object-icon type-${o.type}`}>&#x25A1;</span>
                                        )}
                                    </div>
                                    <div className="object-meta">
                                        <div className="object-id">{o.id}</div>
                                        <div className="object-line"><span className={`object-type type-${o.type}`}>{o.type}</span> · <span>{formatSize(o.size)}</span> · <span>{o.created_at}</span></div>
                                        {o.session_id && <div className="object-line object-session">session: {o.session_id}</div>}
                                        {o.orig_name && <div className="object-line object-orig">{o.orig_name}</div>}
                                    </div>
                                    <div className="object-actions">
                                        <button className="object-action" title="Export to file" onClick={async () => {
                                            try { await window.go.main.Bindings.ExportObject(o.id) } catch {}
                                        }}>&#x2913;</button>
                                        <button
                                            className={`object-action object-delete ${confirmingObjectDelete === o.id ? 'confirming' : ''}`}
                                            title={confirmingObjectDelete === o.id ? 'Click again to delete' : 'Delete object'}
                                            onClick={async () => {
                                                if (confirmingObjectDelete === o.id) {
                                                    await window.go.main.Bindings.DeleteObject(o.id)
                                                    setConfirmingObjectDelete(null)
                                                    const updated = await window.go.main.Bindings.ListObjects()
                                                    setObjects(updated)
                                                    clearObjectCache()
                                                    return
                                                }
                                                const refs = await window.go.main.Bindings.ObjectReferences([o.id])
                                                const used = refs[o.id] || 0
                                                if (used === 0) {
                                                    await window.go.main.Bindings.DeleteObject(o.id)
                                                    const updated = await window.go.main.Bindings.ListObjects()
                                                    setObjects(updated)
                                                    clearObjectCache()
                                                } else {
                                                    setConfirmingObjectDelete(o.id)
                                                    setTimeout(() => setConfirmingObjectDelete(prev => prev === o.id ? null : prev), 6000)
                                                }
                                            }}
                                        >{confirmingObjectDelete === o.id ? '!' : '\u2715'}</button>
                                    </div>
                                </div>
                            ))}
                        </div>
                    )}
                </div>

                <div className="sidebar-bottom">
                    <button className="sidebar-nav-btn" onClick={handleNewSession} disabled={state === 'busy'}>
                        <span className="sidebar-nav-ic">+</span> New Chat
                    </button>
                    <div className="sidebar-nav-divider" />
                    <button className={`sidebar-nav-btn ${sidebarPanel === 'status' ? 'active' : ''}`} onClick={() => setSidebarPanel(sidebarPanel === 'status' ? 'sessions' : 'status')}>
                        <span className="sidebar-nav-ic">&#x2261;</span> Status
                    </button>
                    <button className={`sidebar-nav-btn ${sidebarPanel === 'objects' ? 'active' : ''}`} onClick={() => setSidebarPanel(sidebarPanel === 'objects' ? 'sessions' : 'objects')}>
                        <span className="sidebar-nav-ic">&#x25A3;</span> Objects
                    </button>
                    <button className="sidebar-nav-btn" onClick={openSettings}>
                        <span className="sidebar-nav-ic">&#x2699;</span> Settings
                    </button>
                    <div className="sidebar-nav-divider" />
                    <button className="sidebar-nav-btn" onClick={() => setSidebarCollapsed(true)} title="Collapse sidebar">
                        <span className="sidebar-nav-ic">&#x25C0;</span>
                    </button>
                </div>
            </div>
            {!sidebarCollapsed && <div className="sidebar-resize" onMouseDown={startResize} />}
            <div className="main">
                <div className="messages">
                    {messages.filter(msg => msg.role !== 'tool').map((msg, i) => (
                        <div key={i} className={`message ${msg.role}`}>
                            <MessageItem msg={msg} onLightbox={setLightboxImage} onExpandReport={setExpandedReport} />
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
                    {llmStatus && (
                        <span className="status-tokens">{llmStatus.prompt_tokens.toLocaleString()} in / {llmStatus.output_tokens.toLocaleString()} out</span>
                    )}
                    {state === 'busy' && <span className="tool-progress">{progressTool || 'Thinking...'}</span>}
                </div>
                {state === 'busy' ? (
                    <div className="input-area">
                        <div className="input-row">
                            <textarea disabled rows={3} placeholder="Agent is busy..." />
                            <button className="abort-btn" onClick={handleAbort}>Abort</button>
                        </div>
                    </div>
                ) : (
                    <ChatInput onSend={handleSend} disabled={!canChat} />
                )}
            </div>

            {mitlRequest && (
                <div className="mitl-dialog">
                    <div className="mitl-header">
                        <span className="mitl-icon">&#9888;</span>
                        <span>{mitlRequest.category === 'sql_preview' ? 'SQL Execution Preview'
                            : mitlRequest.category === 'analysis_plan' ? 'Analysis Plan Confirmation'
                            : 'Tool Approval Required'}</span>
                    </div>
                    <div className="mitl-body">
                        {(() => {
                            try {
                                const args = JSON.parse(mitlRequest.arguments)
                                if (mitlRequest.category === 'sql_preview') {
                                    return (<>
                                        <div className="mitl-section">
                                            <span className="mitl-label">SQL:</span>
                                            <pre className="mitl-sql">{args.sql || mitlRequest.arguments}</pre>
                                        </div>
                                    </>)
                                }
                                if (mitlRequest.category === 'analysis_plan') {
                                    return (<>
                                        <div className="mitl-section">
                                            <span className="mitl-label">Analysis Perspective:</span>
                                            <div className="mitl-perspective">{args.prompt}</div>
                                        </div>
                                        {args.table && (
                                            <div className="mitl-section">
                                                <span className="mitl-label">Target Table:</span>
                                                <code>{args.table}</code>
                                            </div>
                                        )}
                                    </>)
                                }
                                // Default: shell tools etc.
                                return (<>
                                    <div className="mitl-tool-name">
                                        <span className="mitl-label">Tool:</span>
                                        <code>{mitlRequest.tool_name}</code>
                                        <span className={`tool-category ${mitlRequest.category}`}>{mitlRequest.category}</span>
                                    </div>
                                    <div className="mitl-section">
                                        <span className="mitl-label">Arguments:</span>
                                        <pre>{JSON.stringify(args, null, 2)}</pre>
                                    </div>
                                </>)
                            } catch {
                                return (<div className="mitl-section">
                                    <span className="mitl-label">Details:</span>
                                    <pre>{mitlRequest.arguments}</pre>
                                </div>)
                            }
                        })()}
                        <div className="mitl-feedback">
                            <input
                                type="text"
                                placeholder="Rejection reason / revision suggestion (optional)"
                                value={mitlFeedback}
                                onChange={e => setMitlFeedback(e.target.value)}
                                onKeyDown={e => {
                                    if (e.key === 'Enter' && mitlFeedback.trim()) {
                                        setMitlRequest(null)
                                        window.go.main.Bindings.RejectMITLWithFeedback(mitlFeedback.trim())
                                        setMitlFeedback('')
                                    }
                                }}
                            />
                        </div>
                    </div>
                    <div className="mitl-actions">
                        <button className="mitl-approve" onClick={() => { setMitlRequest(null); setMitlFeedback(''); window.go.main.Bindings.ApproveMITL() }}>Approve</button>
                        <button className="mitl-reject" onClick={() => {
                            setMitlRequest(null)
                            if (mitlFeedback.trim()) {
                                window.go.main.Bindings.RejectMITLWithFeedback(mitlFeedback.trim())
                            } else {
                                window.go.main.Bindings.RejectMITL()
                            }
                            setMitlFeedback('')
                        }}>Reject</button>
                    </div>
                </div>
            )}

            {showSettings && settings && (
                <div className="settings-overlay" onClick={() => setShowSettings(false)}>
                    <div className="settings-modal" onClick={e => e.stopPropagation()}>
                        <div className="settings-header">
                            <h2>Settings</h2>
                            <button className="settings-close" onClick={() => setShowSettings(false)}>&#x2715;</button>
                        </div>
                        <div className="settings-tabs">
                            <button className={settingsTab === 'general' ? 'active' : ''} onClick={() => setSettingsTab('general')}>General</button>
                            <button className={settingsTab === 'tools' ? 'active' : ''} onClick={() => setSettingsTab('tools')}>Tools</button>
                            <button className={settingsTab === 'mcp' ? 'active' : ''} onClick={() => setSettingsTab('mcp')}>MCP</button>
                        </div>
                        <div className="settings-body">
                            {settingsTab === 'general' && (<>
                                <div className="settings-section">
                                    <h3>General</h3>
                                    <label>
                                        <span>Default Backend</span>
                                        <select value={settings.default_backend} onChange={e => updateSetting({default_backend: e.target.value})}>
                                            <option value="local">Local LLM</option>
                                            <option value="vertex_ai">Vertex AI</option>
                                        </select>
                                    </label>
                                    <label>
                                        <span>Theme</span>
                                        <select value={settings.theme || 'dark'} onChange={e => {
                                            updateSetting({theme: e.target.value})
                                            document.documentElement.setAttribute('data-theme', e.target.value)
                                        }}>
                                            <option value="dark">Dark</option>
                                            <option value="light">Light</option>
                                            <option value="warm">Warm</option>
                                            <option value="midnight">Midnight</option>
                                        </select>
                                    </label>
                                    <label>
                                        <span>Location</span>
                                        <input value={settings.location || ''} placeholder="e.g. Tokyo, Japan" onChange={e => updateSetting({location: e.target.value})} />
                                    </label>
                                </div>
                                <div className="settings-section">
                                    <h3>Memory</h3>
                                    <label>
                                        <input type="checkbox" checked={!!settings.memory_use_v2} onChange={e => updateSetting({memory_use_v2: e.target.checked})} />
                                        <span>Use v2 context builder (experimental)</span>
                                    </label>
                                    <p className="sidebar-hint">Records stay immutable; older context is summarized on demand and cached. Time-range markers added for LLM temporal awareness. See docs/en/memory-architecture-v2.md.</p>
                                </div>
                                <div className="settings-section">
                                    <h3>Sandbox (experimental)</h3>
                                    <label>
                                        <input type="checkbox" checked={!!settings.sandbox?.enabled} onChange={e => updateSetting({sandbox: {...settings.sandbox, enabled: e.target.checked}})} />
                                        <span>Enable container sandbox (podman/docker required, restart to take effect)</span>
                                    </label>
                                    <p className="sidebar-hint">When enabled, exposes six sandbox-* tools that run shell/Python inside a per-session container. /work is mounted from the session's data dir. See docs/en/sandbox-execution.md.</p>
                                    {settings.sandbox?.enabled && (
                                        <>
                                            <label>
                                                <span>Engine</span>
                                                <select value={settings.sandbox.engine || 'auto'} onChange={e => updateSetting({sandbox: {...settings.sandbox, engine: e.target.value}})}>
                                                    <option value="auto">auto (podman → docker)</option>
                                                    <option value="podman">podman</option>
                                                    <option value="docker">docker</option>
                                                </select>
                                            </label>
                                            <label>
                                                <span>Image</span>
                                                <input value={settings.sandbox.image || ''} placeholder="python:3.12-slim" onChange={e => updateSetting({sandbox: {...settings.sandbox, image: e.target.value}})} />
                                            </label>
                                            <label>
                                                <input type="checkbox" checked={!!settings.sandbox.network} onChange={e => updateSetting({sandbox: {...settings.sandbox, network: e.target.checked}})} />
                                                <span>Allow network egress (default off)</span>
                                            </label>
                                            <label>
                                                <span>CPU limit</span>
                                                <input value={settings.sandbox.cpu_limit || ''} placeholder="2" onChange={e => updateSetting({sandbox: {...settings.sandbox, cpu_limit: e.target.value}})} />
                                            </label>
                                            <label>
                                                <span>Memory limit</span>
                                                <input value={settings.sandbox.memory_limit || ''} placeholder="1g" onChange={e => updateSetting({sandbox: {...settings.sandbox, memory_limit: e.target.value}})} />
                                            </label>
                                            <label>
                                                <span>Per-call timeout (seconds)</span>
                                                <input type="number" min={5} value={settings.sandbox.timeout_seconds || 60} onChange={e => updateSetting({sandbox: {...settings.sandbox, timeout_seconds: parseInt(e.target.value, 10) || 60}})} />
                                            </label>
                                        </>
                                    )}
                                </div>
                                <div className="settings-section">
                                    <h3>Local LLM</h3>
                                    <label>
                                        <span>Endpoint</span>
                                        <input value={settings.local_endpoint} onChange={e => updateSetting({local_endpoint: e.target.value})} />
                                    </label>
                                    <label>
                                        <span>Model</span>
                                        <input value={settings.local_model} onChange={e => updateSetting({local_model: e.target.value})} />
                                    </label>
                                    <BackendBudgetEditor
                                        budget={settings.local_budget}
                                        onChange={b => updateSetting({local_budget: b})}
                                    />
                                </div>
                                <div className="settings-section">
                                    <h3>Vertex AI</h3>
                                    <label>
                                        <span>Project ID</span>
                                        <input value={settings.vertex_project} onChange={e => updateSetting({vertex_project: e.target.value})} />
                                    </label>
                                    <label>
                                        <span>Region</span>
                                        <input value={settings.vertex_region} onChange={e => updateSetting({vertex_region: e.target.value})} />
                                    </label>
                                    <label>
                                        <span>Model</span>
                                        <input value={settings.vertex_model} onChange={e => updateSetting({vertex_model: e.target.value})} />
                                    </label>
                                    <BackendBudgetEditor
                                        budget={settings.vertex_budget}
                                        onChange={b => updateSetting({vertex_budget: b})}
                                    />
                                </div>
                            </>)}
                            {settingsTab === 'tools' && (<>
                                <div className="settings-section">
                                    <h3>Registered Tools</h3>
                                    {tools.length === 0 ? (
                                        <p className="sidebar-hint">No tools available</p>
                                    ) : tools.map(t => {
                                        const disabled = (settings.disabled_tools || []).includes(t.name)
                                        const mitlOverride = (settings.mitl_overrides || {})[t.name]
                                        const mitlDefault = t.category === 'write' || t.category === 'execute' || t.source === 'mcp'
                                        const mitlActive = mitlOverride !== undefined ? mitlOverride : mitlDefault
                                        return (
                                            <div key={t.name} className={`tool-item ${disabled ? 'tool-disabled' : ''}`}>
                                                <div className="tool-name">
                                                    <label className="tool-enabled-toggle" title="Enable/Disable">
                                                        <input type="checkbox" checked={!disabled} onChange={e => {
                                                            const list = (settings.disabled_tools || []).filter(n => n !== t.name)
                                                            if (!e.target.checked) list.push(t.name)
                                                            updateSetting({disabled_tools: list} as any)
                                                        }} />
                                                    </label>
                                                    <code>{t.name}</code>
                                                    <span className={`tool-category ${t.category}`}>{t.category}</span>
                                                    <span className="tool-source">{t.source}</span>
                                                    <label className="tool-mitl-toggle" title="MITL approval">
                                                        <input type="checkbox" checked={mitlActive} onChange={e => {
                                                            const overrides = {...(settings.mitl_overrides || {})}
                                                            if (e.target.checked === mitlDefault) {
                                                                delete overrides[t.name]
                                                            } else {
                                                                overrides[t.name] = e.target.checked
                                                            }
                                                            updateSetting({mitl_overrides: overrides} as any)
                                                        }} />
                                                        <span className="mitl-label-text">MITL</span>
                                                    </label>
                                                </div>
                                                <div className="tool-desc">{t.description}</div>
                                            </div>
                                        )
                                    })}
                                </div>
                            </>)}
                            {settingsTab === 'mcp' && (<>
                                <div className="settings-section">
                                    <h3>MCP Profiles</h3>
                                    {(settings.mcp_profiles || []).length === 0 ? (
                                        <p className="sidebar-hint">No MCP profiles configured</p>
                                    ) : (settings.mcp_profiles || []).map((p, i) => {
                                        const status = mcpStatus.find(s => s.name === p.name)
                                        return (
                                            <div key={i} className="mcp-profile-item">
                                                <div className="mcp-profile-header">
                                                    <span className="mcp-profile-name">{p.name}</span>
                                                    {status && <span className={`mcp-status-badge ${status.status}`}>{status.status}{status.status === 'running' ? ` (${status.tool_count} tools)` : ''}</span>}
                                                    <label className="mcp-toggle">
                                                        <input type="checkbox" checked={p.enabled} onChange={e => {
                                                            const updated = [...(settings.mcp_profiles || [])]
                                                            updated[i] = {...p, enabled: e.target.checked}
                                                            updateSetting({mcp_profiles: updated} as any)
                                                        }} />
                                                        <span>{p.enabled ? 'ON' : 'OFF'}</span>
                                                    </label>
                                                    <button className="mcp-delete" onClick={() => {
                                                        const updated = (settings.mcp_profiles || []).filter((_, j) => j !== i)
                                                        updateSetting({mcp_profiles: updated} as any)
                                                    }}>&#x2715;</button>
                                                </div>
                                                <div className="mcp-profile-detail">
                                                    <span className="mcp-label">Binary:</span> {p.binary}
                                                </div>
                                                <div className="mcp-profile-detail">
                                                    <span className="mcp-label">Profile:</span> {p.profile_path}
                                                </div>
                                                {status?.error && <div className="mcp-status-error">{status.error}</div>}
                                            </div>
                                        )
                                    })}
                                    <div className="mcp-add-form">
                                        <h4>Add Profile</h4>
                                        <label><span>Name</span><input id="mcp-name" placeholder="e.g. github" /></label>
                                        <label><span>Binary</span><input id="mcp-binary" defaultValue="/usr/local/bin/mcp-guardian" /></label>
                                        <label><span>Profile</span><input id="mcp-profile" placeholder="~/.config/mcp-guardian/profiles/xxx.json" /></label>
                                        <button className="mcp-add-btn" onClick={() => {
                                            const name = (document.getElementById('mcp-name') as HTMLInputElement).value.trim()
                                            const binary = (document.getElementById('mcp-binary') as HTMLInputElement).value.trim()
                                            const profile = (document.getElementById('mcp-profile') as HTMLInputElement).value.trim()
                                            if (!name || !binary || !profile) return
                                            const updated = [...(settings.mcp_profiles || []), {name, binary, profile_path: profile, enabled: true}]
                                            updateSetting({mcp_profiles: updated} as any);
                                            (document.getElementById('mcp-name') as HTMLInputElement).value = '';
                                            (document.getElementById('mcp-profile') as HTMLInputElement).value = ''
                                        }}>Add</button>
                                    </div>
                                    <button className="mcp-restart-btn" style={{marginTop: 16}} onClick={async () => {
                                        if (window.go) {
                                            await window.go.main.Bindings.RestartMCP()
                                            const status = await window.go.main.Bindings.GetMCPStatus()
                                            setMcpStatus(status || [])
                                            window.go.main.Bindings.GetTools().then(setTools)
                                        }
                                    }}>Restart MCP Guardians</button>
                                </div>
                            </>)}
                        </div>
                        <div className="settings-footer">
                            <button className="settings-close-btn" onClick={() => setShowSettings(false)}>Close</button>
                        </div>
                    </div>
                </div>
            )}

            {lightboxImage && (
                <div className="lightbox-overlay" onClick={() => setLightboxImage(null)}>
                    <img src={lightboxImage} alt="" className="lightbox-image" onClick={e => e.stopPropagation()} />
                    <button className="lightbox-close" onClick={() => setLightboxImage(null)}>&#x2715;</button>
                </div>
            )}

            {expandedReport && (
                <div className="report-overlay" onClick={() => setExpandedReport(null)}>
                    <div className="report-fullscreen" onClick={e => e.stopPropagation()}>
                        <div className="report-fullscreen-header">
                            <span className="report-title">{expandedReport.title}</span>
                            <div className="report-actions">
                                <button onClick={(e) => { navigator.clipboard.writeText(expandedReport.content); const b = e.currentTarget; b.textContent = 'Copied!'; setTimeout(() => b.textContent = 'Copy', 1000) }}>Copy</button>
                                <button onClick={() => window.go?.main.Bindings.SaveReport(expandedReport.content, 'report.md')}>Save</button>
                                <button onClick={() => setExpandedReport(null)}>Close</button>
                            </div>
                        </div>
                        <div className="report-fullscreen-content">
                            <ReactMarkdown
                                remarkPlugins={[remarkGfm, remarkMath]}
                                rehypePlugins={[rehypeHighlight, rehypeKatex]}
                                urlTransform={urlTransform}
                                components={{img: ({src, alt}) => {
                                    if (src?.startsWith('object:')) {
                                        const id = src.slice(7)
                                        return <ObjectImage id={id} alt={alt || ''} onClick={setLightboxImage} />
                                    }
                                    return <img src={src} alt={alt || ''} className="message-image" onClick={() => src && setLightboxImage(src)} />
                                }}}
                            >
                                {expandedReport.content}
                            </ReactMarkdown>
                        </div>
                    </div>
                </div>
            )}
        </div>
    )
}

export default App
