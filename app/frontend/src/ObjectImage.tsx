import {useState, useEffect, useRef} from 'react'

// Cache for resolved object URLs (shared across components)
const objectCache: Record<string, string> = {}

interface Props {
    id: string
    alt?: string
    onClick?: (src: string) => void
}

// ObjectImage resolves object:ID URLs to data URLs via the backend.
// Design: docs/en/object-storage.md Section 7.2
export default function ObjectImage({id, alt, onClick}: Props) {
    const [src, setSrc] = useState<string | null>(objectCache[id] || null)
    const [error, setError] = useState(false)
    const mounted = useRef(true)

    useEffect(() => {
        mounted.current = true
        return () => { mounted.current = false }
    }, [])

    useEffect(() => {
        if (src || error) return
        if (objectCache[id]) {
            setSrc(objectCache[id])
            return
        }

        (async () => {
            try {
                if (window.go) {
                    console.log('[ObjectImage] resolving:', id)
                    const dataURL = await window.go.main.Bindings.GetImageDataURL(id)
                    console.log('[ObjectImage] result:', id, dataURL ? `OK (${dataURL.length} chars)` : 'EMPTY')
                    if (mounted.current && dataURL) {
                        objectCache[id] = dataURL
                        setSrc(dataURL)
                    } else if (mounted.current) {
                        setError(true)
                    }
                } else {
                    console.error('[ObjectImage] window.go not available')
                    if (mounted.current) setError(true)
                }
            } catch (e) {
                console.error('[ObjectImage] error:', id, e)
                if (mounted.current) setError(true)
            }
        })()
    }, [id, src, error])

    if (error) {
        return <span className="object-error" title={`Object ${id} not found`}>&#x1F5BC; {alt || id}</span>
    }
    if (!src) {
        return <span className="object-loading">Loading {alt || id}...</span>
    }
    return <img src={src} alt={alt || ''} className="message-image" onClick={() => onClick?.(src)} />
}

// clearObjectCache clears the cache (call on session switch)
export function clearObjectCache() {
    for (const key in objectCache) {
        delete objectCache[key]
    }
}
