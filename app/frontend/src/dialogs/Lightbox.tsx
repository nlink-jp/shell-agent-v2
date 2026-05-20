// Lightbox shows a single full-screen image overlay. Clicking
// outside the image or the close button dismisses; clicking the
// image itself does nothing (stopPropagation).
//
// Issue #8: when an objectId is provided, an Export button appears
// in the top-right corner alongside Close. Clicking it routes through
// the existing Bindings.ExportObject native-save-dialog flow used by
// the Data panel. Raw-URL lightboxes (e.g. data URLs paste-from-
// clipboard before they're persisted) get no Export button — we
// don't have a backend object handle to hand to ExportObject.

interface Props {
    src: string;
    objectId?: string;
    onClose: () => void;
}

export default function Lightbox({src, objectId, onClose}: Props) {
    return (
        <div className="lightbox-overlay" onClick={onClose}>
            <img src={src} alt="" className="lightbox-image" onClick={e => e.stopPropagation()} />
            <div className="lightbox-actions" onClick={e => e.stopPropagation()}>
                {objectId && (
                    <button className="lightbox-export" onClick={(e) => {
                        // Issue #9 lesson: blur first so the Enter
                        // pressed in the native save dialog can't
                        // re-trigger this button when focus returns.
                        e.currentTarget.blur()
                        if (window.go?.main?.Bindings?.ExportObject) {
                            window.go.main.Bindings.ExportObject(objectId).catch(() => {})
                        }
                    }} title="Save image to disk">Export</button>
                )}
                <button className="lightbox-close" onClick={onClose} title="Close">&#x2715;</button>
            </div>
        </div>
    )
}
