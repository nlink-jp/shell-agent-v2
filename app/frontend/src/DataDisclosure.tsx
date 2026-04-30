// DataDisclosure — read-only "what's in this session" panel.
//
// Phase 2 of the information-display redesign
// (docs/en/information-display-redesign.md). Renders three
// sub-sections (Objects, Tables, /work) for the currently-selected
// session, collapsed by default, and refetches on session switch
// or when the agent reports a tool-result event.

import {useCallback, useEffect, useMemo, useState} from 'react'

interface ObjectInfo {
    id: string;
    type: string;
    mime_type: string;
    orig_name: string;
    created_at: string;
    session_id: string;
    size: number;
}
interface TableInfo {
    name: string;
    row_count: number;
    columns: string[];
    description?: string;
}
interface TablePreview {
    columns: string[];
    rows: any[][];
    total: number;
    truncated: boolean;
}
interface WorkFile {
    path: string;
    size: number;
    mtime: number;
}

interface Props {
    sessionId: string;
    refreshTick: number;            // bumped by parent to force refetch (debounced)
    sandboxEnabled: boolean;        // hide the /work section when sandbox is off
    onPreviewObject?: (id: string) => void;
}

function fmtBytes(n: number): string {
    if (n < 1024) return `${n} B`
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
    return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

function fmtRows(n: number): string {
    if (n < 1000) return `${n} rows`
    if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k rows`
    return `${(n / 1_000_000).toFixed(1)}M rows`
}

export default function DataDisclosure({sessionId, refreshTick, sandboxEnabled, onPreviewObject}: Props) {
    const [objects, setObjects] = useState<ObjectInfo[]>([])
    const [tables, setTables] = useState<TableInfo[]>([])
    const [workFiles, setWorkFiles] = useState<WorkFile[]>([])
    const [open, setOpen] = useState(false)
    const [previewName, setPreviewName] = useState<string | null>(null)
    const [previewData, setPreviewData] = useState<TablePreview | null>(null)
    const [previewError, setPreviewError] = useState<string | null>(null)

    const refetch = useCallback(async () => {
        if (!sessionId || !window.go) {
            setObjects([]); setTables([]); setWorkFiles([])
            return
        }
        const [o, t, w] = await Promise.all([
            window.go.main.Bindings.GetSessionObjects(sessionId).catch(() => []),
            window.go.main.Bindings.GetSessionTables(sessionId).catch(() => []),
            sandboxEnabled
                ? window.go.main.Bindings.GetWorkFiles(sessionId).catch(() => [])
                : Promise.resolve([]),
        ])
        setObjects(o || [])
        setTables(t || [])
        setWorkFiles(w || [])
    }, [sessionId, sandboxEnabled])

    useEffect(() => { refetch() }, [refetch, refreshTick])

    const total = objects.length + tables.length + workFiles.length
    const isEmpty = total === 0

    const openPreview = useCallback(async (name: string) => {
        setPreviewName(name)
        setPreviewData(null)
        setPreviewError(null)
        try {
            const p = await window.go.main.Bindings.PreviewTable(name, 20)
            setPreviewData(p)
        } catch (e: any) {
            setPreviewError(String(e?.message || e))
        }
    }, [])

    const closePreview = useCallback(() => {
        setPreviewName(null)
        setPreviewData(null)
        setPreviewError(null)
    }, [])

    // Render an empty placeholder strip when there really is nothing
    // — keeps the chat pane uncluttered for fresh sessions.
    if (isEmpty) {
        return <div className="data-disclosure data-disclosure-empty">Data — empty</div>
    }

    return (
        <details className="data-disclosure" open={open} onToggle={e => setOpen((e.target as HTMLDetailsElement).open)}>
            <summary>
                <span className="data-summary-title">Data ({total})</span>
                {!open && (
                    <span className="data-summary-counts">
                        {objects.length > 0 && <span>{objects.length} obj</span>}
                        {tables.length > 0 && <span>{tables.length} tbl</span>}
                        {workFiles.length > 0 && <span>{workFiles.length} files</span>}
                    </span>
                )}
            </summary>
            <div className="data-body">
                {objects.length > 0 && (
                    <section className="data-section">
                        <h4>Objects ({objects.length})</h4>
                        <ul>
                            {objects.map(o => (
                                <li key={o.id} className="data-object-row" onClick={() => onPreviewObject?.(o.id)} title={o.id}>
                                    <span className="data-icon">{o.type === 'image' ? '\u{1F5BC}' : o.type === 'report' ? '\u{1F4C4}' : '\u{1F4E6}'}</span>
                                    <span className="data-name">{o.orig_name || o.id}</span>
                                    <span className="data-size">{fmtBytes(o.size)}</span>
                                </li>
                            ))}
                        </ul>
                    </section>
                )}
                {tables.length > 0 && (
                    <section className="data-section">
                        <h4>Tables ({tables.length})</h4>
                        <ul>
                            {tables.map(t => (
                                <li key={t.name} className="data-table-row" onClick={() => openPreview(t.name)}>
                                    <span className="data-icon">{'\u{1F4CA}'}</span>
                                    <span className="data-name">{t.name}</span>
                                    <span className="data-size">{fmtRows(t.row_count)}</span>
                                    <span className="data-cols" title={t.columns.join(', ')}>
                                        {t.columns.slice(0, 4).join(', ')}{t.columns.length > 4 && '\u2026'}
                                    </span>
                                </li>
                            ))}
                        </ul>
                    </section>
                )}
                {sandboxEnabled && workFiles.length > 0 && (
                    <section className="data-section">
                        <h4>/work ({workFiles.length})</h4>
                        <ul>
                            {workFiles.map(f => (
                                <li key={f.path} className="data-work-row">
                                    <span className="data-icon">{'\u{1F4DD}'}</span>
                                    <span className="data-name">{f.path}</span>
                                    <span className="data-size">{fmtBytes(f.size)}</span>
                                </li>
                            ))}
                        </ul>
                    </section>
                )}
            </div>
            {previewName !== null && (
                <PreviewModal name={previewName} data={previewData} error={previewError} onClose={closePreview} />
            )}
        </details>
    )
}

function PreviewModal({name, data, error, onClose}: {
    name: string;
    data: TablePreview | null;
    error: string | null;
    onClose: () => void;
}) {
    const truncatedNote = useMemo(() => {
        if (!data) return ''
        if (!data.truncated) return ''
        return ` (showing first ${data.rows.length} of ${data.total.toLocaleString()})`
    }, [data])

    return (
        <div className="preview-overlay" onClick={onClose}>
            <div className="preview-modal" onClick={e => e.stopPropagation()}>
                <div className="preview-header">
                    <span className="preview-title">{name}{truncatedNote}</span>
                    <button className="preview-close" onClick={onClose}>&#x2715;</button>
                </div>
                <div className="preview-body">
                    {error ? (
                        <pre className="preview-error">{error}</pre>
                    ) : !data ? (
                        <div className="preview-loading">{'Loading\u2026'}</div>
                    ) : (
                        <table className="preview-table">
                            <thead>
                                <tr>{data.columns.map(c => <th key={c}>{c}</th>)}</tr>
                            </thead>
                            <tbody>
                                {data.rows.map((row, i) => (
                                    <tr key={i}>
                                        {row.map((cell, j) => <td key={j}>{formatCell(cell)}</td>)}
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    )}
                </div>
            </div>
        </div>
    )
}

function formatCell(v: any): string {
    if (v === null || v === undefined) return ''
    if (typeof v === 'string') return v
    if (typeof v === 'number') return String(v)
    if (typeof v === 'boolean') return v ? 'true' : 'false'
    return JSON.stringify(v)
}
