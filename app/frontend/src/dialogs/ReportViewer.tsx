// ReportViewer is the full-screen markdown overlay for a stored
// report. Renders the same content the inline report card shows,
// but with more space and a sticky header for Copy / Save / Close.
//
// onExpandReport is invoked when the user clicks an inline
// [name](object:ID) chip whose target is a markdown / report —
// the current view is replaced (intentional v0.9.0 behaviour;
// ADR-0014 §4.3 documents the no-back-stack decision).

import {useMemo} from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeHighlight from 'rehype-highlight'
import rehypeKatex from 'rehype-katex'
import {urlTransform, objectComponents} from '../markdown/objectMarkdown'
import type {ExpandedReport} from '../types'

const REMARK_PLUGINS = [remarkGfm, remarkMath]
const REHYPE_PLUGINS = [rehypeHighlight, rehypeKatex]

interface Props {
    report: ExpandedReport;
    onClose: () => void;
    onLightbox: (src: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
    onSaveReport: (content: string, filename: string) => void;
}

export default function ReportViewer({report, onClose, onLightbox, onExpandReport, onSaveReport}: Props) {
    const components = useMemo(
        () => objectComponents({onLightbox, onExpandReport}),
        [onLightbox, onExpandReport],
    )
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
                        components={components}
                    >
                        {report.content}
                    </ReactMarkdown>
                </div>
            </div>
        </div>
    )
}
