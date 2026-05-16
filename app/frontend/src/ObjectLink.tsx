// ObjectLink renders an inline chip for a non-image object
// reference (markdown / report / blob). Sibling of ObjectImage.
//
// The chip carries the LLM-supplied label (the link text in
// `[label](object:ID)` or the `alt` in `![label](object:ID)`
// when the latter targets a non-image), falling back to the
// object's original filename and finally to a short ID prefix.
//
// Click dispatch:
//   - markdown / report → GetObjectText → strip leading `# ` from
//     the first line for the title → onExpandReport, reusing the
//     existing ReportViewer that App.tsx mounts at root.
//   - blob → ExportObject save-as dialog.
//
// Used by the components factory in markdown/objectMarkdown.tsx;
// not exported from anywhere else.
//
// Design: docs/en/adr/0014-object-link-rendering.md §3.1.4

import type {ReactNode} from 'react'
import type {ObjectInfo} from './types'

interface Props {
    meta: ObjectInfo;
    label: ReactNode;
    onExpandReport: (r: {title: string; content: string}) => void;
}

export default function ObjectLink({meta, label, onExpandReport}: Props) {
    // Prefer LLM-supplied label (string only — ReactMarkdown often
    // hands us a fragment of [span, …] for nested formatting; in
    // that case fall back to the object metadata).
    const labelText = typeof label === 'string' && label.trim()
        ? label
        : (meta.orig_name || meta.id.slice(0, 8))

    const icon = meta.type === 'report' ? '\u{1F4C4}'    // 📄
        : meta.type === 'markdown' ? '\u{1F4DD}'         // 📝
        : '\u{1F4CE}'                                     // 📎 (blob / fallback)

    const handleClick = async () => {
        if (!window.go) return
        try {
            if (meta.type === 'markdown' || meta.type === 'report') {
                const content = await window.go.main.Bindings.GetObjectText(meta.id)
                const firstLine = (content.split('\n')[0] || '').replace(/^#\s*/, '')
                const title = firstLine || meta.orig_name || meta.id || 'Document'
                onExpandReport({title, content})
            } else if (meta.type === 'blob') {
                await window.go.main.Bindings.ExportObject(meta.id)
            }
        } catch {
            // Swallow — meta presence was already verified before
            // mount, so the failure is either a deletion racing
            // with the click or a user-cancelled save dialog. The
            // chip stays mounted; another click retries.
        }
    }

    const tooltip = meta.orig_name
        ? `${meta.orig_name} — click to open`
        : `${meta.id} — click to open`

    return (
        <button
            type="button"
            className={`object-link-chip object-link-${meta.type}`}
            onClick={handleClick}
            title={tooltip}
        >
            <span className="object-link-icon" aria-hidden>{icon}</span>
            <span className="object-link-label">{labelText}</span>
        </button>
    )
}
