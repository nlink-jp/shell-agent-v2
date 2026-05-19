// SessionControlPopover — ADR-0016 §3.10.
//
// An inline popover anchored under the status-bar profile/backend
// badges. Lets the user (1) switch the active session's profile via
// a dropdown, (2) toggle Local↔Vertex within that profile via a
// radio, (3) jump to Settings → LLM Profiles for full CRUD. Sits
// between the lightweight "what am I in?" badges (always visible)
// and the heavy Settings dialog (separate workflow).
//
// All controls disable while the agent is busy or extracting —
// same gate as /profile and /model chat commands.

import {useEffect, useRef} from 'react'
import type {main} from '../../wailsjs/go/models'

interface Props {
    profiles: main.ProfileSummary[];
    currentProfileID: string;
    activeBackend: string;          // "local" | "vertex_ai"
    isBusy: boolean;                 // agent.state == busy OR extracting
    onSwitchProfile: (profileID: string) => Promise<void>;
    onSwitchBackend: (backend: string) => Promise<void>;
    onClose: () => void;
    onOpenSettings: () => void;
}

export default function SessionControlPopover({
    profiles, currentProfileID, activeBackend, isBusy,
    onSwitchProfile, onSwitchBackend, onClose, onOpenSettings,
}: Props) {
    const ref = useRef<HTMLDivElement | null>(null)

    // Close on Esc or outside click.
    useEffect(() => {
        function onKey(e: KeyboardEvent) {
            if (e.key === 'Escape') onClose()
        }
        function onDoc(e: MouseEvent) {
            if (ref.current && !ref.current.contains(e.target as Node)) onClose()
        }
        document.addEventListener('keydown', onKey)
        // Defer the document click listener by a tick so the very
        // click that opened the popover doesn't also close it.
        const t = setTimeout(() => document.addEventListener('mousedown', onDoc), 0)
        return () => {
            document.removeEventListener('keydown', onKey)
            document.removeEventListener('mousedown', onDoc)
            clearTimeout(t)
        }
    }, [onClose])

    const current = profiles.find(p => p.id === currentProfileID)

    return (
        <div ref={ref} className="session-control-popover" role="dialog" aria-label="Session Control">
            <div className="scp-header">
                <h4>Session Control</h4>
                <button className="scp-close" onClick={onClose} aria-label="Close">&#x2715;</button>
            </div>
            {isBusy && (
                <div className="scp-busy-banner" title="Wait for the current turn or memory extraction to finish.">
                    Busy — please wait.
                </div>
            )}
            <div className="scp-section">
                <label className="scp-label">Profile</label>
                <select
                    className="scp-profile-select"
                    value={currentProfileID}
                    disabled={isBusy}
                    onChange={async e => {
                        const id = e.target.value
                        if (id === currentProfileID) return
                        try {
                            await onSwitchProfile(id)
                        } catch (err) {
                            console.error('SwitchSessionProfile:', err)
                        }
                    }}
                >
                    {profiles.map(p => (
                        <option key={p.id} value={p.id}>
                            {p.name}{p.is_default ? ' (default)' : ''}
                        </option>
                    ))}
                </select>
                {current && (
                    <div className="scp-hint">
                        default side: {current.default_backend === 'vertex_ai' ? 'Vertex AI' : 'Local'}
                    </div>
                )}
            </div>
            <div className="scp-section">
                <label className="scp-label">Active backend (this session, ephemeral)</label>
                <div className="scp-radio-group">
                    <label className={`scp-radio-row ${activeBackend === 'local' ? 'selected' : ''}`}>
                        <input
                            type="radio"
                            name="scp-backend"
                            value="local"
                            checked={activeBackend === 'local'}
                            disabled={isBusy}
                            onChange={async () => {
                                try {
                                    await onSwitchBackend('local')
                                } catch (err) {
                                    console.error('SwitchSessionBackend:', err)
                                }
                            }}
                        />
                        <span className="scp-radio-label">Local</span>
                        <span className="scp-radio-model">{current?.local_model || '—'}</span>
                    </label>
                    <label className={`scp-radio-row ${activeBackend === 'vertex_ai' ? 'selected' : ''}`}>
                        <input
                            type="radio"
                            name="scp-backend"
                            value="vertex_ai"
                            checked={activeBackend === 'vertex_ai'}
                            disabled={isBusy}
                            onChange={async () => {
                                try {
                                    await onSwitchBackend('vertex_ai')
                                } catch (err) {
                                    console.error('SwitchSessionBackend:', err)
                                }
                            }}
                        />
                        <span className="scp-radio-label">Vertex AI</span>
                        <span className="scp-radio-model">{current?.vertex_model || '—'}</span>
                    </label>
                </div>
            </div>
            <div className="scp-footer">
                <button className="scp-settings-link" onClick={() => { onOpenSettings(); onClose() }}>
                    Edit profiles in Settings →
                </button>
            </div>
        </div>
    )
}
