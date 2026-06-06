// Sidebar — two-section accordion (Sessions / Memory) plus a
// bottom-nav (New Chat / Settings / Collapse). Same DOM is used
// for both expanded and collapsed modes; the `is-collapsed`
// class on the wrapper hides labels and content panels via CSS,
// keeping icon Y-positions and section dividers identical
// between modes.
//
// State that's only relevant to the sidebar (rename in-progress,
// per-list bulk-select sets, IME composition guard) lives here.
// The "big" state — the sessions list, findings, global / session
// memory — is owned by App and passed in as props; Sidebar
// forwards changes back via setter / handler props.
//
// v0.2.0: the Memory tab now has TWO sub-sections (Global /
// Session) instead of one Pinned section. Findings still appear
// here in Phase 7; Phase 8 moves them to a dedicated
// FindingsDisclosure panel in the chat pane.

import {useEffect, useRef, useState} from 'react'
import BulkActions from '../components/BulkActions'
import type {GlobalMemory, SessionInfo, SessionMemory, SidebarPanel} from '../types'

// Trust badge surfaces the v0.1.26 memory-injection provenance
// fields. "user-stated" facts came from a user turn, manual pin,
// or a deliberate promotion (promoted_from_*) and are the
// trustable category. "derived" covers assistant-turn extraction
// and legacy entries (no Source set), both of which may carry
// attacker-influenced content from tool output. See
// docs/en/memory-model.md.
function globalMemoryTrust(source?: string): {label: string; cls: string} {
    if (
        source === 'user_turn' ||
        source === 'manual' ||
        source === 'promoted_from_session_memory' ||
        source === 'promoted_from_finding'
    ) {
        return {label: 'user-stated', cls: 'trust-user'}
    }
    return {label: 'derived', cls: 'trust-derived'}
}
function sessionMemoryTrust(source?: string): {label: string; cls: string} {
    if (source === 'user_turn') {
        return {label: 'user-stated', cls: 'trust-user'}
    }
    return {label: 'derived', cls: 'trust-derived'}
}

// ADR-0031 lifecycle badges. The state string comes verbatim from
// the backend's DeriveState; an empty value (e.g. legacy entry
// loaded before the first DecayAll) falls through to no badge, so
// nothing visually changes on a brand-new install.
function stateBadge(state?: string): {label: string; cls: string; tip: string} | null {
    switch (state) {
        case 'fresh':
            return {
                label: 'fresh',
                cls: 'state-fresh',
                tip: 'fresh: 直近に作成 / 更新された fact。常に system prompt に注入。',
            }
        case 'active':
            return {
                label: 'active',
                cls: 'state-active',
                tip: 'active: relevance が active 閾値以上。system prompt に注入。',
            }
        case 'dormant':
            return {
                label: 'dormant',
                cls: 'state-dormant',
                tip: 'dormant: relevance が低下。system prompt には出さない。touch されれば active に戻る。',
            }
        case 'archived':
            return {
                label: 'archived',
                cls: 'state-archived',
                tip: 'archived: relevance が下限を下回った。system prompt から完全に除外。cap 圧迫時に最優先で eviction。',
            }
        default:
            return null
    }
}

interface Props {
    // Layout / collapse
    sidebarPanel: SidebarPanel;
    setSidebarPanel: (p: SidebarPanel) => void;
    sidebarCollapsed: boolean;
    setSidebarCollapsed: (v: boolean | ((c: boolean) => boolean)) => void;
    sidebarWidth: number;
    onStartResize: (e: React.MouseEvent) => void;

    // Session list
    sessions: SessionInfo[];
    currentSessionId: string;
    /** v0.3.0: when the active session is private, hide the
     *  ★ Pin to Global Memory buttons (the binding rejects
     *  promotion regardless, this just keeps the UI honest). */
    currentSessionPrivate: boolean;
    busy: boolean;
    onLoadSession: (id: string) => void;
    onNewSession: () => void;
    onNewPrivateSession: () => void;
    /** v0.4.0: per-row Export action + bottom-nav Import button.
     *  Export is hidden while busy (the binding would error anyway). */
    onExportSession: (id: string) => void;
    onImportSession: () => void;
    /** v0.4.2 (#6): returns a promise so the Sidebar can show a
     *  Deleting state on the row until the backend call settles. */
    onDeleteSession: (id: string) => Promise<void>;
    onRenameSession: (id: string, title: string) => void;

    // Memory panel data. Findings moved to FindingsDisclosure
    // (chat-pane panel) in v0.2.0 Phase 8.
    globalMemories: GlobalMemory[];
    onGlobalMemoryDelete: (facts: string[]) => Promise<void>;
    onGlobalMemoryDeleteOne: (fact: string) => Promise<void>;
    /** ADR-0027: export the whole Global Memory to a JSON file /
     *  import (merge, skip duplicates) from one. */
    onExportGlobalMemory: () => void;
    onImportGlobalMemory: () => void;
    sessionMemories: SessionMemory[];
    onSessionMemoryDelete: (facts: string[]) => Promise<void>;
    onPinSessionMemory: (fact: string) => void;

    // Settings
    onOpenSettings: () => void;
}

export default function Sidebar({
    sidebarPanel, setSidebarPanel,
    sidebarCollapsed, setSidebarCollapsed,
    sidebarWidth, onStartResize,
    sessions, currentSessionId, currentSessionPrivate, busy,
    onLoadSession, onNewSession, onNewPrivateSession,
    onExportSession, onImportSession,
    onDeleteSession, onRenameSession,
    globalMemories, onGlobalMemoryDelete, onGlobalMemoryDeleteOne,
    onExportGlobalMemory, onImportGlobalMemory,
    sessionMemories, onSessionMemoryDelete, onPinSessionMemory,
    onOpenSettings,
}: Props) {
    // Sidebar-local: rename UI
    const [editingSession, setEditingSession] = useState<string | null>(null)
    const [editTitle, setEditTitle] = useState('')

    // Sidebar-local: 2-click confirm + delete-in-flight states
    // (v0.4.2 #6). At most one row can be in confirm at a time;
    // the second click on the same row's X actually invokes the
    // delete binding, which the parent awaits. While in flight,
    // the row is greyed and its action buttons disabled. See
    // docs/en/session-delete-ux.md.
    const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null)
    const [deletingSession, setDeletingSession] = useState<string | null>(null)
    const confirmTimerRef = useRef<number | null>(null)
    const clearConfirmTimer = () => {
        if (confirmTimerRef.current !== null) {
            window.clearTimeout(confirmTimerRef.current)
            confirmTimerRef.current = null
        }
    }
    const armConfirm = (id: string) => {
        clearConfirmTimer()
        setConfirmingDelete(id)
        // 6 s window — matches the BulkActions confirm timeout
        // (Findings / Global Memory / Session Memory bulk-delete)
        // so the user's expectation set by other delete UIs in the
        // app carries over here.
        confirmTimerRef.current = window.setTimeout(() => {
            setConfirmingDelete(null)
            confirmTimerRef.current = null
        }, 6000)
    }
    const cancelConfirm = () => {
        clearConfirmTimer()
        setConfirmingDelete(null)
    }
    // Click-outside the confirming row clears the confirm. Listen
    // on the document so any other interaction (rename click,
    // sidebar nav button, etc.) cancels.
    useEffect(() => {
        if (confirmingDelete === null) return
        const onDocClick = (ev: MouseEvent) => {
            const target = ev.target as HTMLElement | null
            if (!target) return
            // Don't cancel if the click is on the confirm button itself
            // — its own onClick handles the second-click commit.
            if (target.closest(`[data-confirm-row="${confirmingDelete}"]`)) return
            cancelConfirm()
        }
        document.addEventListener('click', onDocClick)
        return () => document.removeEventListener('click', onDocClick)
    }, [confirmingDelete])

    // Sidebar-local: bulk-select sets per list
    const [selectedGlobalFacts, setSelectedGlobalFacts] = useState<Set<string>>(new Set())
    const [selectedSessionFacts, setSelectedSessionFacts] = useState<Set<string>>(new Set())

    // ADR-0031: archived entries are hidden by default; user can
    // expand to inspect / delete them. State per list (the two
    // memory pools are independent).
    const [showArchivedGlobal, setShowArchivedGlobal] = useState(false)
    const [showArchivedSession, setShowArchivedSession] = useState(false)

    const handleDeleteClick = async (id: string) => {
        if (deletingSession !== null) return // already deleting something
        if (confirmingDelete !== id) {
            armConfirm(id)
            return
        }
        // Second click on the same row's X — actually delete.
        cancelConfirm()
        setDeletingSession(id)
        try {
            await onDeleteSession(id)
        } finally {
            // Whether the delete succeeded or rejected (e.g.
            // ErrBusy from a stray post-task), clear the in-flight
            // marker. The parent App.tsx is responsible for
            // refreshing the session list and switching session if
            // the active one was deleted.
            setDeletingSession(null)
        }
    }

    const startRename = (id: string, currentTitle: string) => {
        setEditingSession(id)
        setEditTitle(currentTitle)
    }
    const commitRename = () => {
        if (!editingSession || !editTitle.trim()) {
            setEditingSession(null)
            return
        }
        onRenameSession(editingSession, editTitle.trim())
        setEditingSession(null)
    }

    return (
        <>
            <div
                className={`sidebar ${sidebarCollapsed ? 'is-collapsed' : ''}`}
                style={sidebarCollapsed ? undefined : {width: sidebarWidth}}
            >
                <div className="sidebar-accordion">
                    <section className={`acc-section ${sidebarPanel === 'sessions' ? 'expanded' : 'collapsed'}`}>
                        <button
                            className={`sidebar-nav-btn ${sidebarPanel === 'sessions' ? 'active' : ''}`}
                            onClick={() => { if (sidebarCollapsed) setSidebarCollapsed(false); setSidebarPanel('sessions') }}
                            title="Sessions"
                        >
                            <span className="sidebar-nav-ic">&#x2630;</span>
                            <span className="sidebar-nav-label">Sessions</span>
                        </button>
                        {sidebarPanel === 'sessions' && (
                        <div className="acc-content">
                            {sessions.length === 0 ? (
                                <p className="sidebar-hint">No sessions yet</p>
                            ) : sessions.map(s => {
                                const isConfirming = confirmingDelete === s.id
                                const isDeleting = deletingSession === s.id
                                const rowClass = `session-item${s.id === currentSessionId ? ' active' : ''}${isDeleting ? ' is-deleting' : ''}`
                                const rowProps: React.HTMLAttributes<HTMLDivElement> & {[k: string]: any} = {
                                    className: rowClass,
                                    onClick: isDeleting ? undefined : () => onLoadSession(s.id),
                                }
                                // data-confirm-row lets the document-level
                                // click-outside listener distinguish "clicked
                                // inside the confirming row" (don't cancel)
                                // from "clicked anywhere else" (cancel).
                                if (isConfirming) {
                                    rowProps['data-confirm-row'] = s.id
                                }
                                return (
                                <div key={s.id} {...rowProps}>
                                    {editingSession === s.id ? (
                                        <input
                                            className="session-title-edit"
                                            value={editTitle}
                                            onChange={e => setEditTitle(e.target.value)}
                                            onBlur={commitRename}
                                            onKeyDown={e => {
                                                // IME composition guard: the ENTER that confirms a
                                                // Japanese candidate must not commit the rename. We use
                                                // the native `isComposing` flag plus the keyCode === 229
                                                // legacy guard so both modern and older WebKit honour
                                                // the IME state. preventDefault on the post-composition
                                                // ENTER is what kills the macOS "rejected key" beep —
                                                // without it, the ENTER bubbles into the unhandled
                                                // default action on the now-detached input. See issue
                                                // #7 for the reproduction.
                                                if (e.nativeEvent.isComposing || e.keyCode === 229) return
                                                if (e.key === 'Enter') { e.preventDefault(); commitRename() }
                                                if (e.key === 'Escape') { e.preventDefault(); setEditingSession(null) }
                                            }}
                                            autoFocus
                                            onClick={e => e.stopPropagation()}
                                        />
                                    ) : (
                                        <div className="session-title" onDoubleClick={isDeleting ? undefined : (e) => { e.stopPropagation(); startRename(s.id, s.title) }}>
                                            {isDeleting && <span className="session-deleting-spinner" aria-label="Deleting">↻</span>}
                                            {s.private && <span className="session-private-icon" title="Private session — Global Memory promotion suppressed">🔒</span>}
                                            {isDeleting ? 'Deleting…' : s.title}
                                        </div>
                                    )}
                                    <div className="session-meta">
                                        <span className="session-date">{isDeleting ? '' : s.updated_at}</span>
                                        <div className="session-actions">
                                            <button onClick={(e) => { e.stopPropagation(); startRename(s.id, s.title) }} disabled={isDeleting || isConfirming} title="Rename">&#x270E;</button>
                                            <button
                                                onClick={(e) => { e.stopPropagation(); onExportSession(s.id) }}
                                                disabled={busy || isDeleting || isConfirming}
                                                title={busy ? 'Cannot export while agent is busy' : 'Export session as .shellagent bundle'}
                                            >&#x2B07;</button>
                                            <button
                                                onClick={(e) => { e.stopPropagation(); handleDeleteClick(s.id) }}
                                                disabled={isDeleting || (deletingSession !== null && !isConfirming)}
                                                className={isConfirming ? 'session-delete-confirm' : undefined}
                                                title={isConfirming ? `Click again to delete \"${s.title}\"` : 'Delete'}
                                            >{isConfirming ? 'Confirm' : '✕'}</button>
                                        </div>
                                    </div>
                                </div>
                                )
                            })}
                        </div>
                        )}
                    </section>
                    <section className={`acc-section ${sidebarPanel === 'memory' ? 'expanded' : 'collapsed'}`}>
                        <button
                            className={`sidebar-nav-btn ${sidebarPanel === 'memory' ? 'active' : ''}`}
                            onClick={() => { if (sidebarCollapsed) setSidebarCollapsed(false); setSidebarPanel('memory') }}
                            title="Memory"
                        >
                            <span className="sidebar-nav-ic">&#x2605;</span>
                            <span className="sidebar-nav-label">Memory</span>
                        </button>
                        {sidebarPanel === 'memory' && (
                        <div className="acc-content">
                            <div className={`status-section ${selectedGlobalFacts.size > 0 ? 'bulk-active' : ''}`}>
                                <div className="bulk-section-header">
                                    <h3>Global Memory</h3>
                                    <div className="memory-io-actions">
                                        <button
                                            onClick={onImportGlobalMemory}
                                            title="Import Global Memory from a JSON file (merge, skip duplicates)"
                                        >Import</button>
                                        {globalMemories.length > 0 && (
                                            <button
                                                onClick={onExportGlobalMemory}
                                                title="Export all Global Memory to a JSON file"
                                            >Export</button>
                                        )}
                                    </div>
                                    {globalMemories.length > 0 && (
                                        <BulkActions
                                            total={globalMemories.length}
                                            selectedCount={selectedGlobalFacts.size}
                                            onSelectAll={() => setSelectedGlobalFacts(new Set(globalMemories.map(p => p.fact)))}
                                            onClear={() => setSelectedGlobalFacts(new Set())}
                                            onDelete={async () => {
                                                const facts = Array.from(selectedGlobalFacts)
                                                if (facts.length === 0) return
                                                await onGlobalMemoryDelete(facts)
                                                setSelectedGlobalFacts(new Set())
                                            }}
                                        />
                                    )}
                                </div>
                                {globalMemories.length === 0 ? (
                                    <p className="sidebar-hint">No global memories</p>
                                ) : (() => {
                                    const archivedCount = globalMemories.filter(p => p.state === 'archived').length
                                    const visible = globalMemories.filter(p => showArchivedGlobal || p.state !== 'archived')
                                    return (
                                        <>
                                            {visible.map((p, i) => (
                                                <div key={i} className={`pinned-item ${selectedGlobalFacts.has(p.fact) ? 'selected' : ''}`}>
                                                    <input
                                                        type="checkbox"
                                                        className="bulk-check"
                                                        checked={selectedGlobalFacts.has(p.fact)}
                                                        onChange={e => {
                                                            const next = new Set(selectedGlobalFacts)
                                                            if (e.target.checked) next.add(p.fact); else next.delete(p.fact)
                                                            setSelectedGlobalFacts(next)
                                                        }}
                                                    />
                                                    <span className={`pinned-category ${p.category}`}>{p.category}</span>
                                                    {(() => {
                                                        const t = globalMemoryTrust(p.source)
                                                        const tip = t.cls === 'trust-user'
                                                            ? 'user-stated: ユーザー発話または手動 pin / promotion 由来の fact。高信頼。'
                                                            : 'derived: アシスタント発話から抽出された fact、または source 未設定の legacy entry。内容は LLM 経由でツール出力 (CSV セル / MCP 応答 / 画像 OCR / Web 取得) を含みうる。'
                                                        return (
                                                            <span className={`trust-badge ${t.cls}`} data-tooltip={tip}>
                                                                {t.label}
                                                            </span>
                                                        )
                                                    })()}
                                                    {(() => {
                                                        const b = stateBadge(p.state)
                                                        if (!b) return null
                                                        return (
                                                            <span className={`state-badge ${b.cls}`} data-tooltip={b.tip}>
                                                                {b.label}
                                                            </span>
                                                        )
                                                    })()}
                                                    <div className="pinned-content">
                                                        <span className="pinned-fact">{p.native_fact || p.fact}</span>
                                                        {p.native_fact && p.native_fact !== p.fact && (
                                                            <span className="pinned-fact-en">{p.fact}</span>
                                                        )}
                                                        {p.created_at && (
                                                            <span className="pinned-date">learned {p.created_at.slice(0, 10)}</span>
                                                        )}
                                                    </div>
                                                    {typeof p.relevance === 'number' && (
                                                        <div
                                                            className="relevance-bar"
                                                            style={{width: `${Math.max(0, Math.min(1, p.relevance)) * 100}%`}}
                                                            data-tooltip={`relevance ${p.relevance.toFixed(2)}`}
                                                        />
                                                    )}
                                                    <button className="pinned-delete" onClick={() => onGlobalMemoryDeleteOne(p.fact)}>&#x2715;</button>
                                                </div>
                                            ))}
                                            {archivedCount > 0 && (
                                                <button
                                                    className="sidebar-archived-toggle"
                                                    onClick={() => setShowArchivedGlobal(v => !v)}
                                                >
                                                    {showArchivedGlobal
                                                        ? `Hide archived (${archivedCount})`
                                                        : `Show archived (${archivedCount})`}
                                                </button>
                                            )}
                                        </>
                                    )
                                })()}
                            </div>
                            <div className={`status-section ${selectedSessionFacts.size > 0 ? 'bulk-active' : ''}`}>
                                <div className="bulk-section-header">
                                    <h3>Session Memory</h3>
                                    {sessionMemories.length > 0 && (
                                        <BulkActions
                                            total={sessionMemories.length}
                                            selectedCount={selectedSessionFacts.size}
                                            onSelectAll={() => setSelectedSessionFacts(new Set(sessionMemories.map(p => p.fact)))}
                                            onClear={() => setSelectedSessionFacts(new Set())}
                                            onDelete={async () => {
                                                const facts = Array.from(selectedSessionFacts)
                                                if (facts.length === 0) return
                                                await onSessionMemoryDelete(facts)
                                                setSelectedSessionFacts(new Set())
                                            }}
                                        />
                                    )}
                                </div>
                                {sessionMemories.length === 0 ? (
                                    <p className="sidebar-hint">No session memory yet</p>
                                ) : (() => {
                                    const archivedCount = sessionMemories.filter(p => p.state === 'archived').length
                                    const visible = sessionMemories.filter(p => showArchivedSession || p.state !== 'archived')
                                    return (
                                        <>
                                            {visible.map((p, i) => (
                                                <div key={i} className={`pinned-item ${selectedSessionFacts.has(p.fact) ? 'selected' : ''}`}>
                                                    <input
                                                        type="checkbox"
                                                        className="bulk-check"
                                                        checked={selectedSessionFacts.has(p.fact)}
                                                        onChange={e => {
                                                            const next = new Set(selectedSessionFacts)
                                                            if (e.target.checked) next.add(p.fact); else next.delete(p.fact)
                                                            setSelectedSessionFacts(next)
                                                        }}
                                                    />
                                                    <span className={`pinned-category ${p.category}`}>{p.category}</span>
                                                    {(() => {
                                                        const t = sessionMemoryTrust(p.source)
                                                        const tip = t.cls === 'trust-user'
                                                            ? 'user-stated: ユーザー発話由来の fact。高信頼。'
                                                            : 'derived: アシスタント発話から抽出された fact。LLM 経由でツール出力を含みうる。'
                                                        return (
                                                            <span className={`trust-badge ${t.cls}`} data-tooltip={tip}>
                                                                {t.label}
                                                            </span>
                                                        )
                                                    })()}
                                                    {(() => {
                                                        const b = stateBadge(p.state)
                                                        if (!b) return null
                                                        return (
                                                            <span className={`state-badge ${b.cls}`} data-tooltip={b.tip}>
                                                                {b.label}
                                                            </span>
                                                        )
                                                    })()}
                                                    <div className="pinned-content">
                                                        <span className="pinned-fact">{p.native_fact || p.fact}</span>
                                                        {p.native_fact && p.native_fact !== p.fact && (
                                                            <span className="pinned-fact-en">{p.fact}</span>
                                                        )}
                                                        {p.created_at && (
                                                            <span className="pinned-date">learned {p.created_at.slice(0, 10)}</span>
                                                        )}
                                                    </div>
                                                    {typeof p.relevance === 'number' && (
                                                        <div
                                                            className="relevance-bar"
                                                            style={{width: `${Math.max(0, Math.min(1, p.relevance)) * 100}%`}}
                                                            data-tooltip={`relevance ${p.relevance.toFixed(2)}`}
                                                        />
                                                    )}
                                                    {!currentSessionPrivate && (
                                                        <button
                                                            className="pinned-pin"
                                                            title="Pin to Global Memory"
                                                            onClick={() => onPinSessionMemory(p.fact)}
                                                        >
                                                            &#x2605;
                                                        </button>
                                                    )}
                                                </div>
                                            ))}
                                            {archivedCount > 0 && (
                                                <button
                                                    className="sidebar-archived-toggle"
                                                    onClick={() => setShowArchivedSession(v => !v)}
                                                >
                                                    {showArchivedSession
                                                        ? `Hide archived (${archivedCount})`
                                                        : `Show archived (${archivedCount})`}
                                                </button>
                                            )}
                                        </>
                                    )
                                })()}
                            </div>
                            {/* Tokens section moved to chat-pane footer in
                               info-display redesign Phase 4 — telemetry isn't
                               navigable content. */}
                        </div>
                        )}
                    </section>
                </div>

                <div className="sidebar-bottom">
                    <button className="sidebar-nav-btn" onClick={onNewSession} disabled={busy} title="New Chat">
                        <span className="sidebar-nav-ic">+</span>
                        <span className="sidebar-nav-label">New Chat</span>
                    </button>
                    <button
                        className="sidebar-nav-btn"
                        onClick={onNewPrivateSession}
                        disabled={busy}
                        title="New Private Chat — Global Memory promotion suppressed for this conversation"
                    >
                        <span className="sidebar-nav-ic">🔒</span>
                        <span className="sidebar-nav-label">New Private Chat</span>
                    </button>
                    <button
                        className="sidebar-nav-btn"
                        onClick={onImportSession}
                        disabled={busy}
                        title="Import a .shellagent session bundle"
                    >
                        <span className="sidebar-nav-ic">&#x2B06;</span>
                        <span className="sidebar-nav-label">Import Chat</span>
                    </button>
                    <div className="sidebar-nav-divider" />
                    <button className="sidebar-nav-btn" onClick={onOpenSettings} title="Settings">
                        <span className="sidebar-nav-ic">&#x2699;</span>
                        <span className="sidebar-nav-label">Settings</span>
                    </button>
                    <div className="sidebar-nav-divider" />
                    <button
                        className="sidebar-nav-btn"
                        onClick={() => setSidebarCollapsed(c => !c)}
                        title={sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar'}
                    >
                        <span className="sidebar-nav-ic">{sidebarCollapsed ? '▶' : '◀'}</span>
                    </button>
                </div>
            </div>
            {!sidebarCollapsed && <div className="sidebar-resize" onMouseDown={onStartResize} />}
        </>
    )
}
