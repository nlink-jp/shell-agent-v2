// ToolCallDetailsDialog — overlay shown when the user clicks a
// completed tool-event bubble in the chat pane. Displays the
// arguments the assistant passed to the tool and the result the
// tool returned, plus status and timestamps.
//
// Loading is keyed on toolCallId — opening the dialog triggers a
// GetToolCallDetails fetch; while in flight we show a spinner.
// Closing the dialog (X / backdrop click / Escape) discards the
// fetched data so reopening picks up any changes.

import {useEffect, useState} from 'react'
import type {ToolCallDetails} from '../types'

interface Props {
    toolCallId: string;
    onClose: () => void;
}

function tryFormatJson(s: string): string {
    const trimmed = s.trim()
    if (!trimmed) return ''
    if (!trimmed.startsWith('{') && !trimmed.startsWith('[')) return s
    try {
        return JSON.stringify(JSON.parse(trimmed), null, 2)
    } catch {
        return s
    }
}

function formatTimestamp(s: string): string {
    if (!s) return ''
    try {
        const d = new Date(s)
        if (isNaN(d.getTime())) return s
        return d.toLocaleString()
    } catch {
        return s
    }
}

export default function ToolCallDetailsDialog({toolCallId, onClose}: Props) {
    const [details, setDetails] = useState<ToolCallDetails | null>(null)
    const [error, setError] = useState<string | null>(null)

    useEffect(() => {
        let cancelled = false
        setDetails(null)
        setError(null)
        if (!window.go) return
        window.go.main.Bindings.GetToolCallDetails(toolCallId)
            .then(d => { if (!cancelled) setDetails(d) })
            .catch(e => { if (!cancelled) setError(String(e?.message ?? e)) })
        return () => { cancelled = true }
    }, [toolCallId])

    useEffect(() => {
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
        window.addEventListener('keydown', onKey)
        return () => window.removeEventListener('keydown', onKey)
    }, [onClose])

    const statusCls = details?.status === 'error' ? 'error'
        : details?.status === 'success' ? 'success'
        : ''

    return (
        <div className="modal-backdrop" onClick={onClose}>
            <div className="tool-details-dialog" onClick={e => e.stopPropagation()}>
                <div className="tool-details-header">
                    <span className="tool-details-title">
                        {details ? `Tool call: ${details.tool_name}` : 'Tool call'}
                        {statusCls && (
                            <span className={`tool-details-status ${statusCls}`}>{details?.status}</span>
                        )}
                    </span>
                    <button className="tool-details-close" onClick={onClose}>&#x2715;</button>
                </div>
                <div className="tool-details-body">
                    {error ? (
                        <div className="tool-details-error">Failed to load: {error}</div>
                    ) : !details ? (
                        <div className="tool-details-loading">{'Loading…'}</div>
                    ) : (
                        <>
                            {(details.call_timestamp || details.result_timestamp) && (
                                <div className="tool-details-meta">
                                    {details.call_timestamp && (
                                        <span><strong>Called:</strong> {formatTimestamp(details.call_timestamp)}</span>
                                    )}
                                    {details.result_timestamp && (
                                        <span><strong>Returned:</strong> {formatTimestamp(details.result_timestamp)}</span>
                                    )}
                                </div>
                            )}
                            <section className="tool-details-section">
                                <h4>Arguments</h4>
                                <pre className="tool-details-pre">{tryFormatJson(details.arguments) || '(none)'}</pre>
                            </section>
                            <section className="tool-details-section">
                                <h4>Result</h4>
                                <pre className="tool-details-pre">{details.result || '(empty)'}</pre>
                            </section>
                        </>
                    )}
                </div>
            </div>
        </div>
    )
}
