// SettingsDialog is the full-screen settings modal with four
// tabs (General / Tools / MCP / Sandbox). Each field calls
// onUpdate with a partial Settings patch; the parent (App) is
// responsible for merging, persisting, and triggering
// side-effects (theme apply, sandbox restart, LLM-backend
// restart). MCP guardian restart stays in App because it also
// touches the tool list.

import {useState, useEffect, useRef} from 'react'
import BackendBudgetEditor from '../components/BackendBudgetEditor'
import {showError} from '../notify'
import type {MCPProfile, MCPStatus, Settings, ToolInfo, SandboxImageStatus, SandboxImageInfo} from '../types'
import type {main} from '../../wailsjs/go/models'

export type SettingsTab = 'general' | 'profiles' | 'rules' | 'tools' | 'mcp' | 'sandbox'

// Local-only frontend approximation of memory.EstimateTokens.
// max(chars/4, words*1.3) — close enough for the advisory display
// without round-tripping every keystroke through Wails. See
// feedback_token_estimation_json for why word-count alone
// under-counts JSON/Markdown by 4-5×.
function estimateTokens(s: string): number {
    if (!s) return 0
    const chars = s.length / 4
    const words = s.trim().split(/\s+/).filter(Boolean).length * 1.3
    return Math.max(Math.round(chars), Math.round(words))
}

interface Props {
    settings: Settings;
    tools: ToolInfo[];
    mcpStatus: MCPStatus[];
    onUpdate: (patch: Partial<Settings>) => void;
    onClose: () => void;
    onRestartMCP: () => Promise<void>;
    initialTab?: SettingsTab;
}

// validTabs is the runtime allowlist for the `initialTab` prop.
// Defensive against callers that accidentally forward a React
// SyntheticEvent (e.g. `onClick={openSettings}` instead of
// `onClick={() => openSettings()}`) — without this, `tab` would
// end up as an event object and no tab button would match.
const validTabs: readonly SettingsTab[] = ['general', 'profiles', 'rules', 'tools', 'mcp', 'sandbox']

export default function SettingsDialog({settings, tools, mcpStatus, onUpdate, onClose, onRestartMCP, initialTab}: Props) {
    const [tab, setTab] = useState<SettingsTab>(
        typeof initialTab === 'string' && validTabs.includes(initialTab as SettingsTab)
            ? (initialTab as SettingsTab)
            : 'general',
    )

    // System Rules tab state (ADR-0012). The rules file lives at
    // <dataDir>/system_rules.md and is managed through dedicated
    // bindings (Get/SetSystemRules) — not via the Settings object.
    const [rules, setRules] = useState<string>('')
    const [rulesSaved, setRulesSaved] = useState<string>('')
    const [rulesStatus, setRulesStatus] = useState<string>('')
    // Advisory shown after a Save that transitions rules from
    // non-empty to empty. Existing chats keep mirroring earlier
    // patterns from history even though the system prompt no
    // longer carries the rule (in-context conditioning).
    const [rulesClearedAdvisory, setRulesClearedAdvisory] = useState(false)
    // MCP guardian restart feedback: the restart spawns processes and
    // blocks for a few seconds, so the button shows an in-progress
    // state and a brief confirmation rather than silently restarting.
    const [mcpRestarting, setMcpRestarting] = useState(false)
    const [mcpRestarted, setMcpRestarted] = useState(false)
    const reloadRules = () => {
        if (!window.go) return
        window.go.main.Bindings.GetSystemRules().then((s: string) => {
            setRules(s)
            setRulesSaved(s)
        }).catch(() => {})
    }
    useEffect(() => {
        if (tab === 'rules') {
            reloadRules()
            setRulesClearedAdvisory(false)
        }
    }, [tab])
    const rulesDirty = rules !== rulesSaved
    const rulesChars = rules.length
    const rulesTokens = estimateTokens(rules)
    const activeMaxTokens = settings.default_backend === 'vertex_ai'
        ? (settings.vertex_budget?.max_context_tokens || 0)
        : (settings.local_budget?.max_context_tokens || 0)
    const rulesPct = activeMaxTokens > 0 ? (rulesTokens / activeMaxTokens) * 100 : 0
    const rulesAdvisory: 'ok' | 'warn' | 'high' =
        rulesPct >= 20 ? 'high' : rulesPct >= 5 ? 'warn' : 'ok'
    const saveRules = async () => {
        if (!window.go) return
        try {
            const wasNonEmpty = rulesSaved.trim() !== ''
            await window.go.main.Bindings.SetSystemRules(rules)
            // Backend normalises trailing newlines etc. — re-fetch
            // so the editor reflects the canonical stored form.
            const next = await window.go.main.Bindings.GetSystemRules()
            setRules(next)
            setRulesSaved(next)
            setRulesStatus('Saved')
            setTimeout(() => setRulesStatus(''), 2000)
            // Advisory only when rules went from non-empty to empty;
            // re-saving an empty form (or changing to other content)
            // doesn't trip it.
            setRulesClearedAdvisory(wasNonEmpty && next.trim() === '')
        } catch (e: any) {
            setRulesStatus(`Save failed: ${String(e?.message || e)}`)
        }
    }

    // Sandbox image build state — local to the dialog so the
    // build log doesn't survive close/reopen, but the
    // imageStatus refresh after `sandbox:build:done` does
    // pick up the new tag.
    const [imageStatus, setImageStatus] = useState<SandboxImageStatus | null>(null)
    const [buildLog, setBuildLog] = useState<string[]>([])
    const [buildLogOpen, setBuildLogOpen] = useState(false)
    const buildLogEndRef = useRef<HTMLDivElement | null>(null)
    // Two-click confirm for image Delete: first click arms,
    // second confirms. Auto-disarms after 3s. Same pattern as
    // BulkActions / Findings delete. Wails webview's
    // window.confirm() silently returns false on macOS, so we
    // can't rely on it.
    const [confirmDeleteTag, setConfirmDeleteTag] = useState<string | null>(null)
    useEffect(() => {
        if (!confirmDeleteTag) return
        const t = setTimeout(() => setConfirmDeleteTag(null), 3000)
        return () => clearTimeout(t)
    }, [confirmDeleteTag])

    const refreshImageStatus = () => {
        if (!window.go) return
        window.go.main.Bindings.GetSandboxImageStatus().then(setImageStatus).catch(() => {})
    }

    useEffect(() => {
        refreshImageStatus()
        if (!window.runtime) return
        const cleanupLine = window.runtime.EventsOn('sandbox:build:line', (data: any) => {
            setBuildLog(prev => [...prev, String(data?.line ?? '')])
        })
        const cleanupDone = window.runtime.EventsOn('sandbox:build:done', (data: any) => {
            const errStr = String(data?.error ?? '')
            setBuildLog(prev => [...prev, errStr ? `\n=== build failed: ${errStr} ===` : '\n=== build complete ==='])
            refreshImageStatus()
        })
        return () => { cleanupLine(); cleanupDone() }
    }, [])

    useEffect(() => {
        buildLogEndRef.current?.scrollIntoView({behavior: 'smooth', block: 'end'})
    }, [buildLog])

    const startBuild = async () => {
        if (!window.go) return
        setBuildLog([])
        setBuildLogOpen(true)
        try {
            await window.go.main.Bindings.BuildSandboxImage()
            refreshImageStatus()
        } catch (e: any) {
            setBuildLog(prev => [...prev, `\n=== ${String(e?.message || e)} ===`])
        }
    }

    return (
        <div className="settings-overlay" onClick={onClose}>
            <div className="settings-modal" onClick={e => e.stopPropagation()}>
                <div className="settings-header">
                    <h2>Settings</h2>
                    <button className="settings-close" onClick={onClose}>&#x2715;</button>
                </div>
                <div className="settings-tabs">
                    <button className={tab === 'general' ? 'active' : ''} onClick={() => setTab('general')}>General</button>
                    <button className={tab === 'profiles' ? 'active' : ''} onClick={() => setTab('profiles')}>LLM Profiles</button>
                    <button className={tab === 'rules' ? 'active' : ''} onClick={() => setTab('rules')}>System Rules</button>
                    <button className={tab === 'tools' ? 'active' : ''} onClick={() => setTab('tools')}>Tools</button>
                    <button className={tab === 'mcp' ? 'active' : ''} onClick={() => setTab('mcp')}>MCP</button>
                    <button className={tab === 'sandbox' ? 'active' : ''} onClick={() => setTab('sandbox')}>Sandbox</button>
                </div>
                <div className="settings-body">
                    {tab === 'general' && (<>
                        <div className="settings-section">
                            <h3>General</h3>
                            <label>
                                <span>Theme</span>
                                <select value={settings.theme || 'dark'} onChange={e => {
                                    onUpdate({theme: e.target.value})
                                    document.documentElement.setAttribute('data-theme', e.target.value)
                                }}>
                                    <option value="dark">Dark</option>
                                    <option value="light">Light</option>
                                    <option value="warm">Warm</option>
                                    <option value="midnight">Midnight</option>
                                </select>
                            </label>
                            <label>
                                <span>Location</span>
                                <input value={settings.location || ''} placeholder="e.g. Tokyo, Japan" onChange={e => onUpdate({location: e.target.value})} />
                            </label>
                        </div>
                        <div className="settings-section">
                            <h3>Agent loop</h3>
                            <label>
                                <span>Max tool rounds per message</span>
                                <input type="number" min={1} value={settings.max_tool_rounds || 10} onChange={e => onUpdate({max_tool_rounds: parseInt(e.target.value, 10) || 10})} />
                            </label>
                            <p className="sidebar-hint">Maximum tool calls the agent makes before being forced to reply. Raise it for long analyses that legitimately need more steps.</p>
                        </div>
                        <div className="settings-section">
                            <h3>Data analysis</h3>
                            <label>
                                <span>Max rows per query result</span>
                                <input type="number" min={1} value={settings.max_query_rows || 10000} onChange={e => onUpdate({max_query_rows: parseInt(e.target.value, 10) || 10000})} />
                            </label>
                            <p className="sidebar-hint">Cap on rows returned into the chat by <code>query-sql</code> / <code>query-preview</code> / <code>quick-summary</code>. These rows enter the LLM's context, so raising it sends more data to the model and uses more of its context window.</p>
                            <label>
                                <span>Max rows for CSV export to sandbox</span>
                                <input type="number" min={1} value={settings.max_export_rows || 1000000} onChange={e => onUpdate({max_export_rows: parseInt(e.target.value, 10) || 1000000})} />
                            </label>
                            <p className="sidebar-hint">Cap on rows written to a file by <code>export-sql-to-csv</code>. These never enter the chat, so the ceiling is memory rather than context — the default is much higher. Raise it if data handoff to the sandbox is being truncated.</p>
                        </div>
                        <div className="settings-section">
                            <h3>Privacy</h3>
                            <label>
                                <span>Log verbosity</span>
                                <select value={settings.log_level || 'info'} onChange={e => onUpdate({log_level: e.target.value})}>
                                    <option value="debug">debug — everything (incl. messages, LLM responses, tool args)</option>
                                    <option value="info">info — events + lifecycle, no conversation content (default)</option>
                                    <option value="warn">warn — warnings and errors only</option>
                                    <option value="error">error — errors only</option>
                                </select>
                            </label>
                            <p className="sidebar-hint">What gets written to <code>app.log</code>. <strong>info</strong> keeps conversation content out of the log; <strong>debug</strong> captures everything (use when reporting an issue).</p>
                        </div>
                        {/* v0.12.0: Local LLM + Vertex AI sections moved to the
                            "LLM Profiles" tab (one editable form per profile).
                            See ADR-0016. The General tab keeps only Theme /
                            Location / Agent loop / Privacy / Logger. */}
                    </>)}
                    {tab === 'profiles' && <ProfilesTab />}
                    {tab === 'rules' && (<>
                        <div className="settings-section">
                            <h3>System Rules</h3>
                            <p className="sidebar-hint">Standing instructions the agent follows in every chat — e.g. &ldquo;always reply in Japanese&rdquo;, &ldquo;use a report for long answers&rdquo;.</p>
                            <p className="sidebar-hint">Also editable in any text editor at <code>~/Library/Application Support/shell-agent-v2/system_rules.md</code>; click <strong>Reload from disk</strong> to pick up external changes.</p>
                            <textarea
                                className="system-rules-editor"
                                style={{width: '100%', minHeight: '18em', fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: '0.85em'}}
                                placeholder="e.g. Always respond in Japanese. Prefer concise answers. When showing code, include a short rationale."
                                value={rules}
                                onChange={e => setRules(e.target.value)}
                            />
                            <div className="system-rules-footer" style={{display: 'flex', alignItems: 'center', gap: '1em', marginTop: '0.5em', fontSize: '0.85em'}}>
                                <span style={{
                                    color: rulesAdvisory === 'high' ? '#dc322f' : rulesAdvisory === 'warn' ? '#b58900' : '#859900',
                                }}>
                                    {rulesChars.toLocaleString()} chars · ~{rulesTokens.toLocaleString()} tokens
                                    {activeMaxTokens > 0 && ` (${rulesPct.toFixed(1)}% of ${activeMaxTokens.toLocaleString()})`}
                                </span>
                                <span style={{flex: 1}}>{rulesStatus}</span>
                                <button type="button" onClick={reloadRules}>Reload from disk</button>
                                <button type="button" disabled={!rulesDirty} onClick={saveRules}>{rulesDirty ? 'Save' : 'Saved'}</button>
                            </div>
                            {rulesAdvisory === 'warn' && (
                                <p className="sidebar-hint" style={{borderLeft: '3px solid #b58900', paddingLeft: '0.5em', marginTop: '0.5em'}}>
                                    ⚠ System Rules consume a noticeable share of the active backend's context budget. Consider trimming.
                                </p>
                            )}
                            {rulesAdvisory === 'high' && (
                                <p className="sidebar-hint" style={{borderLeft: '3px solid #dc322f', paddingLeft: '0.5em', marginTop: '0.5em'}}>
                                    ⚠ System Rules are using ≥ 20% of the active backend's context budget. This leaves little room for conversation history, tool results, and memory channels. Trim or split into shorter sections.
                                </p>
                            )}
                            {rulesClearedAdvisory && (
                                <p className="sidebar-hint" style={{borderLeft: '3px solid #b58900', paddingLeft: '0.5em', marginTop: '0.5em', display: 'flex', alignItems: 'flex-start', gap: '0.5em'}}>
                                    <span style={{flex: 1}}>
                                        ⚠ Rules cleared. Existing chats may continue mirroring earlier patterns because the LLM sees its previous responses in the conversation history (in-context conditioning). The system prompt no longer carries any rule. <strong>Start a new chat</strong> to verify the change.
                                    </span>
                                    <button
                                        onClick={() => setRulesClearedAdvisory(false)}
                                        style={{background: 'none', border: 'none', cursor: 'pointer', fontSize: '1em', color: 'inherit', padding: 0, lineHeight: 1}}
                                        title="Dismiss"
                                    >&#x2715;</button>
                                </p>
                            )}
                        </div>
                    </>)}
                    {tab === 'tools' && (<>
                        <div className="settings-section">
                            <h3>Registered Tools</h3>
                            {tools.length === 0 ? (
                                <p className="sidebar-hint">No tools available</p>
                            ) : tools.map(t => {
                                const disabled = (settings.disabled_tools || []).includes(t.name)
                                const mitlOverride = (settings.mitl_overrides || {})[t.name]
                                // Backend supplies the per-tool MITL default
                                // (analysisToolMITLDefault for analysis tools,
                                // category/source rules for shell/mcp/sandbox).
                                // Computing it here from category/source went
                                // out of sync after Phase B's analysis-tool
                                // MITL routing change.
                                const mitlDefault = !!t.mitl_default
                                const mitlActive = mitlOverride !== undefined ? mitlOverride : mitlDefault
                                return (
                                    <div key={t.name} className={`tool-item ${disabled ? 'tool-disabled' : ''}`}>
                                        <div className="tool-name">
                                            <label className="tool-enabled-toggle" title="Enable/Disable">
                                                <input type="checkbox" checked={!disabled} onChange={e => {
                                                    const list = (settings.disabled_tools || []).filter(n => n !== t.name)
                                                    if (!e.target.checked) list.push(t.name)
                                                    onUpdate({disabled_tools: list} as any)
                                                }} />
                                            </label>
                                            <code>{t.name}</code>
                                            <span className={`tool-category ${t.category}`}>{t.category}</span>
                                            <span className="tool-source">{t.source}</span>
                                            <label className="tool-mitl-toggle" title="MITL approval">
                                                <input type="checkbox" checked={mitlActive} onChange={e => {
                                                    const overrides = {...(settings.mitl_overrides || {})}
                                                    if (e.target.checked === mitlDefault) {
                                                        delete overrides[t.name]
                                                    } else {
                                                        overrides[t.name] = e.target.checked
                                                    }
                                                    onUpdate({mitl_overrides: overrides} as any)
                                                }} />
                                                <span className="mitl-label-text">MITL</span>
                                            </label>
                                        </div>
                                        <div className="tool-desc">{t.description}</div>
                                    </div>
                                )
                            })}
                        </div>
                    </>)}
                    {tab === 'mcp' && (<>
                        <div className="settings-section">
                            <h3>MCP Profiles</h3>
                            {(settings.mcp_profiles || []).length === 0 ? (
                                <p className="sidebar-hint">No MCP profiles configured</p>
                            ) : (settings.mcp_profiles || []).map((p, i) => {
                                const status = mcpStatus.find(s => s.name === p.name)
                                const updateField = (field: keyof MCPProfile, value: any) => {
                                    const updated = [...(settings.mcp_profiles || [])]
                                    updated[i] = {...p, [field]: value}
                                    onUpdate({mcp_profiles: updated} as any)
                                }
                                return (
                                    <div key={i} className="mcp-profile-item">
                                        <div className="mcp-profile-header">
                                            <input
                                                className="mcp-profile-name-input"
                                                value={p.name}
                                                onChange={e => updateField('name', e.target.value)}
                                            />
                                            {status && <span className={`mcp-status-badge ${status.status}`}>{status.status}{status.status === 'running' ? ` (${status.tool_count} tools)` : ''}</span>}
                                            <label className="mcp-toggle">
                                                <input type="checkbox" checked={p.enabled} onChange={e => updateField('enabled', e.target.checked)} />
                                                <span>{p.enabled ? 'ON' : 'OFF'}</span>
                                            </label>
                                            <button className="mcp-delete" onClick={() => {
                                                const updated = (settings.mcp_profiles || []).filter((_, j) => j !== i)
                                                onUpdate({mcp_profiles: updated} as any)
                                            }}>&#x2715;</button>
                                        </div>
                                        <label className="mcp-profile-edit">
                                            <span className="mcp-label">Binary</span>
                                            <input value={p.binary} onChange={e => updateField('binary', e.target.value)} />
                                        </label>
                                        <label className="mcp-profile-edit">
                                            <span className="mcp-label">Profile</span>
                                            <input value={p.profile_path} onChange={e => updateField('profile_path', e.target.value)} />
                                        </label>
                                        {status?.error && <div className="mcp-status-error">{status.error}</div>}
                                    </div>
                                )
                            })}
                            <div className="mcp-add-form">
                                <h4>Add Profile</h4>
                                <label><span>Name</span><input id="mcp-name" placeholder="e.g. github" /></label>
                                <label><span>Binary</span><input id="mcp-binary" defaultValue="/usr/local/bin/mcp-guardian" /></label>
                                <label><span>Profile</span><input id="mcp-profile" placeholder="~/.config/mcp-guardian/profiles/xxx.json" /></label>
                                <button className="mcp-add-btn" onClick={() => {
                                    const name = (document.getElementById('mcp-name') as HTMLInputElement).value.trim()
                                    const binary = (document.getElementById('mcp-binary') as HTMLInputElement).value.trim()
                                    const profile = (document.getElementById('mcp-profile') as HTMLInputElement).value.trim()
                                    if (!name || !binary || !profile) return
                                    const updated = [...(settings.mcp_profiles || []), {name, binary, profile_path: profile, enabled: true}]
                                    onUpdate({mcp_profiles: updated} as any);
                                    (document.getElementById('mcp-name') as HTMLInputElement).value = '';
                                    (document.getElementById('mcp-profile') as HTMLInputElement).value = ''
                                }}>Add</button>
                            </div>
                            <div style={{marginTop: 16, display: 'flex', alignItems: 'center', gap: 8}}>
                                <button
                                    className="mcp-restart-btn"
                                    disabled={mcpRestarting}
                                    onClick={async () => {
                                        setMcpRestarted(false)
                                        setMcpRestarting(true)
                                        try {
                                            await onRestartMCP()
                                            setMcpRestarted(true)
                                            setTimeout(() => setMcpRestarted(false), 3000)
                                        } finally {
                                            setMcpRestarting(false)
                                        }
                                    }}
                                >{mcpRestarting ? 'Restarting…' : 'Restart MCP Guardians'}</button>
                                {mcpRestarted && <span className="mcp-restart-done">✓ Restarted</span>}
                            </div>
                        </div>
                    </>)}
                    {tab === 'sandbox' && (<>
                        <div className="settings-section">
                            <h3>Sandbox</h3>
                            <p className="sidebar-hint">Runs shell scripts and Python code inside an isolated per-session container. Useful for one-off code execution and data wrangling that you don't want touching your real filesystem — only the session's <code>/work</code> directory is mounted, network is off by default.</p>
                            {imageStatus?.active_tag && !imageStatus.active_pinned_by_digest && (
                                <p className="sidebar-hint" style={{borderLeft: '3px solid #b58900', paddingLeft: '0.5em', marginTop: '0.5em'}}>
                                    ⚠ Active image <code>{imageStatus.active_tag}</code> uses a mutable tag. A registry or network compromise could swap the image bytes the next time the engine pulls. Build locally (the Dockerfile flow below produces a content-addressed tag) or pin upstream by <code>@sha256:&lt;digest&gt;</code>.
                                </p>
                            )}
                        </div>
                        <div className="settings-section">
                            <h3>Built images</h3>
                            <p className="sidebar-hint">Sandbox container images you've built locally. The Active one is what tool calls actually run in.</p>
                            {(!imageStatus?.images || imageStatus.images.length === 0) ? (
                                <p className="sidebar-hint">(none yet — edit the Dockerfile below and click Build)</p>
                            ) : (
                                <ul className="sandbox-image-list">
                                    {imageStatus.images.map((img: SandboxImageInfo) => (
                                        <li key={img.tag} className={img.active ? 'active' : ''}>
                                            <span className="img-tag">{img.tag}</span>
                                            <span className="img-meta">
                                                {img.size_bytes > 0 ? `${(img.size_bytes / 1024 / 1024).toFixed(0)} MB` : ''}
                                                {img.created ? ` • ${img.created.replace('T', ' ').replace('Z', '')}` : ''}
                                            </span>
                                            {img.active ? (
                                                <span className="status-badge status-ready">Active</span>
                                            ) : (
                                                <button onClick={async () => {
                                                    if (!window.go) return
                                                    try { await window.go.main.Bindings.SetActiveSandboxImage(img.tag) } catch (e: any) { showError('Error', String(e?.message || e)) }
                                                    refreshImageStatus()
                                                }}>Use</button>
                                            )}
                                            <button
                                                className={`img-delete ${confirmDeleteTag === img.tag ? 'confirming' : ''}`}
                                                title={confirmDeleteTag === img.tag ? 'Click again to confirm' : `Delete ${img.tag}`}
                                                onClick={async () => {
                                                    if (!window.go) return
                                                    if (confirmDeleteTag !== img.tag) {
                                                        setConfirmDeleteTag(img.tag)
                                                        return
                                                    }
                                                    setConfirmDeleteTag(null)
                                                    try {
                                                        await window.go.main.Bindings.RemoveSandboxImage(img.tag)
                                                    } catch (e: any) {
                                                        showError('Error', String(e?.message || e))
                                                    }
                                                    refreshImageStatus()
                                                }}
                                            >
                                                {confirmDeleteTag === img.tag ? 'Click to confirm' : 'Delete'}
                                            </button>
                                        </li>
                                    ))}
                                </ul>
                            )}
                            <label className={`checkbox-row ${!imageStatus?.active_ready ? 'disabled-row' : ''}`} title={!imageStatus?.active_ready ? 'Pick an Active image first' : ''}>
                                <input type="checkbox" disabled={!imageStatus?.active_ready} checked={!!settings.sandbox?.enabled} onChange={e => onUpdate({sandbox: {...settings.sandbox, enabled: e.target.checked}})} />
                                <span>Enable container sandbox</span>
                            </label>
                        </div>
                        <div className="settings-section">
                            <h3>Dockerfile</h3>
                            <p className="sidebar-hint">The Dockerfile used to build a sandbox image. Edit to add packages (e.g. Python libs you want available inside the container) and click <strong>Build</strong>.</p>
                            <textarea
                                className="dockerfile-editor"
                                rows={16}
                                value={settings.sandbox?.dockerfile ?? imageStatus?.current_dockerfile ?? ''}
                                onChange={e => onUpdate({sandbox: {...settings.sandbox, dockerfile: e.target.value}})}
                            />
                            <div className="sandbox-image-status">
                                <button type="button" disabled={!imageStatus?.recommended_dockerfile} onClick={() => onUpdate({sandbox: {...settings.sandbox, dockerfile: imageStatus?.recommended_dockerfile || ''}})}>
                                    Reset to recommended
                                </button>
                                <button type="button" disabled={!!imageStatus?.building} onClick={startBuild}>
                                    {imageStatus?.building ? 'Building…' : 'Build'}
                                </button>
                                {imageStatus?.building && <span className="status-badge status-building">⏳ Building…</span>}
                                {buildLog.length > 0 && !buildLogOpen && (
                                    <button type="button" onClick={() => setBuildLogOpen(true)}>View build log</button>
                                )}
                            </div>
                        </div>
                        <div className="settings-section">
                            <h3>Container limits</h3>
                            <label>
                                <span>Engine</span>
                                <select value={settings.sandbox?.engine || 'auto'} onChange={e => onUpdate({sandbox: {...settings.sandbox, engine: e.target.value}})}>
                                    <option value="auto">auto (podman → docker)</option>
                                    <option value="podman">podman</option>
                                    <option value="docker">docker</option>
                                </select>
                            </label>
                            <label className="checkbox-row">
                                <input type="checkbox" checked={!!settings.sandbox?.network} onChange={e => onUpdate({sandbox: {...settings.sandbox, network: e.target.checked}})} />
                                <span>Allow network egress (default off)</span>
                            </label>
                            <label>
                                <span>CPU limit</span>
                                <input value={settings.sandbox?.cpu_limit || ''} placeholder="2" onChange={e => onUpdate({sandbox: {...settings.sandbox, cpu_limit: e.target.value}})} />
                            </label>
                            <label>
                                <span>Memory limit</span>
                                <input value={settings.sandbox?.memory_limit || ''} placeholder="1g" onChange={e => onUpdate({sandbox: {...settings.sandbox, memory_limit: e.target.value}})} />
                            </label>
                            <label>
                                <span>Per-call timeout (seconds)</span>
                                <input type="number" min={5} value={settings.sandbox?.timeout_seconds || 60} onChange={e => onUpdate({sandbox: {...settings.sandbox, timeout_seconds: parseInt(e.target.value, 10) || 60}})} />
                            </label>
                        </div>
                        {buildLogOpen && (
                            <div className="build-log-overlay" onClick={() => setBuildLogOpen(false)}>
                                <div className="build-log-panel" onClick={e => e.stopPropagation()}>
                                    <div className="build-log-header">
                                        <span>Sandbox image build log</span>
                                        <button onClick={() => setBuildLogOpen(false)}>&#x2715;</button>
                                    </div>
                                    <pre className="build-log-body">
                                        {buildLog.join('\n')}
                                        <div ref={buildLogEndRef} />
                                    </pre>
                                </div>
                            </div>
                        )}
                    </>)}
                </div>
                <div className="settings-footer">
                    <button className="settings-close-btn" onClick={onClose}>Close</button>
                </div>
            </div>
        </div>
    )
}


// ProfilesTab — ADR-0016 §3.5 LLM Profiles UI.
//
// Live-apply per macOS convention: text fields commit on blur,
// dropdowns commit on change, deletes / set-default commit on
// click. No explicit Save button — the rest of the app's Settings
// dialog also auto-saves on each change.
//
// A draftRef tracks the latest local state synchronously so blur /
// change handlers commit the right values even when React's state
// update is still in flight.
function ProfilesTab() {
    const [profiles, setProfiles] = useState<main.ProfileSummary[]>([])
    const [selectedID, setSelectedID] = useState<string>('')
    const [draft, setDraft] = useState<main.ProfileDetail | null>(null)
    const draftRef = useRef<main.ProfileDetail | null>(null)
    const [status, setStatus] = useState<string>('')
    const [confirmDeleteID, setConfirmDeleteID] = useState<string>('')

    useEffect(() => {
        draftRef.current = draft
    }, [draft])

    const refreshList = async () => {
        if (!window.go) return
        try {
            const ps = await window.go.main.Bindings.ListProfiles()
            setProfiles(ps)
            if (!selectedID && ps.length > 0) {
                setSelectedID(ps[0].id)
            }
        } catch (e) {
            console.error('ListProfiles:', e)
        }
    }

    useEffect(() => {
        refreshList()
    }, []) // eslint-disable-line react-hooks/exhaustive-deps

    useEffect(() => {
        if (!selectedID || !window.go) {
            setDraft(null)
            return
        }
        window.go.main.Bindings.GetProfile(selectedID).then(d => {
            setDraft(d)
        }).catch(e => console.error('GetProfile:', e))
    }, [selectedID])

    useEffect(() => {
        if (!confirmDeleteID) return
        const t = setTimeout(() => setConfirmDeleteID(''), 3000)
        return () => clearTimeout(t)
    }, [confirmDeleteID])

    // flashStatus shows a "Saved" / rename note briefly. Errors stay
    // longer (5s) so the user has time to read them.
    const flashStatus = (msg: string, ms = 1800) => {
        setStatus(msg)
        setTimeout(() => setStatus(s => (s === msg ? '' : s)), ms)
    }

    // commit reads the live draftRef (NOT the React state — which
    // may be stale when blur fires right after a setDraft) and
    // forwards it to UpdateProfile. After a successful commit the
    // backend may have auto-renamed the profile (duplicate Name
    // disambiguation); refetch to surface that change.
    const commit = async () => {
        const d = draftRef.current
        if (!window.go || !d) return
        try {
            const res = await window.go.main.Bindings.UpdateProfile(d.id, {
                name: d.name,
                default_backend: d.default_backend,
                local: d.local,
                vertex: d.vertex,
            } as main.UpdateProfileRequest)
            await refreshList()
            const fresh = await window.go.main.Bindings.GetProfile(d.id)
            setDraft(fresh)
            if (res.name_adjusted) {
                flashStatus(`Renamed to "${res.profile.name}" (the name was taken)`, 4500)
            } else {
                flashStatus('Saved ✓')
            }
        } catch (e: any) {
            flashStatus(`Save failed: ${String(e?.message || e)}`, 5000)
        }
    }

    // commitWith is the immediate-commit path for selects / radios.
    // It updates the draft and immediately persists, without
    // waiting for React state to settle.
    const commitWith = async (override: Partial<main.ProfileDetail>) => {
        const current = draftRef.current
        if (!current) return
        const next = {...current, ...override} as main.ProfileDetail
        setDraft(next)
        draftRef.current = next
        await commit()
    }

    // setLocal / setVertex / setName patch the draft only (no
    // persistence). Blur handlers call commit() afterwards.
    const patch = (p: Partial<main.ProfileDetail>) => {
        setDraft(prev => {
            if (!prev) return prev
            const next = {...prev, ...p} as main.ProfileDetail
            draftRef.current = next
            return next
        })
    }
    const patchLocal = (p: Partial<main.LocalProfileFields>) => {
        setDraft(prev => {
            if (!prev) return prev
            const next = {...prev, local: {...prev.local, ...p}} as main.ProfileDetail
            draftRef.current = next
            return next
        })
    }
    const patchVertex = (p: Partial<main.VertexProfileFields>) => {
        setDraft(prev => {
            if (!prev) return prev
            const next = {...prev, vertex: {...prev.vertex, ...p}} as main.ProfileDetail
            draftRef.current = next
            return next
        })
    }

    const createNew = async (cloneFromID: string) => {
        if (!window.go) return
        const name = cloneFromID ? 'Cloned profile' : 'New profile'
        try {
            const res = await window.go.main.Bindings.CreateProfile({
                name,
                clone_from_id: cloneFromID,
                default_side: 'local',
            } as main.CreateProfileRequest)
            await refreshList()
            setSelectedID(res.profile.id)
            if (res.name_adjusted) {
                flashStatus(`Created "${res.profile.name}" (the name was taken)`, 4500)
            } else {
                flashStatus('Profile created')
            }
        } catch (e: any) {
            flashStatus(`Create failed: ${String(e?.message || e)}`, 5000)
        }
    }

    const removeProfile = async (id: string) => {
        if (!window.go) return
        try {
            await window.go.main.Bindings.DeleteProfile(id)
            await refreshList()
            if (selectedID === id) {
                const remaining = profiles.filter(p => p.id !== id)
                setSelectedID(remaining.length > 0 ? remaining[0].id : '')
            }
            flashStatus('Profile deleted')
        } catch (e: any) {
            flashStatus(`Delete failed: ${String(e?.message || e)}`, 5000)
        }
    }

    const setAsDefault = async (id: string) => {
        if (!window.go) return
        try {
            await window.go.main.Bindings.SetDefaultProfile(id)
            await refreshList()
            flashStatus('Default profile updated')
        } catch (e: any) {
            flashStatus(`SetDefault failed: ${String(e?.message || e)}`, 5000)
        }
    }

    return (
        <div className="settings-section">
            <div className="profile-tab-header">
                <h3>LLM Profiles</h3>
                {status && <span className="profile-save-status">{status}</span>}
            </div>
            <p className="sidebar-hint">
                A profile bundles one Local LLM endpoint and one Vertex AI project. Each chat session
                runs against one profile — use separate profiles to keep work / personal LLM accounts
                and endpoints apart.
            </p>
            <div className="profiles-layout">
                <div className="profiles-list">
                    {profiles.map(p => (
                        <div
                            key={p.id}
                            className={`profile-list-row ${p.id === selectedID ? 'selected' : ''}`}
                            onClick={() => setSelectedID(p.id)}
                        >
                            <span className="profile-name">{p.name}</span>
                            {p.is_default && <span className="profile-default-tag">default</span>}
                        </div>
                    ))}
                    <div className="profiles-list-actions">
                        <button onClick={() => createNew('')}>+ New empty</button>
                        {selectedID && (
                            <button onClick={() => createNew(selectedID)}>+ Clone selected</button>
                        )}
                    </div>
                </div>
                <div className="profile-detail">
                    {draft ? (
                        <>
                            <label>
                                <span>Name</span>
                                <input
                                    value={draft.name}
                                    onChange={e => patch({name: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>Default side (which backend /model lands on first)</span>
                                <select
                                    value={draft.default_backend}
                                    onChange={e => commitWith({default_backend: e.target.value})}
                                >
                                    <option value="local">Local LLM</option>
                                    <option value="vertex_ai">Vertex AI</option>
                                </select>
                            </label>
                            <div className="profile-action-row">
                                {!draft.is_default && (
                                    <button onClick={() => setAsDefault(draft.id)}>Set as default</button>
                                )}
                                {!draft.is_default && (
                                    confirmDeleteID === draft.id
                                        ? <button className="danger-confirm" onClick={() => removeProfile(draft.id)}>Confirm delete</button>
                                        : <button className="danger" onClick={() => setConfirmDeleteID(draft.id)}>Delete profile</button>
                                )}
                                {draft.is_default && (
                                    <span className="sidebar-hint">Default profiles cannot be deleted. Set a different default first.</span>
                                )}
                            </div>
                            <h4>Local</h4>
                            <label>
                                <span>Endpoint</span>
                                <input
                                    value={draft.local.endpoint}
                                    onChange={e => patchLocal({endpoint: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>Model</span>
                                <input
                                    value={draft.local.model}
                                    onChange={e => patchLocal({model: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>API key env var</span>
                                <input
                                    value={draft.local.api_key_env}
                                    onChange={e => patchLocal({api_key_env: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            {/* BackendBudgetEditor's inputs don't ship their own
                                onBlur. Wrap so blur events from the embedded
                                <input>s bubble here and trigger commit. */}
                            <div onBlur={commit}>
                                <BackendBudgetEditor
                                    budget={draft.local.context_budget}
                                    onChange={b => patchLocal({context_budget: b as any})}
                                />
                            </div>
                            <label>
                                <span>Per-request timeout (seconds)</span>
                                <input
                                    type="number" min={5}
                                    value={draft.local.request_timeout_seconds || 300}
                                    onChange={e => patchLocal({request_timeout_seconds: parseInt(e.target.value, 10) || 300})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>Retry max attempts</span>
                                <input
                                    type="number" min={1} max={10}
                                    value={draft.local.retry_max_attempts || 3}
                                    onChange={e => patchLocal({retry_max_attempts: parseInt(e.target.value, 10) || 3})}
                                    onBlur={commit}
                                />
                            </label>
                            <label className="checkbox-row">
                                <input
                                    type="checkbox"
                                    checked={draft.local.auto_extract_enabled}
                                    onChange={e => { patchLocal({auto_extract_enabled: e.target.checked}); commit(); }}
                                />
                                <span>
                                    Auto-extract memories after each turn
                                    <small>Local default: off. When on, the <code>remember_fact</code> tool is hidden.</small>
                                </span>
                            </label>
                            <label className="checkbox-row">
                                <input
                                    type="checkbox"
                                    checked={draft.local.auto_title_enabled}
                                    onChange={e => { patchLocal({auto_title_enabled: e.target.checked}); commit(); }}
                                />
                                <span>
                                    Auto-generate session titles
                                    <small>Local default: off. When off, sessions stay untitled until renamed (skips one LLM call per session).</small>
                                </span>
                            </label>
                            <h4>Vertex AI</h4>
                            <label>
                                <span>Project ID</span>
                                <input
                                    value={draft.vertex.project_id}
                                    onChange={e => patchVertex({project_id: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>Region</span>
                                <input
                                    value={draft.vertex.region}
                                    onChange={e => patchVertex({region: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>Model</span>
                                <input
                                    value={draft.vertex.model}
                                    onChange={e => patchVertex({model: e.target.value})}
                                    onBlur={commit}
                                />
                            </label>
                            <div onBlur={commit}>
                                <BackendBudgetEditor
                                    budget={draft.vertex.context_budget}
                                    onChange={b => patchVertex({context_budget: b as any})}
                                />
                            </div>
                            <label>
                                <span>Per-request timeout (seconds)</span>
                                <input
                                    type="number" min={5}
                                    value={draft.vertex.request_timeout_seconds || 180}
                                    onChange={e => patchVertex({request_timeout_seconds: parseInt(e.target.value, 10) || 180})}
                                    onBlur={commit}
                                />
                            </label>
                            <label>
                                <span>Retry max attempts</span>
                                <input
                                    type="number" min={1} max={10}
                                    value={draft.vertex.retry_max_attempts || 3}
                                    onChange={e => patchVertex({retry_max_attempts: parseInt(e.target.value, 10) || 3})}
                                    onBlur={commit}
                                />
                            </label>
                            <label className="checkbox-row">
                                <input
                                    type="checkbox"
                                    checked={draft.vertex.auto_extract_enabled}
                                    onChange={e => { patchVertex({auto_extract_enabled: e.target.checked}); commit(); }}
                                />
                                <span>
                                    Auto-extract memories after each turn
                                    <small>Vertex default: on. When on, the <code>remember_fact</code> tool is hidden.</small>
                                </span>
                            </label>
                            <label className="checkbox-row">
                                <input
                                    type="checkbox"
                                    checked={draft.vertex.auto_title_enabled}
                                    onChange={e => { patchVertex({auto_title_enabled: e.target.checked}); commit(); }}
                                />
                                <span>
                                    Auto-generate session titles
                                    <small>Vertex default: on. When off, sessions stay untitled until renamed.</small>
                                </span>
                            </label>
                        </>
                    ) : (
                        <p className="sidebar-hint">Select a profile to edit.</p>
                    )}
                </div>
            </div>
        </div>
    )
}
