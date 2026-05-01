// SettingsDialog is the full-screen settings modal with three
// tabs (General / Tools / MCP). Each field calls onUpdate with a
// partial Settings patch; the parent (App) is responsible for
// merging, persisting, and triggering side-effects (theme apply,
// sandbox restart, LLM-backend restart). MCP guardian restart
// stays in App because it also touches the tool list.

import {useState} from 'react'
import BackendBudgetEditor from '../components/BackendBudgetEditor'
import type {MCPProfile, MCPStatus, Settings, ToolInfo} from '../types'

type SettingsTab = 'general' | 'tools' | 'mcp'

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
    return (
        <div className="settings-overlay" onClick={onClose}>
            <div className="settings-modal" onClick={e => e.stopPropagation()}>
                <div className="settings-header">
                    <h2>Settings</h2>
                    <button className="settings-close" onClick={onClose}>&#x2715;</button>
                </div>
                <div className="settings-tabs">
                    <button className={tab === 'general' ? 'active' : ''} onClick={() => setTab('general')}>General</button>
                    <button className={tab === 'tools' ? 'active' : ''} onClick={() => setTab('tools')}>Tools</button>
                    <button className={tab === 'mcp' ? 'active' : ''} onClick={() => setTab('mcp')}>MCP</button>
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
                            <h3>Memory</h3>
                            <label>
                                <input type="checkbox" checked={!!settings.memory_use_v2} onChange={e => onUpdate({memory_use_v2: e.target.checked})} />
                                <span>Use v2 context builder (experimental)</span>
                            </label>
                            <p className="sidebar-hint">Records stay immutable; older context is summarized on demand and cached. Time-range markers added for LLM temporal awareness. See docs/en/memory-architecture-v2.md.</p>
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
                            <h3>Sandbox (experimental)</h3>
                            <label>
                                <input type="checkbox" checked={!!settings.sandbox?.enabled} onChange={e => onUpdate({sandbox: {...settings.sandbox, enabled: e.target.checked}})} />
                                <span>Enable container sandbox (podman/docker required)</span>
                            </label>
                            <p className="sidebar-hint">Exposes six sandbox-* tools that run shell/Python inside a per-session container. Settings changes here take effect immediately — existing sandbox containers are torn down and re-created with the new config on next tool use. Missing images are pulled automatically. See docs/en/sandbox-execution.md.</p>
                            {settings.sandbox?.enabled && (
                                <>
                                    <label>
                                        <span>Engine</span>
                                        <select value={settings.sandbox.engine || 'auto'} onChange={e => onUpdate({sandbox: {...settings.sandbox, engine: e.target.value}})}>
                                            <option value="auto">auto (podman → docker)</option>
                                            <option value="podman">podman</option>
                                            <option value="docker">docker</option>
                                        </select>
                                    </label>
                                    <label>
                                        <span>Image</span>
                                        <input value={settings.sandbox.image || ''} placeholder="python:3.12-slim" onChange={e => onUpdate({sandbox: {...settings.sandbox, image: e.target.value}})} />
                                    </label>
                                    <label>
                                        <input type="checkbox" checked={!!settings.sandbox.network} onChange={e => onUpdate({sandbox: {...settings.sandbox, network: e.target.checked}})} />
                                        <span>Allow network egress (default off)</span>
                                    </label>
                                    <label>
                                        <span>CPU limit</span>
                                        <input value={settings.sandbox.cpu_limit || ''} placeholder="2" onChange={e => onUpdate({sandbox: {...settings.sandbox, cpu_limit: e.target.value}})} />
                                    </label>
                                    <label>
                                        <span>Memory limit</span>
                                        <input value={settings.sandbox.memory_limit || ''} placeholder="1g" onChange={e => onUpdate({sandbox: {...settings.sandbox, memory_limit: e.target.value}})} />
                                    </label>
                                    <label>
                                        <span>Per-call timeout (seconds)</span>
                                        <input type="number" min={5} value={settings.sandbox.timeout_seconds || 60} onChange={e => onUpdate({sandbox: {...settings.sandbox, timeout_seconds: parseInt(e.target.value, 10) || 60}})} />
                                    </label>
                                </>
                            )}
                        </div>
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
                                const mitlDefault = t.category === 'write' || t.category === 'execute' || t.source === 'mcp'
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
                </div>
                <div className="settings-footer">
                    <button className="settings-close-btn" onClick={onClose}>Close</button>
                </div>
            </div>
        </div>
    )
}
