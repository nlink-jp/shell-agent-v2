// DataDisclosure — read-only "what's in this session" panel.
//
// Phase 2 of the information-display redesign
// (docs/en/information-display-redesign.md). Renders three
// sub-sections (Objects, Tables, /work) for the currently-selected
// session, collapsed by default, and refetches on session switch
// or when the agent reports a tool-result event.

import {useCallback, useEffect, useMemo, useState} from 'react'
import ObjectImage from './ObjectImage'

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
    onPreviewObject?: (obj: ObjectInfo) => void;
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
                <span className="data-summary-left">
                    <span className="data-summary-marker" aria-hidden>{open ? '\u25BC' : '\u25B6'}</span>
                    <span className="data-summary-title">Data ({total})</span>
                </span>
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
                        <div className="data-object-grid">
                            {objects.map(o => (
                                <ObjectCard
                                    key={o.id}
                                    obj={o}
                                    onPreview={onPreviewObject}
                                />
                            ))}
                        </div>
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
                                        {t.columns.join(', ')}
                                    </span>
                                </li>
                            ))}
                        </ul>
                    </section>
                )}
                {sandboxEnabled && workFiles.length > 0 && (
                    <section className="data-section">
                        <h4>/work ({workFiles.length})</h4>
                        <div className="data-work-grid">
                            {workFiles.map(f => (
                                <div key={f.path} className="data-work-card" title={f.path}>
                                    <span className="data-work-ext">{extOf(f.path)}</span>
                                    <span className="data-work-name">{f.path}</span>
                                    <span className="data-work-size">{fmtBytes(f.size)}</span>
                                </div>
                            ))}
                        </div>
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

// ObjectCard renders a single object as a thumbnail card.
// Images get a real thumbnail via ObjectImage (which caches data
// URLs across the session); blobs and reports get a typed icon
// card so the user can still see size and filename at a glance.
function ObjectCard({obj, onPreview}: {obj: ObjectInfo; onPreview?: (obj: ObjectInfo) => void}) {
    const isImage = obj.type === 'image'
    return (
        <div
            className={`data-object-card data-object-${obj.type}`}
            title={obj.id}
            onClick={() => onPreview?.(obj)}
        >
            <div className="data-object-thumb">
                {isImage ? (
                    <ObjectImage id={obj.id} alt={obj.orig_name} />
                ) : (
                    <span className="data-object-glyph">
                        {obj.type === 'report' ? '\u{1F4C4}' : '\u{1F4E6}'}
                    </span>
                )}
            </div>
            <div className="data-object-meta">
                <div className="data-object-name">{obj.orig_name || obj.id}</div>
                <div className="data-object-size">{fmtBytes(obj.size)}</div>
            </div>
        </div>
    )
}

// extOf returns the file extension (no leading dot, upper-cased)
// or "FILE" if there isn't one. Used as a tiny corner badge on
// /work cards.
function extOf(path: string): string {
    const idx = path.lastIndexOf('.')
    if (idx < 0 || idx === path.length - 1) return 'FILE'
    const ext = path.slice(idx + 1)
    if (ext.length > 5) return 'FILE'
    return ext.toUpperCase()
}

function formatCell(v: any): string {
    if (v === null || v === undefined) return ''
    if (typeof v === 'string') return v
    if (typeof v === 'number') return String(v)
    if (typeof v === 'boolean') return v ? 'true' : 'false'
    return JSON.stringify(v)
}
