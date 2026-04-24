import {useState} from 'react'
import './App.css'

function App() {
    const [state] = useState<'idle' | 'busy'>('idle')

    return (
        <div className="app">
            <div className="titlebar-drag" />
            <div className="sidebar">
                <h2>Sessions</h2>
                {/* TODO: session list */}
            </div>
            <div className="main">
                <div className="messages">
                    {/* TODO: message list */}
                </div>
                <div className="input-area">
                    <input
                        type="text"
                        placeholder={state === 'idle' ? 'Type a message...' : 'Agent is busy...'}
                        disabled={state === 'busy'}
                    />
                    {state === 'busy' && (
                        <button className="abort-btn">Abort</button>
                    )}
                </div>
            </div>
        </div>
    )
}

export default App
