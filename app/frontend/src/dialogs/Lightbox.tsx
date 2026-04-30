// Lightbox shows a single full-screen image overlay. Clicking
// outside the image or the close button dismisses; clicking the
// image itself does nothing (stopPropagation).

interface Props {
    src: string;
    onClose: () => void;
}

export default function Lightbox({src, onClose}: Props) {
    return (
        <div className="lightbox-overlay" onClick={onClose}>
            <img src={src} alt="" className="lightbox-image" onClick={e => e.stopPropagation()} />
            <button className="lightbox-close" onClick={onClose}>&#x2715;</button>
        </div>
    )
}
