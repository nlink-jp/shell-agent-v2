import {useState, useEffect, useRef, useCallback} from 'react'
import './App.css'

// Wails runtime bindings (generated at build time)
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

function App() {
    const [state, setState] = useState<'idle' | 'busy'>('idle')
    const [messages, setMessages] = useState<ChatMessage[]>([])
    const [input, setInput] = useState('')
    const [streaming, setStreaming] = useState('')
    const [backend, setBackend] = useState('')
    const messagesEndRef = useRef<HTMLDivElement>(null)

    useEffect(() => {
        // Listen for streaming events
        if (window.runtime) {
            const cleanup = window.runtime.EventsOn('agent:stream', (data: any) => {
                if (data.done) {
                    setStreaming('')
                } else {
                    setStreaming(prev => prev + data.token)
                }
            })
            return cleanup
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
        if (window.go) {
            await window.go.main.Bindings.Abort()
        }
    }, [])

    const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
        if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault()
            handleSend()
        }
    }, [handleSend])

    return (
        <div className="app">
            <div className="titlebar-drag" />
            <div className="sidebar">
                <h2>Sessions</h2>
                <div className="sidebar-info">
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
