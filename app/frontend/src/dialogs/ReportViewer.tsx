// ReportViewer is the full-screen markdown overlay for a stored
// report. Renders the same content the inline report card shows,
// but with more space and a sticky header for Copy / Save / Close.

import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import ObjectImage from '../ObjectImage'
import type {ExpandedReport} from '../types'

const REMARK_PLUGINS = [remarkGfm, remarkMath]
const REHYPE_PLUGINS = [rehypeHighlight, rehypeKatex]

function urlTransform(url: string): string {
    if (url.startsWith('object:')) return url
    return defaultUrlTransform(url)
}

interface Props {
    report: ExpandedReport;
    onClose: () => void;
    onLightbox: (url: string) => void;
    onSaveReport: (content: string, filename: string) => void;
}

export default function ReportViewer({report, onClose, onLightbox, onSaveReport}: Props) {
    return (
        <div className="report-overlay" onClick={onClose}>
            <div className="report-fullscreen" onClick={e => e.stopPropagation()}>
                <div className="report-fullscreen-header">
                    <span className="report-title">{report.title}</span>
                    <div className="report-actions">
                        <button onClick={(e) => { navigator.clipboard.writeText(report.content); const b = e.currentTarget; b.textContent = 'Copied!'; setTimeout(() => b.textContent = 'Copy', 1000) }}>Copy</button>
                        <button onClick={() => onSaveReport(report.content, 'report.md')}>Save</button>
                        <button onClick={onClose}>Close</button>
                    </div>
                </div>
                <div className="report-fullscreen-content">
                    <ReactMarkdown
                        remarkPlugins={REMARK_PLUGINS}
                        rehypePlugins={REHYPE_PLUGINS}
                        urlTransform={urlTransform}
                        components={{img: ({src, alt}) => {
                            if (src?.startsWith('object:')) {
                                const id = src.slice(7)
                                return <ObjectImage id={id} alt={alt || ''} onClick={onLightbox} />
                            }
                            return <img src={src} alt={alt || ''} className="message-image" onClick={() => src && onLightbox(src)} />
                        }}}
                    >
                        {report.content}
                    </ReactMarkdown>
                </div>
            </div>
        </div>
    )
}
