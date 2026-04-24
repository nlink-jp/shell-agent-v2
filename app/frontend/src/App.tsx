import {useState, useEffect, useRef, useCallback} from 'react'
import './App.css'

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
                    HasData(): Promise<boolean>;
                    GetFindings(): Promise<Finding[]>;
                    GetSettings(): Promise<Settings>;
                    SaveSettings(s: Settings): Promise<void>;
                };
            };
        };
        runtime: {
            EventsOn(event: string, callback: (...args: any[]) => void): () => void;
        };
    }
}

interface ChatMessage {
    role: 'user' | 'assistant' | 'system';
    content: string;
}

interface Finding {
    id: string;
    content: string;
    session_id: string;
    session_title: string;
    tags: string[];
    created_label: string;
}

interface Settings {
    default_backend: string;
    local_endpoint: string;
    local_model: string;
    vertex_project: string;
    vertex_region: string;
    vertex_model: string;
    theme: string;
}

type SidebarTab = 'sessions' | 'findings' | 'settings';

function App() {
    const [state, setState] = useState<'idle' | 'busy'>('idle')
    const [messages, setMessages] = useState<ChatMessage[]>([])
    const [input, setInput] = useState('')
    const [streaming, setStreaming] = useState('')
    const [backend, setBackend] = useState('')
    const [sidebarTab, setSidebarTab] = useState<SidebarTab>('sessions')
    const [findings, setFindings] = useState<Finding[]>([])
    const [settings, setSettings] = useState<Settings | null>(null)
    const messagesEndRef = useRef<HTMLDivElement>(null)

    useEffect(() => {
        if (window.runtime) {
            return window.runtime.EventsOn('agent:stream', (data: any) => {
                if (data.done) {
                    setStreaming('')
                } else {
                    setStreaming(prev => prev + data.token)
                }
            })
        }
    }, [])

    useEffect(() => {
        messagesEndRef.current?.scrollIntoView({behavior: 'smooth'})
    }, [messages, streaming])

    useEffect(() => {
        if (window.go) {
            window.go.main.Bindings.GetBackend().then(setBackend)
        }
    }, [])

    const refreshFindings = useCallback(async () => {
        if (window.go) {
            const f = await window.go.main.Bindings.GetFindings()
            setFindings(f || [])
        }
    }, [])

    const loadSettings = useCallback(async () => {
        if (window.go) {
            const s = await window.go.main.Bindings.GetSettings()
            setSettings(s)
        }
    }, [])

    useEffect(() => {
        if (sidebarTab === 'findings') refreshFindings()
        if (sidebarTab === 'settings') loadSettings()
    }, [sidebarTab, refreshFindings, loadSettings])

    const handleSend = useCallback(async () => {
        const msg = input.trim()
        if (!msg || state === 'busy') return

        setInput('')
        setMessages(prev => [...prev, {role: 'user', content: msg}])
        setState('busy')
        setStreaming('')

        try {
            const response = await window.go.main.Bindings.Send(msg)
            setMessages(prev => [...prev, {role: 'assistant', content: response}])
        } catch (err: any) {
            setMessages(prev => [...prev, {role: 'system', content: `Error: ${err.message || err}`}])
        } finally {
            setState('idle')
            setStreaming('')
            if (window.go) {
                window.go.main.Bindings.GetBackend().then(setBackend)
            }
        }
    }, [input, state])

    const handleAbort = useCallback(async () => {
        if (window.go) await window.go.main.Bindings.Abort()
    }, [])

    const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
        if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault()
            handleSend()
        }
    }, [handleSend])

    const handleSaveSettings = useCallback(async () => {
        if (settings && window.go) {
            await window.go.main.Bindings.SaveSettings(settings)
        }
    }, [settings])

    return (
        <div className="app">
            <div className="titlebar-drag" />
            <div className="sidebar">
                <div className="sidebar-tabs">
                    <button className={sidebarTab === 'sessions' ? 'active' : ''} onClick={() => setSidebarTab('sessions')}>Sessions</button>
                    <button className={sidebarTab === 'findings' ? 'active' : ''} onClick={() => setSidebarTab('findings')}>Findings</button>
                    <button className={sidebarTab === 'settings' ? 'active' : ''} onClick={() => setSidebarTab('settings')}>Settings</button>
                </div>

                {sidebarTab === 'sessions' && (
                    <div className="sidebar-panel">
                        <p className="sidebar-hint">Session list (coming soon)</p>
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
                                        {f.tags.map(tag => <span key={tag} className="tag">{tag}</span>)}
                                    </div>
                                )}
                            </div>
                        ))}
                    </div>
                )}

                {sidebarTab === 'settings' && settings && (
                    <div className="sidebar-panel settings-panel">
                        <label>
                            Default Backend
                            <select value={settings.default_backend} onChange={e => setSettings({...settings, default_backend: e.target.value})}>
                                <option value="local">Local LLM</option>
                                <option value="vertex_ai">Vertex AI</option>
                            </select>
                        </label>
                        <h3>Local LLM</h3>
                        <label>
                            Endpoint
                            <input value={settings.local_endpoint} onChange={e => setSettings({...settings, local_endpoint: e.target.value})} />
                        </label>
                        <label>
                            Model
                            <input value={settings.local_model} onChange={e => setSettings({...settings, local_model: e.target.value})} />
                        </label>
                        <h3>Vertex AI</h3>
                        <label>
                            Project ID
                            <input value={settings.vertex_project} onChange={e => setSettings({...settings, vertex_project: e.target.value})} />
                        </label>
                        <label>
                            Region
                            <input value={settings.vertex_region} onChange={e => setSettings({...settings, vertex_region: e.target.value})} />
                        </label>
                        <label>
                            Model
                            <input value={settings.vertex_model} onChange={e => setSettings({...settings, vertex_model: e.target.value})} />
                        </label>
                        <button className="save-btn" onClick={handleSaveSettings}>Save</button>
                    </div>
                )}

                <div className="sidebar-footer">
                    <span className={`backend-badge ${backend}`}>{backend || '...'}</span>
                </div>
            </div>
            <div className="main">
                <div className="messages">
                    {messages.map((msg, i) => (
                        <div key={i} className={`message ${msg.role}`}>
                            <div className="message-role">{msg.role}</div>
                            <div className="message-content">{msg.content}</div>
                        </div>
                    ))}
                    {streaming && (
                        <div className="message assistant streaming">
                            <div className="message-role">assistant</div>
                            <div className="message-content">{streaming}</div>
                        </div>
                    )}
                    <div ref={messagesEndRef} />
                </div>
                <div className="input-area">
                    <input
                        type="text"
                        value={input}
                        onChange={e => setInput(e.target.value)}
                        onKeyDown={handleKeyDown}
                        placeholder={state === 'idle' ? 'Type a message...' : 'Agent is busy...'}
                        disabled={state === 'busy'}
                    />
                    {state === 'busy' ? (
                        <button className="abort-btn" onClick={handleAbort}>Abort</button>
                    ) : (
                        <button className="send-btn" onClick={handleSend} disabled={!input.trim()}>Send</button>
                    )}
                </div>
            </div>
        </div>
    )
}

export default App
