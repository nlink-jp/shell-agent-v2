// MessageItem renders a single chat message. Memoized so that
// pushing a new message (e.g. tool-event) does not re-render
// the entire history — important when the history contains
// huge ReactMarkdown blocks that are slow to re-parse.

import {memo, useMemo} from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import ObjectImage from '../ObjectImage'
import {urlTransform, objectComponents} from '../markdown/objectMarkdown'
import type {ChatMessage} from '../types'

// Module-scope stable references avoid re-instantiating plugin
// arrays on every render — otherwise ReactMarkdown sees new props
// and re-parses every message.
const MD_REMARK_PLUGINS = [remarkGfm, remarkMath]
const MD_REHYPE_PLUGINS = [rehypeHighlight, rehypeKatex]

interface MessageItemProps {
    msg: ChatMessage;
    onLightbox: (url: string, objectId?: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
    onToolEventClick?: (toolCallId: string) => void;
}

// openDocumentAttachment resolves a {id?, name, dataURL?} entry
// to the markdown body and routes it through the existing
// ReportViewer. Live messages have dataURL (decode locally);
// restored messages have id (fetch via GetObjectText). Keeps
// the click-to-preview behaviour symmetric across the two paths.
async function openDocumentAttachment(
    att: {id?: string; name: string; dataURL?: string},
    onExpandReport: (r: {title: string; content: string}) => void,
): Promise<void> {
    let content: string
    if (att.dataURL) {
        // data:text/markdown;base64,<encoded>
        const m = att.dataURL.match(/^data:[^;,]*;base64,(.*)$/)
        if (!m) return
        try {
            // atob → binary string → percent-encoded → decoded UTF-8.
            // base64 decode and re-decode as UTF-8 so non-ASCII
            // markdown (CJK headings, accented characters) renders
            // correctly through ReportViewer.
            const bin = atob(m[1])
            const bytes = new Uint8Array(bin.length)
            for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
            content = new TextDecoder('utf-8').decode(bytes)
        } catch (_e) {
            content = '(failed to decode attachment data URL)'
        }
    } else if (att.id && window.go) {
        try {
            content = await window.go.main.Bindings.GetObjectText(att.id)
        } catch (e: any) {
            content = `(failed to load attachment: ${e?.message || e})`
        }
    } else {
        return
    }
    const firstLine = (content.split('\n')[0] || '').replace(/^#\s*/, '')
    const title = firstLine || att.name || att.id || 'Attachment'
    onExpandReport({title, content})
}

const MessageItem = memo(function MessageItem({msg, onLightbox, onExpandReport, onToolEventClick}: MessageItemProps) {
    // Single object-aware override surface shared with every other
    // ReactMarkdown site (ADR-0014). useMemo keeps the reference
    // stable so ReactMarkdown doesn't re-parse on each render.
    const components = useMemo(
        () => objectComponents({onLightbox, onExpandReport}),
        [onLightbox, onExpandReport],
    )

    if (msg.role === 'tool-event') {
        const cls = msg.status === 'running' ? 'running'
            : msg.status === 'error' ? 'error'
            : 'success'
        const icon = msg.status === 'running' ? '\u25CF'
            : msg.status === 'error' ? '\u2715'
            : '\u2713'
        // Use a distinct class name from the outer
        // .message.tool-event wrapper — otherwise both elements
        // pick up the bubble styling and we get a frame inside a
        // frame.
        // Click-to-inspect is enabled only for completed bubbles
        // that carry a tool_call_id — running bubbles have no
        // recorded result yet, and legacy restored rows without
        // an id can't fetch details.
        const clickable = msg.status !== 'running' && !!msg.toolCallId && !!onToolEventClick
        return (
            <div
                className={`tool-bubble ${cls}${clickable ? ' clickable' : ''}`}
                onClick={clickable && msg.toolCallId ? () => onToolEventClick!(msg.toolCallId!) : undefined}
                title={clickable ? 'Click to view arguments and result' : undefined}
            >
                <span className="tool-bubble-icon">{icon}</span>
                <span className="tool-bubble-name">{msg.content}</span>
            </div>
        )
    }
    if (msg.role === 'summary') {
        return (
            <div className="summary-block">
                <div className="summary-block-header">Summarized earlier turns</div>
                <div className="summary-block-body">
                    <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform} components={components}>
                        {msg.content}
                    </ReactMarkdown>
                </div>
            </div>
        )
    }
    if (msg.role === 'report') {
        const title = msg.content.split('\n')[0].replace(/^#\s*/, '')
        return (
            <div className="report-container">
                <div className="report-header">
                    <span className="report-title">{title}</span>
                    <div className="report-actions">
                        <button onClick={() => onExpandReport({title, content: msg.content})}>Expand</button>
                        <button onClick={(e) => { navigator.clipboard.writeText(msg.content); const b = e.currentTarget; b.textContent = 'Copied!'; setTimeout(() => b.textContent = 'Copy', 1000) }}>Copy</button>
                        <button onClick={(e) => {
                            // Issue #9: see ReportViewer.tsx — blur
                            // before the native save dialog opens so
                            // the dialog-confirming Enter doesn't
                            // re-trigger this button when focus
                            // returns to the browser.
                            e.currentTarget.blur()
                            window.go?.main.Bindings.SaveReport(msg.content, 'report.md')
                        }}>Save</button>
                    </div>
                </div>
                <div className="report-content" onClick={() => onExpandReport({title, content: msg.content})}>
                    <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform} components={components}>
                        {msg.content}
                    </ReactMarkdown>
                </div>
            </div>
        )
    }
    return (
        <>
            <div className="message-header">
                <span className="message-role">{msg.role}</span>
                <span className="message-time">{msg.timestamp || ''}</span>
            </div>
            {msg.imageUrls && msg.imageUrls.length > 0 && (
                <div className="message-images">
                    {msg.imageUrls.map((url, j) => {
                        // Live messages use data: URLs (immediate);
                        // restored messages use object:<id> URLs that
                        // ObjectImage resolves to a data URL via the
                        // backend so the original images re-appear.
                        if (url.startsWith('object:')) {
                            return <ObjectImage key={j} id={url.slice(7)} onClick={onLightbox} />
                        }
                        return <img key={j} src={url} alt="" className="message-image" onClick={() => onLightbox(url)} />
                    })}
                </div>
            )}
            {/* v0.5: markdown / text attachments render as labelled
                cards with filename + click-to-preview (opens the
                same ReportViewer the data panel uses). Live messages
                carry a dataURL (decode locally); restored messages
                carry an objstore id (fetch via GetObjectText on
                click). Either way the bubble shows the filename
                immediately. */}
            {msg.documents && msg.documents.length > 0 && (
                <div className="message-attachments">
                    {msg.documents.map((att, j) => (
                        <button
                            key={j}
                            type="button"
                            className="message-attachment-doc"
                            title={att.name + ' — click to preview'}
                            onClick={() => { void openDocumentAttachment(att, onExpandReport) }}
                        >
                            📝 {att.name}
                        </button>
                    ))}
                </div>
            )}
            <div className="message-content">
                <ReactMarkdown remarkPlugins={MD_REMARK_PLUGINS} rehypePlugins={MD_REHYPE_PLUGINS} urlTransform={urlTransform} components={components}>
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
    )
})

export default MessageItem
