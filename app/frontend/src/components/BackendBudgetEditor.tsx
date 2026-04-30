// BackendBudgetEditor renders the four numeric fields that
// configure a per-backend context budget (hot-token threshold,
// max context, warm-summary cap, per-tool-result truncation).
// The Settings dialog uses one instance per backend.

import type {BackendBudget} from '../types'

interface Props {
    budget: BackendBudget;
    onChange: (b: BackendBudget) => void;
}

export default function BackendBudgetEditor({budget, onChange}: Props) {
    const num = (v: string) => Math.max(0, parseInt(v, 10) || 0)
    return (
        <div className="budget-editor">
            <label>
                <span>Hot Token Limit (compaction trigger)</span>
                <input type="number" min={0} value={budget.hot_token_limit} onChange={e => onChange({...budget, hot_token_limit: num(e.target.value)})} />
            </label>
            <label>
                <span>Max Context Tokens (0 = unlimited)</span>
                <input type="number" min={0} value={budget.max_context_tokens} onChange={e => onChange({...budget, max_context_tokens: num(e.target.value)})} />
            </label>
            <label>
                <span>Max Warm Summary Tokens</span>
                <input type="number" min={0} value={budget.max_warm_tokens} onChange={e => onChange({...budget, max_warm_tokens: num(e.target.value)})} />
            </label>
            <label>
                <span>Max Tool-Result Tokens (per call)</span>
                <input type="number" min={0} value={budget.max_tool_result_tokens} onChange={e => onChange({...budget, max_tool_result_tokens: num(e.target.value)})} />
            </label>
        </div>
    )
}
