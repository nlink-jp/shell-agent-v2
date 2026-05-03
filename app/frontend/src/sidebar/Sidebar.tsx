// Sidebar — two-section accordion (Sessions / Memory) plus a
// bottom-nav (New Chat / Settings / Collapse). Same DOM is used
// for both expanded and collapsed modes; the `is-collapsed`
// class on the wrapper hides labels and content panels via CSS,
// keeping icon Y-positions and section dividers identical
// between modes.
//
// State that's only relevant to the sidebar (rename in-progress,
// per-list bulk-select sets, IME composition guard) lives here.
// The "big" state — the sessions list, findings, pinned memory —
// is owned by App and passed in as props; Sidebar forwards
// changes back via setter / handler props.

import {useRef, useState} from 'react'
import BulkActions from '../components/BulkActions'
import type {Finding, PinnedMemory, SessionInfo, SidebarPanel} from '../types'

// Trust badge surfaces the v0.1.26 memory-injection provenance fields.
// "user-stated" facts came from a user turn or a manual pin and are
// the trustable category. "derived" covers assistant-turn extraction
// and legacy entries (no Source set), both of which may carry
// attacker-influenced content from tool output. See
// docs/en/memory-injection-hardening.md §5 Phase A / D.
function pinnedTrust(source?: string): {label: string; cls: string} {
    if (source === 'user_turn' || source === 'manual') {
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
    pinnedMemories: PinnedMemory[];
    onPinnedDelete: (keys: string[]) => Promise<void>;
    onPinnedDeleteOne: (key: string) => Promise<void>;

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
    pinnedMemories, onPinnedDelete, onPinnedDeleteOne,
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
    const [selectedPinnedKeys, setSelectedPinnedKeys] = useState<Set<string>>(new Set())

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
                                                    {f.session_title && (
                                                        <span className="finding-origin" title={`Session: ${f.session_id}`}>{f.session_title}</span>
                                                    )}
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
                            <div className={`status-section ${selectedPinnedKeys.size > 0 ? 'bulk-active' : ''}`}>
                                <div className="bulk-section-header">
                                    <h3>Pinned Memory</h3>
                                    {pinnedMemories.length > 0 && (
                                        <BulkActions
                                            total={pinnedMemories.length}
                                            selectedCount={selectedPinnedKeys.size}
                                            onSelectAll={() => setSelectedPinnedKeys(new Set(pinnedMemories.map(p => p.fact)))}
                                            onClear={() => setSelectedPinnedKeys(new Set())}
                                            onDelete={async () => {
                                                const keys = Array.from(selectedPinnedKeys)
                                                if (keys.length === 0) return
                                                await onPinnedDelete(keys)
                                                setSelectedPinnedKeys(new Set())
                                            }}
                                        />
                                    )}
                                </div>
                                {pinnedMemories.length === 0 ? (
                                    <p className="sidebar-hint">No pinned facts</p>
                                ) : pinnedMemories.map((p, i) => (
                                    <div key={i} className={`pinned-item ${selectedPinnedKeys.has(p.fact) ? 'selected' : ''}`}>
                                        <input
                                            type="checkbox"
                                            className="bulk-check"
                                            checked={selectedPinnedKeys.has(p.fact)}
                                            onChange={e => {
                                                const next = new Set(selectedPinnedKeys)
                                                if (e.target.checked) next.add(p.fact); else next.delete(p.fact)
                                                setSelectedPinnedKeys(next)
                                            }}
                                        />
                                        <span className={`pinned-category ${p.category}`}>{p.category}</span>
                                        {(() => {
                                            const t = pinnedTrust(p.source)
                                            const tip = t.cls === 'trust-user'
                                                ? 'user-stated: ユーザー発話または手動 pin 由来の fact。高信頼。'
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
                                        <button className="pinned-delete" onClick={() => onPinnedDeleteOne(p.fact)}>&#x2715;</button>
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
                        <span className="sidebar-nav-ic">{sidebarCollapsed ? '\u25B6' : '\u25C0'}</span>
                    </button>
                </div>
            </div>
            {!sidebarCollapsed && <div className="sidebar-resize" onMouseDown={onStartResize} />}
        </>
    )
}
