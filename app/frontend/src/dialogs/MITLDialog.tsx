// MITLDialog renders the human-in-the-loop approval prompt for
// tools whose category requires confirmation. The dialog body
// adapts to the request category (sql_preview, analysis_plan,
// code-bearing sandbox tool, or generic). All Wails binding
// calls go through props so the dialog has no direct
// dependency on the bindings layer.

import {useState} from 'react'
import type {MITLRequest} from '../types'

interface Props {
    request: MITLRequest;
    onApprove: () => void;
    onReject: () => void;
    onRejectWithFeedback: (feedback: string) => void;
}

export default function MITLDialog({request, onApprove, onReject, onRejectWithFeedback}: Props) {
    const [feedback, setFeedback] = useState('')

    const submitReject = () => {
        const trimmed = feedback.trim()
        setFeedback('')
        if (trimmed) {
            onRejectWithFeedback(trimmed)
        } else {
            onReject()
        }
    }

    return (
        <div className="mitl-dialog">
            <div className="mitl-header">
                <span className="mitl-icon">&#9888;</span>
                <span>{request.category === 'sql_preview' ? 'SQL Execution Preview'
                    : request.category === 'analysis_plan' ? 'Analysis Plan Confirmation'
                    : 'Tool Approval Required'}</span>
            </div>
            <div className="mitl-body">
                {(() => {
                    try {
                        const args = JSON.parse(request.arguments)
                        if (request.category === 'sql_preview') {
                            return (<>
                                <div className="mitl-section">
                                    <span className="mitl-label">SQL:</span>
                                    <pre className="mitl-sql">{args.sql || request.arguments}</pre>
                                </div>
                            </>)
                        }
                        if (request.category === 'analysis_plan') {
                            return (<>
                                <div className="mitl-section">
                                    <span className="mitl-label">Analysis Perspective:</span>
                                    <div className="mitl-perspective">{args.prompt}</div>
                                </div>
                                {args.table && (
                                    <div className="mitl-section">
                                        <span className="mitl-label">Target Table:</span>
                                        <code>{args.table}</code>
                                    </div>
                                )}
                            </>)
                        }
                        // Sandbox code-bearing tools: render the code/SQL/content
                        // field as raw multi-line text so the user can actually
                        // read what they're approving. JSON.stringify keeps the
                        // entire body on one logical line which is unusable for
                        // anything beyond a few words of Python.
                        const codeFieldByTool: Record<string, string> = {
                            'sandbox-run-shell': 'command',
                            'sandbox-run-python': 'code',
                            'sandbox-write-file': 'content',
                            'sandbox-export-sql': 'sql',
                        }
                        const codeField = codeFieldByTool[request.tool_name]
                        if (codeField && typeof args[codeField] === 'string') {
                            const codeBody: string = args[codeField]
                            const otherArgs: Record<string, any> = {}
                            for (const k of Object.keys(args)) {
                                if (k !== codeField) otherArgs[k] = args[k]
                            }
                            return (<>
                                <div className="mitl-tool-name">
                                    <span className="mitl-label">Tool:</span>
                                    <code>{request.tool_name}</code>
                                    <span className={`tool-category ${request.category}`}>{request.category}</span>
                                </div>
                                {Object.keys(otherArgs).length > 0 && (
                                    <div className="mitl-section">
                                        <span className="mitl-label">Arguments:</span>
                                        <pre>{JSON.stringify(otherArgs, null, 2)}</pre>
                                    </div>
                                )}
                                <div className="mitl-section">
                                    <span className="mitl-label">{codeField}:</span>
                                    <pre className="mitl-code">{codeBody}</pre>
                                </div>
                            </>)
                        }
                        // Default: shell tools etc.
                        return (<>
                            <div className="mitl-tool-name">
                                <span className="mitl-label">Tool:</span>
                                <code>{request.tool_name}</code>
                                <span className={`tool-category ${request.category}`}>{request.category}</span>
                            </div>
                            <div className="mitl-section">
                                <span className="mitl-label">Arguments:</span>
                                <pre>{JSON.stringify(args, null, 2)}</pre>
                            </div>
                        </>)
                    } catch {
                        return (<div className="mitl-section">
                            <span className="mitl-label">Details:</span>
                            <pre>{request.arguments}</pre>
                        </div>)
                    }
                })()}
                <div className="mitl-feedback">
                    <input
                        type="text"
                        placeholder="Rejection reason / revision suggestion (optional)"
                        value={feedback}
                        onChange={e => setFeedback(e.target.value)}
                        onKeyDown={e => {
                            if (e.key === 'Enter' && feedback.trim()) {
                                onRejectWithFeedback(feedback.trim())
                                setFeedback('')
                            }
                        }}
                    />
                </div>
            </div>
            <div className="mitl-actions">
                <button className="mitl-approve" onClick={() => { setFeedback(''); onApprove() }}>Approve</button>
                <button className="mitl-reject" onClick={submitReject}>Reject</button>
            </div>
        </div>
    )
}
