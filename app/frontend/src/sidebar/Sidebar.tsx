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

import {useRef, useState} from 'react'
import BulkActions from '../components/BulkActions'
import type {Finding, GlobalMemory, SessionInfo, SessionMemory, SidebarPanel} from '../types'

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
function findingTrust(source?: string): {label: string; cls: string} {
    if (source === 'manual') {
        return {label: 'user-stated', cls: 'trust-user'}
    }
    return {label: 'derived', cls: 'trust-derived'}
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
    busy: boolean;
    onLoadSession: (id: string) => void;
    onNewSession: () => void;
    onDeleteSession: (id: string) => void;
    onRenameSession: (id: string, title: string) => void;

    // Memory panel data
    findings: Finding[];
    onFindingsDelete: (ids: string[]) => Promise<void>;
    globalMemories: GlobalMemory[];
    onGlobalMemoryDelete: (facts: string[]) => Promise<void>;
    onGlobalMemoryDeleteOne: (fact: string) => Promise<void>;
    sessionMemories: SessionMemory[];
    onSessionMemoryDelete: (facts: string[]) => Promise<void>;
    onPinSessionMemory: (fact: string, category: string) => Promise<void>;

    // Settings
    onOpenSettings: () => void;
}

export default function Sidebar({
    sidebarPanel, setSidebarPanel,
    sidebarCollapsed, setSidebarCollapsed,
    sidebarWidth, onStartResize,
    sessions, currentSessionId, busy,
    onLoadSession, onNewSession, onDeleteSession, onRenameSession,
    findings, onFindingsDelete,
    globalMemories, onGlobalMemoryDelete, onGlobalMemoryDeleteOne,
    sessionMemories, onSessionMemoryDelete, onPinSessionMemory,
    onOpenSettings,
}: Props) {
    // Sidebar-local: rename UI
    const [editingSession, setEditingSession] = useState<string | null>(null)
    const [editTitle, setEditTitle] = useState('')
    // IME composition guard so Enter during conversion doesn't
    // commit a half-typed Japanese title.
    const composingRef = useRef(false)

    // Sidebar-local: bulk-select sets per list
    const [selectedFindingIds, setSelectedFindingIds] = useState<Set<string>>(new Set())
    const [selectedGlobalFacts, setSelectedGlobalFacts] = useState<Set<string>>(new Set())
    const [selectedSessionFacts, setSelectedSessionFacts] = useState<Set<string>>(new Set())

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
                            ) : sessions.map(s => (
                                <div key={s.id} className={`session-item ${s.id === currentSessionId ? 'active' : ''}`} onClick={() => onLoadSession(s.id)}>
                                    {editingSession === s.id ? (
                                        <input
                                            className="session-title-edit"
                                            value={editTitle}
                                            onChange={e => setEditTitle(e.target.value)}
                                            onBlur={commitRename}
                                            onKeyDown={e => {
                                                if (e.key === 'Enter' && !composingRef.current) commitRename()
                                                if (e.key === 'Escape') setEditingSession(null)
                                            }}
                                            onCompositionStart={() => { composingRef.current = true }}
                                            onCompositionEnd={() => { setTimeout(() => { composingRef.current = false }, 50) }}
                                            autoFocus
                                            onClick={e => e.stopPropagation()}
                                        />
                                    ) : (
                                        <div className="session-title" onDoubleClick={(e) => { e.stopPropagation(); startRename(s.id, s.title) }}>{s.title}</div>
                                    )}
                                    <div className="session-meta">
                                        <span className="session-date">{s.updated_at}</span>
                                        <div className="session-actions">
                                            <button onClick={(e) => { e.stopPropagation(); startRename(s.id, s.title) }} title="Rename">&#x270E;</button>
                                            <button onClick={(e) => { e.stopPropagation(); onDeleteSession(s.id) }} title="Delete">&#x2715;</button>
                                        </div>
                                    </div>
                                </div>
                            ))}
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
                            {findings.length > 0 && (
                                <div className={`status-section ${selectedFindingIds.size > 0 ? 'bulk-active' : ''}`}>
                                    <div className="bulk-section-header">
                                        <h3>Findings</h3>
                                        <BulkActions
                                            total={findings.length}
                                            selectedCount={selectedFindingIds.size}
                                            onSelectAll={() => setSelectedFindingIds(new Set(findings.map(f => f.id)))}
                                            onClear={() => setSelectedFindingIds(new Set())}
                                            onDelete={async () => {
                                                const ids = Array.from(selectedFindingIds)
                                                if (ids.length === 0) return
                                                await onFindingsDelete(ids)
                                                setSelectedFindingIds(new Set())
                                            }}
                                        />
                                    </div>
                                    {findings.map(f => (
                                        <div key={f.id} className={`finding-card ${selectedFindingIds.has(f.id) ? 'selected' : ''}`}>
                                            <input
                                                type="checkbox"
                                                className="bulk-check"
                                                checked={selectedFindingIds.has(f.id)}
                                                onChange={e => {
                                                    const next = new Set(selectedFindingIds)
                                                    if (e.target.checked) next.add(f.id); else next.delete(f.id)
                                                    setSelectedFindingIds(next)
                                                }}
                                            />
                                            <div className="finding-body">
                                                <div className="finding-content">{f.content}</div>
                                                <div className="finding-meta">
                                                    {(() => {
                                                        const t = findingTrust(f.source)
                                                        const tip = t.cls === 'trust-user'
                                                            ? 'user-stated: ユーザー操作で promote された finding。高信頼。'
                                                            : 'derived: LLM が promote-finding で登録した finding。内容は LLM を経由しており、攻撃者影響下のバイトを含みうる。'
                                                        return (
                                                            <span className={`trust-badge ${t.cls}`} data-tooltip={tip}>
                                                                {t.label}
                                                            </span>
                                                        )
                                                    })()}
                                                    <span className="finding-date">{f.created_label}</span>
                                                </div>
                                                {f.tags && f.tags.length > 0 && (
                                                    <div className="finding-tags">
                                                        {f.tags.map(tag => {
                                                            const sevClass = ['critical', 'high', 'medium', 'low', 'info'].includes(tag) ? ` severity-${tag}` : ''
                                                            return <span key={tag} className={`tag${sevClass}`}>{tag}</span>
                                                        })}
                                                    </div>
                                                )}
                                            </div>
                                        </div>
                                    ))}
                                </div>
                            )}
                            <div className={`status-section ${selectedGlobalFacts.size > 0 ? 'bulk-active' : ''}`}>
                                <div className="bulk-section-header">
                                    <h3>Global Memory</h3>
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
                                ) : globalMemories.map((p, i) => (
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
                                        <div className="pinned-content">
                                            <span className="pinned-fact">{p.native_fact || p.fact}</span>
                                            {p.native_fact && p.native_fact !== p.fact && (
                                                <span className="pinned-fact-en">{p.fact}</span>
                                            )}
                                            {p.created_at && (
                                                <span className="pinned-date">learned {p.created_at.slice(0, 10)}</span>
                                            )}
                                        </div>
                                        <button className="pinned-delete" onClick={() => onGlobalMemoryDeleteOne(p.fact)}>&#x2715;</button>
                                    </div>
                                ))}
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
                                ) : sessionMemories.map((p, i) => (
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
                                        <div className="pinned-content">
                                            <span className="pinned-fact">{p.native_fact || p.fact}</span>
                                            {p.native_fact && p.native_fact !== p.fact && (
                                                <span className="pinned-fact-en">{p.fact}</span>
                                            )}
                                            {p.created_at && (
                                                <span className="pinned-date">learned {p.created_at.slice(0, 10)}</span>
                                            )}
                                        </div>
                                        <button
                                            className="pinned-pin"
                                            title="Pin to Global Memory"
                                            onClick={() => {
                                                // Default to "decision" category — user can
                                                // re-categorise from the Global Memory list
                                                // afterwards. Phase 9 replaces this with a
                                                // category-picker dialog.
                                                onPinSessionMemory(p.fact, 'decision')
                                            }}
                                        >
                                            &#x2605;
                                        </button>
                                    </div>
                                ))}
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
