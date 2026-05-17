// QueuePill renders the single-slot send-queue status above the
// chat input bar. A SEND issued while the previous turn's memory
// extraction is still running lands in a.queuedSend on the agent
// side and emits agent:queued to the frontend; this component
// surfaces the user-friendly view of that state.
//
// Click ✕ calls Abort, which clears both the queued SEND and the
// in-flight extraction (ADR-0015 §3.4). The chat-pane's existing
// Abort button covers the same path; this is just a more
// proximate affordance for the queue-cleanup case.
//
// Design: docs/en/adr/0015-deferred-extraction-send.md §3.6

interface Props {
    message: string
    onCancel: () => void
}

// Trim the user-supplied message for display; the full text is
// preserved in the title attribute so a hover reveals the rest.
function truncate(s: string, n: number): string {
    if (s.length <= n) return s
    return s.slice(0, n - 1) + '…'
}

export default function QueuePill({message, onCancel}: Props) {
    const preview = truncate(message, 60)
    return (
        <div className="queue-pill" title={message}>
            <span className="queue-pill-icon" aria-hidden>{'⏳'}</span>
            <span className="queue-pill-label">
                Queued: <em>{preview}</em>
                <span className="queue-pill-hint"> — sends when memory extraction completes</span>
            </span>
            <button
                type="button"
                className="queue-pill-cancel"
                onClick={onCancel}
                title="Cancel queued message and abort extraction"
                aria-label="Cancel queued message"
            >
                {'✕'}
            </button>
        </div>
    )
}
