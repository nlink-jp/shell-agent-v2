// objectMarkdown — single source of truth for object:-aware
// ReactMarkdown wiring. Hosts the URL sanitiser pass-through,
// the meta-lookup hook, and (added in Commit 3) the
// components-map factory consumed by every ReactMarkdown site.
//
// Why this module exists: prior to ADR-0014 the urlTransform +
// img-component override were duplicated across six sites
// (MessageItem ×3, App.tsx ×2, ReportViewer ×1). Adding the
// upcoming `a`-component override on top would have multiplied
// the drift surface. Centralising here lets every site import
// the same wiring; a single change propagates everywhere.
//
// Design: docs/en/adr/0014-object-link-rendering.md §3.1

import {useEffect, useRef, useState} from 'react'
import type {ReactNode} from 'react'
import {defaultUrlTransform} from 'react-markdown'
import type {Components} from 'react-markdown'
import ObjectImage from '../ObjectImage'
import ObjectLink from '../ObjectLink'
import type {ObjectInfo} from '../types'

// urlTransform passes `object:` URLs through ReactMarkdown's
// sanitiser. Drop-in replacement for the three+ inline copies
// the project carried pre-ADR-0014.
export function urlTransform(url: string): string {
    if (url.startsWith('object:')) return url
    return defaultUrlTransform(url)
}

// Shared in-memory cache of resolved object metadata. Keyed by
// object ID. Survives across components and ReactMarkdown
// renders so a 50-link report only pays the IPC cost once per
// distinct ID.
//
// Cleared on session switch via clearObjectMetaCache (called by
// the same code path that clears ObjectImage's data-URL cache).
const metaCache: Record<string, ObjectInfo> = {}
const metaPending: Record<string, Promise<ObjectInfo>> = {}

// clearObjectMetaCache empties the meta cache. Wire this into
// the session-switch path so a stale meta lookup from session A
// doesn't bleed into session B (objects in v2 are global, but
// the visibility filter is per-session — clearing keeps the
// reactive UI in step).
export function clearObjectMetaCache(): void {
    for (const key in metaCache) delete metaCache[key]
    for (const key in metaPending) delete metaPending[key]
}

// useObjectMeta returns the resolved ObjectInfo for an ID. The
// hook dedupes concurrent fetches (multiple components mounting
// in the same tick share one Promise) and caches across
// remounts. The three-tuple shape mirrors common React fetch
// hooks: callers branch on `loading` / `error` / present `meta`.
export function useObjectMeta(id: string): {
    meta: ObjectInfo | null;
    loading: boolean;
    error: boolean;
} {
    const [meta, setMeta] = useState<ObjectInfo | null>(metaCache[id] || null)
    const [error, setError] = useState(false)
    const mounted = useRef(true)

    useEffect(() => {
        mounted.current = true
        return () => { mounted.current = false }
    }, [])

    useEffect(() => {
        // Reset state when id changes. The cache check below
        // means a hit fills `meta` synchronously on the next
        // render.
        if (!id) {
            setMeta(null)
            setError(false)
            return
        }
        if (metaCache[id]) {
            setMeta(metaCache[id])
            setError(false)
            return
        }
        setMeta(null)
        setError(false)

        if (!window.go) {
            setError(true)
            return
        }

        // Dedupe concurrent fetches for the same id.
        let p = metaPending[id]
        if (!p) {
            p = window.go.main.Bindings.GetObjectMeta(id)
            metaPending[id] = p
        }
        p.then(info => {
            metaCache[id] = info
            delete metaPending[id]
            if (mounted.current) setMeta(info)
        }).catch(() => {
            delete metaPending[id]
            if (mounted.current) setError(true)
        })
    }, [id])

    return {meta, loading: !meta && !error, error}
}

// FactoryOpts captures the callbacks the override components
// need. The factory is invoked from every ReactMarkdown site
// inside a useMemo so the components reference is stable across
// renders, otherwise ReactMarkdown would re-parse the message
// on every parent re-render.
interface FactoryOpts {
    onLightbox: (src: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
}

// objectComponents builds the ReactMarkdown components map with
// object:-aware img and a overrides. Both forms normalise to the
// referenced object's actual type: an `![alt](object:ID)` where
// ID is a markdown document renders as a chip (not a broken-image
// glyph), and a `[label](object:ID)` where ID is an image renders
// inline (not a dead anchor). The LLM-supplied label / alt is
// preserved as the visible text in either case.
//
// Design: docs/en/adr/0014-object-link-rendering.md §3.1.2 / §3.1.3
export function objectComponents(opts: FactoryOpts): Components {
    const {onLightbox, onExpandReport} = opts
    return {
        img: ({src, alt}) => {
            if (typeof src === 'string' && src.startsWith('object:')) {
                return (
                    <ObjectRef
                        id={src.slice(7)}
                        label={alt || ''}
                        onLightbox={onLightbox}
                        onExpandReport={onExpandReport}
                    />
                )
            }
            return (
                <img
                    src={src}
                    alt={alt || ''}
                    className="message-image"
                    onClick={() => { if (typeof src === 'string') onLightbox(src) }}
                />
            )
        },
        a: ({href, children}) => {
            if (typeof href === 'string' && href.startsWith('object:')) {
                return (
                    <ObjectRef
                        id={href.slice(7)}
                        label={children}
                        onLightbox={onLightbox}
                        onExpandReport={onExpandReport}
                    />
                )
            }
            return <a href={href} target="_blank" rel="noreferrer">{children}</a>
        },
    }
}

// ObjectRef is the shared dispatch point for an `object:` URL —
// the same logic backs the `img` and `a` overrides. Resolves
// the meta once, then renders ObjectImage for image-type IDs
// (data URL → inline <img> + lightbox affordance) or
// ObjectLink for everything else (markdown / report / blob chip
// with a type-appropriate click handler).
//
// Type-mismatched LLM input is normalised here: `![…](object:ID)`
// where ID is a markdown document falls through to the chip
// branch, and `[…](object:ID)` where ID is an image falls through
// to the inline-image branch. Either way, the LLM's `alt` /
// link-children text is preserved as the visible label.
function ObjectRef({id, label, onLightbox, onExpandReport}: {
    id: string;
    label: ReactNode;
    onLightbox: (src: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
}) {
    const {meta, loading, error} = useObjectMeta(id)

    if (loading) {
        return <span className="object-loading">Loading…</span>
    }
    if (error || !meta) {
        const labelText = typeof label === 'string' && label.trim()
            ? label
            : id.slice(0, 8)
        return (
            <span className="object-error object-link-missing" title={`Object ${id} not found`}>
                {labelText}
            </span>
        )
    }
    if (meta.type === 'image') {
        const alt = typeof label === 'string' ? label : ''
        return <ObjectImage id={id} alt={alt} onClick={onLightbox} />
    }
    return (
        <ObjectLink
            meta={meta}
            label={label}
            onExpandReport={onExpandReport}
        />
    )
}
