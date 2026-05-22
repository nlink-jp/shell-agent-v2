// FindingsDisclosure — chat-pane panel for the active session's
// data-analysis findings (v0.2.0 Phase 8).
//
// Modeled on DataDisclosure: a single <details> that defaults
// closed, refetches on session-switch / refreshTick, and also
// listens for `findings:updated` so the panel reflects new
// promote-finding emissions in real time without waiting for the
// surrounding App refresh.
//
// Each row exposes a Pin button that hands the finding back to
// the parent so it can promote into Global Memory (Phase 9 will
// wrap this in a category-picker dialog).

import {useCallback, useEffect, useMemo, useState} from 'react'
import type {Finding} from './types'

interface Props {
    sessionId: string;
    refreshTick: number;
    onPinFinding: (finding: Finding) => void;
    /** v0.3.0: hide the ★ Pin button when the active session is
     *  private. The binding rejects promotion regardless. */
    sessionPrivate?: boolean;
}

type SeverityFilter = 'all' | 'critical' | 'high' | 'medium' | 'low' | 'info'

const SEVERITIES: SeverityFilter[] = ['all', 'critical', 'high', 'medium', 'low', 'info']

function findingTrust(source?: string): {label: string; cls: string} {
    if (source === 'manual') {
        return {label: 'user-stated', cls: 'trust-user'}
    }
    return {label: 'derived', cls: 'trust-derived'}
}

export default function FindingsDisclosure({sessionId, refreshTick, onPinFinding, sessionPrivate}: Props) {
    const [findings, setFindings] = useState<Finding[]>([])
    const [open, setOpen] = useState(false)
    const [severity, setSeverity] = useState<SeverityFilter>('all')
    const [query, setQuery] = useState('')
    const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())
    const [confirmBulkDelete, setConfirmBulkDelete] = useState(false)
    const [confirmDeleteOne, setConfirmDeleteOne] = useState<string | null>(null)

    const refetch = useCallback(async () => {
        if (!sessionId || !window.go) {
            setFindings([])
            return
        }
        const f = await window.go.main.Bindings.GetFindings().catch(() => [])
        setFindings(f || [])
    }, [sessionId])

    useEffect(() => { refetch() }, [refetch, refreshTick])

    // Real-time refresh on findings:updated. Kept in addition to
    // refreshTick because the App-level subscriber may swap state
    // a tick before the panel re-renders.
    useEffect(() => {
        if (!window.runtime) return
        const cleanup = window.runtime.EventsOn('findings:updated', () => { refetch() })
        return cleanup
    }, [refetch])

    const filtered = useMemo(() => {
        let out = findings
        if (severity !== 'all') {
            out = out.filter(f => (f.tags || []).includes(severity))
        }
        const q = query.trim().toLowerCase()
        if (q) {
            out = out.filter(f =>
                f.content.toLowerCase().includes(q) ||
                (f.tags || []).some(t => t.toLowerCase().includes(q))
            )
        }
        return out
    }, [findings, severity, query])

    if (findings.length === 0) {
        return null
    }

    return (
        <details
            className="findings-disclosure"
            open={open}
            onToggle={e => setOpen((e.target as HTMLDetailsElement).open)}
        >
            <summary>
                <span className="data-summary-left">
                    <span className="data-summary-marker" aria-hidden>{open ? '▼' : '▶'}</span>
                    <span className="data-summary-title">Findings ({findings.length})</span>
                </span>
                {!open && filtered.length !== findings.length && (
                    <span className="data-summary-counts">
                        <span>{filtered.length} shown</span>
                    </span>
                )}
            </summary>
            <div className="findings-body">
                <div className="findings-toolbar">
                    <div className="findings-filter">
                        {SEVERITIES.map(s => (
                            <button
                                key={s}
                                className={`findings-filter-btn ${severity === s ? 'active' : ''}`}
                                onClick={() => setSeverity(s)}
                            >{s}</button>
                        ))}
                    </div>
                    <input
                        type="search"
                        className="findings-search"
                        placeholder="Search findings…"
                        value={query}
                        onChange={e => setQuery(e.target.value)}
                    />
                </div>
                {selectedIds.size > 0 && !confirmBulkDelete && (
                    <div className="data-bulk-actions findings-bulk">
                        <span className="data-bulk-count">{selectedIds.size} selected</span>
                        <button onClick={() => setSelectedIds(new Set(filtered.map(f => f.id)))}>All</button>
                        <button onClick={() => setSelectedIds(new Set())}>Clear</button>
                        <button className="data-bulk-delete" onClick={() => setConfirmBulkDelete(true)}>Delete{'…'}</button>
                    </div>
                )}
                {confirmBulkDelete && (
                    <div className="data-confirm-bar findings-bulk">
                        <span className="data-confirm-text">Delete {selectedIds.size} finding(s)?</span>
                        <button className="data-confirm-yes" onClick={async () => {
                            const ids = Array.from(selectedIds)
                            setConfirmBulkDelete(false)
                            if (ids.length === 0) return
                            try {
                                await window.go.main.Bindings.DeleteFindings(ids)
                                setSelectedIds(new Set())
                                await refetch()
                            } catch (err) {
                                alert('Delete failed: ' + ((err as any)?.message ?? String(err)))
                            }
                        }}>Delete</button>
                        <button className="data-confirm-no" onClick={() => setConfirmBulkDelete(false)}>Cancel</button>
                    </div>
                )}
                <ul className="findings-list">
                    {filtered.length === 0 ? (
                        <li className="findings-empty">No findings match the current filter.</li>
                    ) : filtered.map(f => {
                        const t = findingTrust(f.source)
                        const isConfirming = confirmDeleteOne === f.id
                        return (
                            <li key={f.id} className={`findings-row ${selectedIds.has(f.id) ? 'selected' : ''}`}>
                                <input
                                    type="checkbox"
                                    className="bulk-check"
                                    checked={selectedIds.has(f.id)}
                                    onChange={e => {
                                        const next = new Set(selectedIds)
                                        if (e.target.checked) next.add(f.id); else next.delete(f.id)
                                        setSelectedIds(next)
                                    }}
                                />
                                <div className="findings-content">
                                    <div className="findings-text">{f.content}</div>
                                    <div className="findings-meta">
                                        {(() => {
                                            const tip = t.cls === 'trust-user'
                                                ? 'user-stated: ユーザー操作で promote された finding。高信頼。'
                                                : 'derived: LLM が promote_finding / analyze_data 経由で登録した finding。内容は LLM を経由しており、攻撃者影響下のバイトを含みうる。'
                                            return (
                                                <span className={`trust-badge ${t.cls}`} data-tooltip={tip}>{t.label}</span>
                                            )
                                        })()}
                                        <span className="finding-date">{f.created_label}</span>
                                        {f.tags && f.tags.length > 0 && (
                                            <span className="findings-tags">
                                                {f.tags.map(tag => {
                                                    const sevClass = ['critical', 'high', 'medium', 'low', 'info'].includes(tag) ? ` severity-${tag}` : ''
                                                    return <span key={tag} className={`tag${sevClass}`}>{tag}</span>
                                                })}
                                            </span>
                                        )}
                                    </div>
                                </div>
                                <div className="findings-actions">
                                    {!sessionPrivate && (
                                        <button
                                            className="findings-pin"
                                            title="Pin to Global Memory"
                                            onClick={() => onPinFinding(f)}
                                        >&#x2605;</button>
                                    )}
                                    {isConfirming ? (
                                        <>
                                            <button className="data-confirm-yes" onClick={async () => {
                                                setConfirmDeleteOne(null)
                                                try {
                                                    await window.go.main.Bindings.DeleteFindings([f.id])
                                                    await refetch()
                                                } catch (err) {
                                                    alert('Delete failed: ' + ((err as any)?.message ?? String(err)))
                                                }
                                            }}>Yes</button>
                                            <button className="data-confirm-no" onClick={() => setConfirmDeleteOne(null)}>No</button>
                                        </>
                                    ) : (
                                        <button
                                            className="findings-delete"
                                            title="Delete"
                                            onClick={() => setConfirmDeleteOne(f.id)}
                                        >&#x2715;</button>
                                    )}
                                </div>
                            </li>
                        )
                    })}
                </ul>
            </div>
        </details>
    )
}
