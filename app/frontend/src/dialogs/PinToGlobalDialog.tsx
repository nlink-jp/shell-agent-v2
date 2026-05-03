// PinToGlobalDialog — category picker shown before promoting a
// Session Memory entry or a Finding into Global Memory.
//
// v0.2.0 Phase 9. The default category is "decision" because
// promotions usually come from a deliberate user choice rather
// than a stated preference; one click confirms.

import {useState} from 'react'

export type PinSource = 'session_memory' | 'finding'

interface Props {
    /** Short text shown to the user so they know what's about to be
     *  pinned. The dialog does not edit it — for now the entry is
     *  promoted as-is. */
    factPreview: string;
    sourceKind: PinSource;
    /** Default category radio. */
    defaultCategory?: 'preference' | 'decision';
    onConfirm: (category: 'preference' | 'decision') => void;
    onCancel: () => void;
}

export default function PinToGlobalDialog({
    factPreview,
    sourceKind,
    defaultCategory = 'decision',
    onConfirm,
    onCancel,
}: Props) {
    const [category, setCategory] = useState<'preference' | 'decision'>(defaultCategory)

    const sourceLabel = sourceKind === 'session_memory' ? 'Session Memory entry' : 'Finding'

    return (
        <div className="modal-backdrop" onClick={onCancel}>
            <div className="pin-dialog" onClick={e => e.stopPropagation()}>
                <div className="pin-dialog-header">
                    <span>Pin to Global Memory</span>
                    <button className="pin-dialog-close" onClick={onCancel}>&#x2715;</button>
                </div>
                <div className="pin-dialog-body">
                    <p className="pin-dialog-hint">
                        Promote this {sourceLabel} into cross-session Global Memory.
                        Choose the category that best fits how you want it remembered.
                    </p>
                    <blockquote className="pin-dialog-preview">{factPreview}</blockquote>
                    <fieldset className="pin-dialog-fieldset">
                        <legend>Category</legend>
                        <label>
                            <input
                                type="radio"
                                name="pin-category"
                                checked={category === 'preference'}
                                onChange={() => setCategory('preference')}
                            />
                            <span><strong>preference</strong> — a stable taste or way the user likes to work</span>
                        </label>
                        <label>
                            <input
                                type="radio"
                                name="pin-category"
                                checked={category === 'decision'}
                                onChange={() => setCategory('decision')}
                            />
                            <span><strong>decision</strong> — a chosen tool, dataset, approach, or commitment</span>
                        </label>
                    </fieldset>
                </div>
                <div className="pin-dialog-actions">
                    <button className="pin-dialog-cancel" onClick={onCancel}>Cancel</button>
                    <button className="pin-dialog-confirm" onClick={() => onConfirm(category)}>
                        Pin as {category}
                    </button>
                </div>
            </div>
        </div>
    )
}
