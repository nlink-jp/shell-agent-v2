// SettingsDialog is the full-screen settings modal with four
// tabs (General / Tools / MCP / Sandbox). Each field calls
// onUpdate with a partial Settings patch; the parent (App) is
// responsible for merging, persisting, and triggering
// side-effects (theme apply, sandbox restart, LLM-backend
// restart). MCP guardian restart stays in App because it also
// touches the tool list.

import {useState, useEffect, useRef} from 'react'
import BackendBudgetEditor from '../components/BackendBudgetEditor'
import type {MCPProfile, MCPStatus, Settings, ToolInfo, SandboxImageStatus, SandboxImageInfo} from '../types'
import type {main} from '../../wailsjs/go/models'

type SettingsTab = 'general' | 'profiles' | 'rules' | 'tools' | 'mcp' | 'sandbox'

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
}

export default function SettingsDialog({settings, tools, mcpStatus, onUpdate, onClose, onRestartMCP}: Props) {
    const [tab, setTab] = useState<SettingsTab>('general')

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
                                <span>Default Backend</span>
                                <select value={settings.default_backend} onChange={e => onUpdate({default_backend: e.target.value})}>
                                    <option value="local">Local LLM</option>
                                    <option value="vertex_ai">Vertex AI</option>
                                </select>
                            </label>
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
                            <p className="sidebar-hint">Hard cap on tool-call rounds for one user message. Default 10. Loop detection (v0.1.16) catches stuck same-error stretches early; raise this only when a long, legitimate analysis legitimately needs more rounds.</p>
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
                            <p className="sidebar-hint">Controls what reaches <code>app.log</code>. Default <strong>info</strong> keeps user messages, LLM responses, and tool arguments out of the log file. Switch to <strong>debug</strong> only when reproducing an issue, then switch back. See <code>docs/en/privacy-controls.md</code> §3.</p>
                        </div>
                        {/* Sandbox section moved to dedicated tab in r3. */}
                        <div className="settings-section">
                            <h3>Local LLM</h3>
                            <label>
                                <span>Endpoint</span>
                                <input value={settings.local_endpoint} onChange={e => onUpdate({local_endpoint: e.target.value})} />
                            </label>
                            <label>
                                <span>Model</span>
                                <input value={settings.local_model} onChange={e => onUpdate({local_model: e.target.value})} />
                            </label>
                            <BackendBudgetEditor
                                budget={settings.local_budget}
                                onChange={b => onUpdate({local_budget: b})}
                            />
                            <label>
                                <span>Per-request timeout (seconds)</span>
                                <input type="number" min={5} value={settings.local_timeout_seconds || 300} onChange={e => onUpdate({local_timeout_seconds: parseInt(e.target.value, 10) || 300})} />
                            </label>
                            <label>
                                <span>Retry max attempts</span>
                                <input type="number" min={1} max={10} value={settings.local_retry_max_attempts || 3} onChange={e => onUpdate({local_retry_max_attempts: parseInt(e.target.value, 10) || 3})} />
                            </label>
                            <p className="sidebar-hint">Total LLM call attempts including the first (1 = no retries). Defaults to 3. Backoff timing knobs (base / max / jitter) are config-only — see README.</p>
                        </div>
                        <div className="settings-section">
                            <h3>Vertex AI</h3>
                            <label>
                                <span>Project ID</span>
                                <input value={settings.vertex_project} onChange={e => onUpdate({vertex_project: e.target.value})} />
                            </label>
                            <label>
                                <span>Region</span>
                                <input value={settings.vertex_region} onChange={e => onUpdate({vertex_region: e.target.value})} />
                            </label>
                            <label>
                                <span>Model</span>
                                <input value={settings.vertex_model} onChange={e => onUpdate({vertex_model: e.target.value})} />
                            </label>
                            <BackendBudgetEditor
                                budget={settings.vertex_budget}
                                onChange={b => onUpdate({vertex_budget: b})}
                            />
                            <label>
                                <span>Per-request timeout (seconds)</span>
                                <input type="number" min={5} value={settings.vertex_timeout_seconds || 180} onChange={e => onUpdate({vertex_timeout_seconds: parseInt(e.target.value, 10) || 180})} />
                            </label>
                            <label>
                                <span>Retry max attempts</span>
                                <input type="number" min={1} max={10} value={settings.vertex_retry_max_attempts || 3} onChange={e => onUpdate({vertex_retry_max_attempts: parseInt(e.target.value, 10) || 3})} />
                            </label>
                            <p className="sidebar-hint">Total LLM call attempts including the first (1 = no retries). Defaults to 3. Backoff timing knobs (base / max / jitter) are config-only — see README.</p>
                        </div>
                    </>)}
                    {tab === 'profiles' && <ProfilesTab />}
                    {tab === 'rules' && (<>
                        <div className="settings-section">
                            <h3>System Rules</h3>
                            <p className="sidebar-hint">Standing instructions the agent follows in every session. Plain Markdown, injected near the top of the system prompt. See <code>docs/en/adr/0012-system-rules.md</code>.</p>
                            <p className="sidebar-hint">Stored at <code>~/Library/Application Support/shell-agent-v2/system_rules.md</code>. Edits via external editor are picked up by <strong>Reload from disk</strong>.</p>
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
                            <button className="mcp-restart-btn" style={{marginTop: 16}} onClick={() => onRestartMCP()}>Restart MCP Guardians</button>
                        </div>
                    </>)}
                    {tab === 'sandbox' && (<>
                        <div className="settings-section">
                            <h3>Sandbox (experimental)</h3>
                            <p className="sidebar-hint">Exposes the eight sandbox-* tools that run shell/Python inside a per-session container. Tools register only when an Active image exists, the engine has it locally, AND the checkbox below is on. See docs/en/sandbox-image-build.md.</p>
                            {imageStatus?.active_tag && !imageStatus.active_pinned_by_digest && (
                                <p className="sidebar-hint" style={{borderLeft: '3px solid #b58900', paddingLeft: '0.5em', marginTop: '0.5em'}}>
                                    ⚠ Active image <code>{imageStatus.active_tag}</code> uses a mutable tag. A registry or network compromise could swap the image bytes the next time the engine pulls. Build locally (the Dockerfile flow below produces a content-addressed tag) or pin upstream by <code>@sha256:&lt;digest&gt;</code>.
                                </p>
                            )}
                        </div>
                        <div className="settings-section">
                            <h3>Built images</h3>
                            <p className="sidebar-hint">Locally-built sandbox images. Use to switch the Active one (no rebuild needed); Delete to free disk.</p>
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
                                                    try { await window.go.main.Bindings.SetActiveSandboxImage(img.tag) } catch (e: any) { alert(String(e?.message || e)) }
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
                                                        alert(String(e?.message || e))
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
                            <label className={!imageStatus?.active_ready ? 'disabled-row' : ''} title={!imageStatus?.active_ready ? 'Pick an Active image first' : ''}>
                                <input type="checkbox" disabled={!imageStatus?.active_ready} checked={!!settings.sandbox?.enabled} onChange={e => onUpdate({sandbox: {...settings.sandbox, enabled: e.target.checked}})} />
                                <span>Enable container sandbox</span>
                            </label>
                        </div>
                        <div className="settings-section">
                            <h3>Dockerfile</h3>
                            <p className="sidebar-hint">Edit and click Build to produce a new image. The tag is derived from the Dockerfile content (sha256), so identical edits don't rebuild.</p>
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
                            <p className="sidebar-hint">Build runs locally on your podman/docker. Takes a few minutes (apt-get + pip install).</p>
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
                            <label>
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
// Self-contained component that lists profiles and lets the user
// create / clone / rename / edit / delete / set-default. The full
// per-profile detail form (Local + Vertex sections) mirrors the
// legacy single-profile Settings layout, so a v0.11.x user reads
// the same fields in the same order — just scoped to a selected
// profile.
//
// Lives in the same file as the dialog body so it shares the
// dialog's styling vocabulary; broken out only for state-locality.
function ProfilesTab() {
    const [profiles, setProfiles] = useState<main.ProfileSummary[]>([])
    const [selectedID, setSelectedID] = useState<string>('')
    const [detail, setDetail] = useState<main.ProfileDetail | null>(null)
    const [dirty, setDirty] = useState(false)
    const [status, setStatus] = useState<string>('')
    const [confirmDeleteID, setConfirmDeleteID] = useState<string>('')

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
            setDetail(null)
            return
        }
        window.go.main.Bindings.GetProfile(selectedID).then(d => {
            setDetail(d)
            setDirty(false)
        }).catch(e => console.error('GetProfile:', e))
    }, [selectedID])

    // Two-click delete confirm matches the rest of the app
    // (BulkActions / Findings / image-delete).
    useEffect(() => {
        if (!confirmDeleteID) return
        const t = setTimeout(() => setConfirmDeleteID(''), 3000)
        return () => clearTimeout(t)
    }, [confirmDeleteID])

    const patch = (p: Partial<main.ProfileDetail>) => {
        setDetail(prev => prev ? {...prev, ...p} as main.ProfileDetail : prev)
        setDirty(true)
    }
    const patchLocal = (p: Partial<main.LocalProfileFields>) => {
        setDetail(prev => prev ? {...prev, local: {...prev.local, ...p}} as main.ProfileDetail : prev)
        setDirty(true)
    }
    const patchVertex = (p: Partial<main.VertexProfileFields>) => {
        setDetail(prev => prev ? {...prev, vertex: {...prev.vertex, ...p}} as main.ProfileDetail : prev)
        setDirty(true)
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
                setStatus(`Renamed to "${res.profile.name}" (a profile named "${res.original_name}" already existed).`)
                setTimeout(() => setStatus(''), 5000)
            }
        } catch (e: any) {
            setStatus(`Create failed: ${String(e?.message || e)}`)
        }
    }

    const save = async () => {
        if (!window.go || !detail) return
        try {
            const res = await window.go.main.Bindings.UpdateProfile(detail.id, {
                name: detail.name,
                default_backend: detail.default_backend,
                local: detail.local,
                vertex: detail.vertex,
            } as main.UpdateProfileRequest)
            await refreshList()
            // Re-fetch the (possibly auto-renamed) detail.
            const fresh = await window.go.main.Bindings.GetProfile(detail.id)
            setDetail(fresh)
            setDirty(false)
            if (res.name_adjusted) {
                setStatus(`Renamed to "${res.profile.name}" (a profile named "${res.original_name}" already existed).`)
            } else {
                setStatus('Saved')
            }
            setTimeout(() => setStatus(''), 4000)
        } catch (e: any) {
            setStatus(`Save failed: ${String(e?.message || e)}`)
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
        } catch (e: any) {
            setStatus(`Delete failed: ${String(e?.message || e)}`)
            setTimeout(() => setStatus(''), 5000)
        }
    }

    const setAsDefault = async (id: string) => {
        if (!window.go) return
        try {
            await window.go.main.Bindings.SetDefaultProfile(id)
            await refreshList()
            setStatus('Default profile updated')
            setTimeout(() => setStatus(''), 3000)
        } catch (e: any) {
            setStatus(`SetDefault failed: ${String(e?.message || e)}`)
        }
    }

    return (
        <div className="settings-section">
            <h3>LLM Profiles</h3>
            <p className="sidebar-hint">
                Each profile is a pair of (Local, Vertex AI) configs plus a default side.
                Sessions reference a profile; /model toggles between the pair within the
                session's profile. See <code>docs/en/adr/0016-multi-profile-llm-backend.md</code>.
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
                    {detail ? (
                        <>
                            <label>
                                <span>Name</span>
                                <input value={detail.name} onChange={e => patch({name: e.target.value})} />
                            </label>
                            <label>
                                <span>Default side (which backend /model lands on first)</span>
                                <select value={detail.default_backend} onChange={e => patch({default_backend: e.target.value})}>
                                    <option value="local">Local LLM</option>
                                    <option value="vertex_ai">Vertex AI</option>
                                </select>
                            </label>
                            <div className="profile-action-row">
                                {!detail.is_default && (
                                    <button onClick={() => setAsDefault(detail.id)}>Set as default</button>
                                )}
                                {!detail.is_default && (
                                    confirmDeleteID === detail.id
                                        ? <button className="danger-confirm" onClick={() => removeProfile(detail.id)}>Confirm delete</button>
                                        : <button className="danger" onClick={() => setConfirmDeleteID(detail.id)}>Delete profile</button>
                                )}
                                {detail.is_default && (
                                    <span className="sidebar-hint">Default profiles cannot be deleted. Set a different default first.</span>
                                )}
                            </div>
                            <h4>Local</h4>
                            <label>
                                <span>Endpoint</span>
                                <input value={detail.local.endpoint} onChange={e => patchLocal({endpoint: e.target.value})} />
                            </label>
                            <label>
                                <span>Model</span>
                                <input value={detail.local.model} onChange={e => patchLocal({model: e.target.value})} />
                            </label>
                            <label>
                                <span>API key env var</span>
                                <input value={detail.local.api_key_env} onChange={e => patchLocal({api_key_env: e.target.value})} />
                            </label>
                            <BackendBudgetEditor
                                budget={detail.local.context_budget}
                                onChange={b => patchLocal({context_budget: b as any})}
                            />
                            <label>
                                <span>Per-request timeout (seconds)</span>
                                <input type="number" min={5} value={detail.local.request_timeout_seconds || 300} onChange={e => patchLocal({request_timeout_seconds: parseInt(e.target.value, 10) || 300})} />
                            </label>
                            <label>
                                <span>Retry max attempts</span>
                                <input type="number" min={1} max={10} value={detail.local.retry_max_attempts || 3} onChange={e => patchLocal({retry_max_attempts: parseInt(e.target.value, 10) || 3})} />
                            </label>
                            <h4>Vertex AI</h4>
                            <label>
                                <span>Project ID</span>
                                <input value={detail.vertex.project_id} onChange={e => patchVertex({project_id: e.target.value})} />
                            </label>
                            <label>
                                <span>Region</span>
                                <input value={detail.vertex.region} onChange={e => patchVertex({region: e.target.value})} />
                            </label>
                            <label>
                                <span>Model</span>
                                <input value={detail.vertex.model} onChange={e => patchVertex({model: e.target.value})} />
                            </label>
                            <BackendBudgetEditor
                                budget={detail.vertex.context_budget}
                                onChange={b => patchVertex({context_budget: b as any})}
                            />
                            <label>
                                <span>Per-request timeout (seconds)</span>
                                <input type="number" min={5} value={detail.vertex.request_timeout_seconds || 180} onChange={e => patchVertex({request_timeout_seconds: parseInt(e.target.value, 10) || 180})} />
                            </label>
                            <label>
                                <span>Retry max attempts</span>
                                <input type="number" min={1} max={10} value={detail.vertex.retry_max_attempts || 3} onChange={e => patchVertex({retry_max_attempts: parseInt(e.target.value, 10) || 3})} />
                            </label>
                            <div className="profile-save-row">
                                <button onClick={save} disabled={!dirty}>Save changes</button>
                                {status && <span className="profile-save-status">{status}</span>}
                            </div>
                        </>
                    ) : (
                        <p className="sidebar-hint">Select a profile to edit.</p>
                    )}
                </div>
            </div>
        </div>
    )
}
