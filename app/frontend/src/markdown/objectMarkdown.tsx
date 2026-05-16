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
import {defaultUrlTransform} from 'react-markdown'
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
