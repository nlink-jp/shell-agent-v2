import {useState, useEffect, useRef, useCallback} from 'react'
import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import ChatInput from './ChatInput'
import ObjectImage, {clearObjectCache} from './ObjectImage'
import DataDisclosure from './DataDisclosure'
import MessageItem from './components/MessageItem'
import BulkActions from './components/BulkActions'
import SettingsDialog from './dialogs/SettingsDialog'
import MITLDialog from './dialogs/MITLDialog'
import Lightbox from './dialogs/Lightbox'
import ReportViewer from './dialogs/ReportViewer'
import 'highlight.js/styles/github-dark.css'
import 'katex/dist/katex.min.css'
import './themes.css'
import './App.css'
import './bindings'
import type {
    ChatMessage,
    ExpandedReport,
    Finding,
    LLMStatus,
    MCPStatus,
    MessageData,
    MITLRequest,
    ObjectInfo,
    PinnedMemory,
    SessionInfo,
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
    // Bumped to force the chat-pane Data disclosure to refetch.
    // Bumped on (a) session switch, (b) agent turn completion.
    const [dataRefreshTick, setDataRefreshTick] = useState(0)
    const [findings, setFindings] = useState<Finding[]>([])
    const [showSettings, setShowSettings] = useState(false)
    const [editingSession, setEditingSession] = useState<string | null>(null)
    const [editTitle, setEditTitle] = useState('')
    const [mitlRequest, setMitlRequest] = useState<MITLRequest | null>(null)
    // mitlFeedback state moved into MITLDialog (local concern).
    const [cmdResult, setCmdResult] = useState<string | null>(null)
    const [tools, setTools] = useState<ToolInfo[]>([])
    const [mcpStatus, setMcpStatus] = useState<MCPStatus[]>([])
    const [pinnedMemories, setPinnedMemories] = useState<PinnedMemory[]>([])
    const [selectedFindingIds, setSelectedFindingIds] = useState<Set<string>>(new Set())
    const [selectedPinnedKeys, setSelectedPinnedKeys] = useState<Set<string>>(new Set())
    // Objects panel state was removed in info-display redesign Phase 3 —
    // bulk selection, confirmation, and the global object list now live
    // inside the per-session DataDisclosure component.
    const [llmStatus, setLLMStatus] = useState<LLMStatus | null>(null)
    const [lightboxImage, setLightboxImage] = useState<string | null>(null)
    const [expandedReport, setExpandedReport] = useState<ExpandedReport | null>(null)
    const [settings, setSettings] = useState<Settings | null>(null)
    // settingsTab state moved into SettingsDialog (Phase 3 of
    // frontend-decomposition: it's a local-only concern).
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
                    // Phase A: backend reports 'success' or 'error'.
                    // Old event payloads without status fall back to
                    // success so older runs still render.
                    const endStatus: 'success' | 'error' = data.status === 'error' ? 'error' : 'success'
                    setMessages(prev => {
                        let idx = -1
                        for (let i = prev.length - 1; i >= 0; i--) {
                            const m = prev[i]
                            if (m.role === 'tool-event' && m.status === 'running' && m.content === data.detail) { idx = i; break }
                        }
                        if (idx === -1) return prev
                        const next = prev.slice()
                        next[idx] = {...next[idx], status: endStatus}
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
        if (sidebarPanel === 'memory' && window.go) {
            refreshFindings()
            window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            window.go.main.Bindings.GetPinnedMemories().then(setPinnedMemories)
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
    }, [state, currentSessionId])

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
            {/* Single sidebar DOM for both states: collapsed only
               adds an `is-collapsed` class that hides labels and
               content panels via CSS. The icon Y-positions and
               section dividers stay identical between modes. */}
            <div
                className={`sidebar ${sidebarCollapsed ? 'is-collapsed' : ''}`}
                style={sidebarCollapsed ? undefined : {width: sidebarWidth}}
            >
                {/* Two-way accordion: Sessions and Memory each
                   have a clickable header always visible. The
                   active section's content fills the available
                   vertical space; the inactive section is just
                   a header. */}
                <div className="sidebar-accordion">
                    <section className={`acc-section ${sidebarPanel === 'sessions' ? 'expanded' : 'collapsed'}`}>
                        <button
                            className={`sidebar-nav-btn ${sidebarPanel === 'sessions' ? 'active' : ''}`}
                            onClick={() => { if (sidebarCollapsed) setSidebarCollapsed(false); setSidebarPanel('sessions') }}
                            title="Sessions"
                        >
                            <span className="sidebar-nav-ic">&#x2630;</span>
                            <span className="sidebar-nav-label">Sessions</span>
                        </button>
                        {sidebarPanel === 'sessions' && (
                        <div className="acc-content"><>
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
                    </></div>
                        )}
                    </section>
                    <section className={`acc-section ${sidebarPanel === 'memory' ? 'expanded' : 'collapsed'}`}>
                        <button
                            className={`sidebar-nav-btn ${sidebarPanel === 'memory' ? 'active' : ''}`}
                            onClick={() => { if (sidebarCollapsed) setSidebarCollapsed(false); setSidebarPanel('memory') }}
                            title="Memory"
                        >
                            <span className="sidebar-nav-ic">&#x2605;</span>
                            <span className="sidebar-nav-label">Memory</span>
                        </button>
                        {sidebarPanel === 'memory' && (
                        <div className="acc-content"><>
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
                        {/* Tokens section moved to chat-pane footer in
                           info-display redesign Phase 4 — telemetry isn't
                           navigable content. */}
                    </></div>
                        )}
                    </section>
                    {/* Sidebar Objects panel removed in info-display redesign Phase 3.
                       Object management now lives in the per-session Data
                       disclosure (DataDisclosure component). */}
                </div>

                <div className="sidebar-bottom">
                    <button className="sidebar-nav-btn" onClick={handleNewSession} disabled={state === 'busy'} title="New Chat">
                        <span className="sidebar-nav-ic">+</span>
                        <span className="sidebar-nav-label">New Chat</span>
                    </button>
                    <div className="sidebar-nav-divider" />
                    <button className="sidebar-nav-btn" onClick={openSettings} title="Settings">
                        <span className="sidebar-nav-ic">&#x2699;</span>
                        <span className="sidebar-nav-label">Settings</span>
                    </button>
                    <div className="sidebar-nav-divider" />
                    <button
                        className="sidebar-nav-btn"
                        onClick={() => setSidebarCollapsed(c => !c)}
                        title={sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar'}
                    >
                        <span className="sidebar-nav-ic">{sidebarCollapsed ? '\u25B6' : '\u25C0'}</span>
                    </button>
                </div>
            </div>
            {!sidebarCollapsed && <div className="sidebar-resize" onMouseDown={startResize} />}
            <div className="main">
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
                                }
                                // blob has no built-in preview yet — left as a click no-op.
                            } catch {}
                        }}
                    />
                )}
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

            {expandedReport && (
                <ReportViewer
                    report={expandedReport}
                    onClose={() => setExpandedReport(null)}
                    onLightbox={setLightboxImage}
                    onSaveReport={(content, filename) => window.go?.main.Bindings.SaveReport(content, filename)}
                />
            )}
        </div>
    )
}

export default App
