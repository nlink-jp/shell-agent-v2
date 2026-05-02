// BlobPreview is the in-app preview for non-image, non-report
// objects (CSV, JSON, plain text, etc.). Images already get the
// Lightbox; reports get the ReportViewer; everything else used to
// be a click no-op, which read as "the app stored my CSV but won't
// let me see it." This component closes that gap.
//
// CSV / TSV are rendered as a simple HTML table — shell-agent-v2's
// data-analysis pitch makes it worth the small extra effort over a
// raw <pre> dump. Other text mimes drop through to a fixed-width
// preformatted view.
//
// Both modes truncate aggressively: at most ~100 KB of source is
// fetched (capped before this component sees the content), and the
// table renders at most TABLE_ROW_LIMIT rows. The footer states
// what was elided so the user is never silently misled into
// thinking the visible slice is the whole file.

import {useMemo} from 'react'

const TABLE_ROW_LIMIT = 200
const TABLE_COL_LIMIT = 30 // wider tables are still readable; this only guards against absurd CSV
const PRE_CHAR_LIMIT = 200000

export interface BlobPreviewData {
    title: string
    mime: string
    content: string
    sizeBytes?: number    // original full file size for the footer; optional
    truncated?: boolean   // true when content was truncated before reaching us
}

interface Props {
    data: BlobPreviewData
    onClose: () => void
}

export default function BlobPreview({data, onClose}: Props) {
    const isCsv = data.mime === 'text/csv'
    const isTsv = data.mime === 'text/tab-separated-values'
    const tabular = isCsv || isTsv

    return (
        <div className="preview-overlay" onClick={onClose}>
            <div className="preview-modal" onClick={e => e.stopPropagation()}>
                <div className="preview-header">
                    <span className="preview-title">{data.title}</span>
                    <button className="preview-close" onClick={onClose}>&#x2715;</button>
                </div>
                <div className="blob-preview-body">
                    {tabular
                        ? <CSVTable text={data.content} delimiter={isTsv ? '\t' : ','} truncated={data.truncated} />
                        : <TextDump text={data.content} truncated={data.truncated} />}
                </div>
            </div>
        </div>
    )
}

function TextDump({text, truncated}: {text: string; truncated?: boolean}) {
    const display = text.length > PRE_CHAR_LIMIT ? text.slice(0, PRE_CHAR_LIMIT) : text
    const wasCut = truncated || text.length > PRE_CHAR_LIMIT
    return (
        <>
            <pre className="blob-preview-pre">{display}</pre>
            {wasCut && (
                <div className="blob-preview-footnote">
                    Preview truncated. Use Export to download the full file.
                </div>
            )}
        </>
    )
}

// CSVTable parses a CSV/TSV slice and renders the first
// TABLE_ROW_LIMIT rows as a real <table>. The parser handles
// the common RFC 4180 cases — quoted fields, embedded delimiters,
// embedded newlines, and "" escapes — without pulling in a CSV
// library. Comma-quoted fields are by far the most common case
// users will hit (pandas to_csv default), so it's worth getting
// right; rare edge cases (multi-character delimiters, BOMs in the
// middle of fields) are not handled and would fall back to a noisy
// row, which is still better than a click that does nothing.
function CSVTable({text, delimiter, truncated}: {text: string; delimiter: string; truncated?: boolean}) {
    const parsed = useMemo(() => parseCSV(text, delimiter), [text, delimiter])
    if (parsed.rows.length === 0) {
        return <div className="blob-preview-empty">Empty file.</div>
    }

    const header = parsed.rows[0]
    const body = parsed.rows.slice(1, 1 + TABLE_ROW_LIMIT)
    const moreRows = Math.max(0, parsed.rows.length - 1 - body.length)
    const tooManyCols = header.length > TABLE_COL_LIMIT
    const visibleCols = tooManyCols ? header.slice(0, TABLE_COL_LIMIT) : header

    return (
        <>
            <table className="blob-preview-table">
                <thead>
                    <tr>
                        {visibleCols.map((h, i) => <th key={i}>{h}</th>)}
                        {tooManyCols && <th key="more-cols-h">{`\u2026 +${header.length - TABLE_COL_LIMIT} more`}</th>}
                    </tr>
                </thead>
                <tbody>
                    {body.map((row, ri) => (
                        <tr key={ri}>
                            {visibleCols.map((_, ci) => <td key={ci}>{row[ci] ?? ''}</td>)}
                            {tooManyCols && <td key="more-cols-c">{'\u2026'}</td>}
                        </tr>
                    ))}
                </tbody>
            </table>
            {(moreRows > 0 || truncated) && (
                <div className="blob-preview-footnote">
                    {moreRows > 0 && `Showing first ${body.length} of ${parsed.rows.length - 1} data rows. `}
                    {truncated && 'Source was truncated before reaching the preview. '}
                    Use Export to download the full file.
                </div>
            )}
        </>
    )
}

// parseCSV is a tiny RFC 4180-ish parser. It deliberately ignores
// configuration knobs (escape char, line terminator override) — the
// only quoting style we accept is the standard double-quote with
// "" doubling. Returning rows: string[][] keeps the table-render
// path branchless.
function parseCSV(text: string, delimiter: string): {rows: string[][]} {
    const rows: string[][] = []
    let field = ''
    let row: string[] = []
    let inQuotes = false
    let i = 0
    const n = text.length

    while (i < n) {
        const c = text[i]
        if (inQuotes) {
            if (c === '"') {
                if (i + 1 < n && text[i + 1] === '"') {
                    field += '"'
                    i += 2
                    continue
                }
                inQuotes = false
                i++
                continue
            }
            field += c
            i++
            continue
        }
        if (c === '"') {
            inQuotes = true
            i++
            continue
        }
        if (c === delimiter) {
            row.push(field)
            field = ''
            i++
            continue
        }
        if (c === '\n' || c === '\r') {
            row.push(field)
            rows.push(row)
            row = []
            field = ''
            // Swallow \r\n as a single break.
            if (c === '\r' && i + 1 < n && text[i + 1] === '\n') i += 2
            else i++
            continue
        }
        field += c
        i++
    }
    // Flush the last field/row only if there is anything in flight —
    // a trailing newline shouldn't synthesize a phantom empty row.
    if (field.length > 0 || row.length > 0) {
        row.push(field)
        rows.push(row)
    }
    return {rows}
}
