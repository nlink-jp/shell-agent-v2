import {useState, useEffect, useRef, useCallback} from 'react'
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
                };
            };
        };
        runtime: {
            EventsOn(event: string, callback: (...args: any[]) => void): () => void;
        };
    }
}

interface ChatMessage {
    role: 'user' | 'assistant' | 'system' | 'tool' | 'report';
    content: string;
    timestamp: string;
    imageUrls?: string[];
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

interface Settings {
    default_backend: string;
    local_endpoint: string;
    local_model: string;
    vertex_project: string;
    vertex_region: string;
    vertex_model: string;
    theme: string;
    location: string;
}

type SidebarTab = 'sessions' | 'findings' | 'tools' | 'status';

function App() {
    const [state, setState] = useState<'idle' | 'busy'>('idle')
    const [messages, setMessages] = useState<ChatMessage[]>([])
    const [streaming, setStreaming] = useState('')
    const [backend, setBackend] = useState('')
    const [sidebarTab, setSidebarTab] = useState<SidebarTab>('sessions')
    const [sessions, setSessions] = useState<SessionInfo[]>([])
    const [currentSessionId, setCurrentSessionId] = useState<string>('')
    const [findings, setFindings] = useState<Finding[]>([])
    const [showSettings, setShowSettings] = useState(false)
    const [editingSession, setEditingSession] = useState<string | null>(null)
    const [editTitle, setEditTitle] = useState('')
    const [mitlRequest, setMitlRequest] = useState<{tool_name: string; arguments: string; category: string} | null>(null)
    const [mitlFeedback, setMitlFeedback] = useState('')
    const [tools, setTools] = useState<ToolInfo[]>([])
    const [pinnedMemories, setPinnedMemories] = useState<PinnedMemory[]>([])
    const [llmStatus, setLLMStatus] = useState<LLMStatus | null>(null)
    const [lightboxImage, setLightboxImage] = useState<string | null>(null)
    const [expandedReport, setExpandedReport] = useState<{title: string; content: string} | null>(null)
    const [settings, setSettings] = useState<Settings | null>(null)
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
                } else if (data.type === 'tool_start') {
                    setProgressTool(data.detail || '')
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
        if (window.go) {
            window.go.main.Bindings.GetBackend().then(setBackend)
            // Apply saved theme on startup
            window.go.main.Bindings.GetSettings().then(s => {
                if (s.theme) document.documentElement.setAttribute('data-theme', s.theme)
            })
        }
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
        if (sidebarTab === 'findings') refreshFindings()
        if (sidebarTab === 'tools' && window.go) window.go.main.Bindings.GetTools().then(setTools)
        if (sidebarTab === 'status' && window.go) {
            window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            window.go.main.Bindings.GetPinnedMemories().then(setPinnedMemories)
        }
    }, [sidebarTab, refreshFindings])

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
            if (response && response.trim()) {
                setMessages(prev => [...prev, {role: 'assistant', content: response, timestamp: nowTime()}])
            }
        } catch (err: any) {
            setMessages(prev => [...prev, {role: 'system', content: `Error: ${err.message || err}`, timestamp: nowTime()}])
        } finally {
            setState('idle')
            setStreaming('')
            setProgressTool('')
            if (window.go) {
                window.go.main.Bindings.GetBackend().then(setBackend)
                window.go.main.Bindings.GetLLMStatus().then(setLLMStatus)
            }
        }
    }, [state, currentSessionId])

    const handleAbort = useCallback(async () => {
        if (window.go) await window.go.main.Bindings.Abort()
    }, [])

    return (
        <div className="app">
            <div className="titlebar-drag" />
            <div className="sidebar">
                <div className="sidebar-tabs">
                    <button className={sidebarTab === 'sessions' ? 'active' : ''} onClick={() => setSidebarTab('sessions')}>Sessions</button>
                    <button className={sidebarTab === 'findings' ? 'active' : ''} onClick={() => setSidebarTab('findings')}>Findings</button>
                    <button className={sidebarTab === 'tools' ? 'active' : ''} onClick={() => setSidebarTab('tools')}>Tools</button>
                    <button className={sidebarTab === 'status' ? 'active' : ''} onClick={() => setSidebarTab('status')}>Status</button>
                </div>

                {sidebarTab === 'sessions' && (
                    <div className="sidebar-panel">
                        <button className="new-session-btn" onClick={handleNewSession} disabled={state === 'busy'}>+ New Session</button>
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
                    </div>
                )}

                {sidebarTab === 'findings' && (
                    <div className="sidebar-panel">
                        {findings.length === 0 ? (
                            <p className="sidebar-hint">No findings yet</p>
                        ) : findings.map(f => (
                            <div key={f.id} className="finding-card">
                                <div className="finding-content">{f.content}</div>
                                <div className="finding-meta">
                                    <span className="finding-date">{f.created_label}</span>
                                    {f.session_title && (
                                        <span className="finding-origin" title={`Session: ${f.session_id}`}>
                                            {f.session_title}
                                        </span>
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
                        ))}
                    </div>
                )}

                {sidebarTab === 'tools' && (
                    <div className="sidebar-panel">
                        {tools.length === 0 ? (
                            <p className="sidebar-hint">No tools available</p>
                        ) : tools.map(t => (
                            <div key={t.name} className="tool-item">
                                <div className="tool-name">
                                    <code>{t.name}</code>
                                    <span className={`tool-category ${t.category}`}>{t.category}</span>
                                    <span className="tool-source">{t.source}</span>
                                </div>
                                <div className="tool-desc">{t.description}</div>
                            </div>
                        ))}
                    </div>
                )}

                {sidebarTab === 'status' && (
                    <div className="sidebar-panel">
                        {llmStatus && (
                            <div className="status-section">
                                <h3>LLM</h3>
                                <div className="status-row"><span>Backend</span><span>{llmStatus.backend}</span></div>
                                <div className="status-row"><span>Session</span><span>{llmStatus.session_id || '-'}</span></div>
                                <div className="status-row"><span>Hot messages</span><span>{llmStatus.hot_messages}</span></div>
                                <div className="status-row"><span>Warm summaries</span><span>{llmStatus.warm_summaries}</span></div>
                                <div className="status-row"><span>Prompt tokens</span><span>{llmStatus.prompt_tokens.toLocaleString()}</span></div>
                                <div className="status-row"><span>Output tokens</span><span>{llmStatus.output_tokens.toLocaleString()}</span></div>
                            </div>
                        )}
                        <div className="status-section">
                            <h3>Pinned Memory</h3>
                            {pinnedMemories.length === 0 ? (
                                <p className="sidebar-hint">No pinned facts</p>
                            ) : pinnedMemories.map((p, i) => (
                                <div key={i} className="pinned-item">
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
                    </div>
                )}

                <div className="sidebar-footer">
                    <span className={`backend-badge ${backend}`}>{backend || '...'}</span>
                    <button className="settings-btn" onClick={openSettings} title="Settings">&#x2699;</button>
                </div>
            </div>
            <div className="main">
                <div className="messages">
                    {messages.filter(msg => msg.role !== 'tool').map((msg, i) => (
                        <div key={i} className={`message ${msg.role}`}>
                            {msg.role === 'report' ? (
                                <>
                                    <div className="report-container">
                                        <div className="report-header">
                                            <span className="report-title">{msg.content.split('\n')[0].replace(/^#\s*/, '')}</span>
                                            <div className="report-actions">
                                                <button onClick={() => setExpandedReport({title: msg.content.split('\n')[0].replace(/^#\s*/, ''), content: msg.content})}>Expand</button>
                                                <button onClick={(e) => { navigator.clipboard.writeText(msg.content); const b = e.currentTarget; b.textContent = 'Copied!'; setTimeout(() => b.textContent = 'Copy', 1000) }}>Copy</button>
                                                <button onClick={() => window.go?.main.Bindings.SaveReport(msg.content, 'report.md')}>Save</button>
                                            </div>
                                        </div>
                                        <div className="report-content" onClick={() => setExpandedReport({title: msg.content.split('\n')[0].replace(/^#\s*/, ''), content: msg.content})}>
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
                                                {msg.content}
                                            </ReactMarkdown>
                                        </div>
                                    </div>
                                </>
                            ) : (
                                <>
                                    <div className="message-header">
                                        <span className="message-role">{msg.role}</span>
                                        <span className="message-time">{msg.timestamp || ''}</span>
                                    </div>
                                    {msg.imageUrls && msg.imageUrls.length > 0 && (
                                        <div className="message-images">
                                            {msg.imageUrls.map((url, j) => (
                                                <img key={j} src={url} alt="" className="message-image" onClick={() => setLightboxImage(url)} />
                                            ))}
                                        </div>
                                    )}
                                    <div className="message-content">
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
                            )}
                        </div>
                    ))}
                    {streaming && (
                        <div className="message assistant streaming">
                            <div className="message-header">
                                <span className="message-role">assistant</span>
                            </div>
                            <div className="message-content">
                                <ReactMarkdown remarkPlugins={[remarkGfm, remarkMath]} rehypePlugins={[rehypeHighlight, rehypeKatex]} urlTransform={urlTransform}>
                                    {streaming}
                                </ReactMarkdown>
                            </div>
                        </div>
                    )}
                    <div ref={messagesEndRef} />
                </div>
                {state === 'busy' ? (
                    <div className="input-area">
                        {progressTool && (
                            <div className="tool-progress">Executing: {progressTool}</div>
                        )}
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
                        <div className="mitl-tool-name">
                            <span className="mitl-label">Tool:</span>
                            <code>{mitlRequest.tool_name}</code>
                            <span className={`tool-category ${mitlRequest.category}`}>{mitlRequest.category}</span>
                        </div>
                        <div className="mitl-args">
                            <span className="mitl-label">{mitlRequest.category === 'sql_preview' ? 'SQL:' : 'Details:'}</span>
                            <pre>{(() => { try { return JSON.stringify(JSON.parse(mitlRequest.arguments), null, 2) } catch { return mitlRequest.arguments } })()}</pre>
                        </div>
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
                        </div>
                        <div className="settings-body">
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
                                <h3>Local LLM</h3>
                                <label>
                                    <span>Endpoint</span>
                                    <input value={settings.local_endpoint} onChange={e => updateSetting({local_endpoint: e.target.value})} />
                                </label>
                                <label>
                                    <span>Model</span>
                                    <input value={settings.local_model} onChange={e => updateSetting({local_model: e.target.value})} />
                                </label>
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
                            </div>
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
