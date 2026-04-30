// MessageItem renders a single chat message. Memoized so that
// pushing a new message (e.g. tool-event) does not re-render
// the entire history — important when the history contains
// huge ReactMarkdown blocks that are slow to re-parse.

import {memo, useMemo} from 'react'
import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import ObjectImage from '../ObjectImage'
import type {ChatMessage} from '../types'

// Allow object: protocol through ReactMarkdown URL sanitization.
// ReactMarkdown v10 defaultUrlTransform strips non-http/https/mailto
// protocols. object: URLs are resolved by ObjectImage component via
// img component override.
function urlTransform(url: string): string {
    if (url.startsWith('object:')) return url
    return defaultUrlTransform(url)
}

// Module-scope stable references avoid re-instantiating plugin
// arrays on every render — otherwise ReactMarkdown sees new props
// and re-parses every message.
const MD_REMARK_PLUGINS = [remarkGfm, remarkMath]
const MD_REHYPE_PLUGINS = [rehypeHighlight, rehypeKatex]

interface MessageItemProps {
    msg: ChatMessage;
    onLightbox: (url: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
}

const MessageItem = memo(function MessageItem({msg, onLightbox, onExpandReport}: MessageItemProps) {
    const components = useMemo(() => ({
        img: ({src, alt}: {src?: string; alt?: string}) => {
            if (src?.startsWith('object:')) {
                const id = src.slice(7)
                return <ObjectImage id={id} alt={alt || ''} onClick={onLightbox} />
            }
            return <img src={src} alt={alt || ''} className="message-image" onClick={() => src && onLightbox(src)} />
        },
    }), [onLightbox])

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
        return (
            <div className={`tool-bubble ${cls}`}>
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
                        <button onClick={() => window.go?.main.Bindings.SaveReport(msg.content, 'report.md')}>Save</button>
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
                    {msg.imageUrls.map((url, j) => (
                        <img key={j} src={url} alt="" className="message-image" onClick={() => onLightbox(url)} />
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
