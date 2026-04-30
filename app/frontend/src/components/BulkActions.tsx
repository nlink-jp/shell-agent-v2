// BulkActions renders the small toolbar above a selectable list.
// Delete uses a two-click confirm pattern (the Wails webview may
// not surface native window.confirm dialogs reliably, so we keep
// the confirmation in-component).
//
// onPrepareConfirm, if provided, is awaited on the first click and
// its returned string overrides the confirming-state button text —
// used historically by the Objects panel to surface "N still
// referenced" before the user committed to deletion.

import {useEffect, useState} from 'react'

interface BulkActionsProps {
    total: number;
    selectedCount: number;
    onSelectAll: () => void;
    onClear: () => void;
    onDelete: () => void;
    onPrepareConfirm?: () => Promise<string>;
}

export default function BulkActions({total, selectedCount, onSelectAll, onClear, onDelete, onPrepareConfirm}: BulkActionsProps) {
    const [confirming, setConfirming] = useState(false)
    const [confirmLabel, setConfirmLabel] = useState('Confirm')
    useEffect(() => {
        if (!confirming) return
        const t = setTimeout(() => setConfirming(false), 6000)
        return () => clearTimeout(t)
    }, [confirming])
    useEffect(() => { if (selectedCount === 0) setConfirming(false) }, [selectedCount])

    if (total === 0) return null
    const allSelected = selectedCount === total && total > 0
    return (
        <div className="bulk-actions">
            {selectedCount > 0 ? (
                <>
                    <span className="bulk-count">{selectedCount} selected</span>
                    <button
                        className={`bulk-btn bulk-btn-danger ${confirming ? 'confirming' : ''}`}
                        onClick={async () => {
                            if (confirming) { onDelete(); setConfirming(false); return }
                            if (onPrepareConfirm) {
                                const label = await onPrepareConfirm()
                                setConfirmLabel(label || 'Confirm')
                            } else {
                                setConfirmLabel('Confirm')
                            }
                            setConfirming(true)
                        }}
                        title={confirming ? `Click again to delete ${selectedCount} item(s)` : `Delete ${selectedCount} selected`}
                    >
                        {confirming ? confirmLabel : 'Delete'}
                    </button>
                    <button className="bulk-btn" onClick={onClear}>Clear</button>
                </>
            ) : (
                <button className="bulk-btn" onClick={onSelectAll} disabled={allSelected}>Select all</button>
            )}
        </div>
    )
}
