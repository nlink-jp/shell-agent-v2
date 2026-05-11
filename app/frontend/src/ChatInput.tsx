import {useState, useRef, useEffect, useLayoutEffect, memo} from 'react'

// PendingAttachment tracks both the data URL and the original
// filename so the chat input can preserve the name through the
// SendWithImages binding (whose new parallel `imageNames` slice
// makes it to objstore.OrigName) — the data panel + chat bubbles
// then show "audit.md" instead of the bare 32-hex object ID.
export interface PendingAttachment {
    dataURL: string
    name: string
}

interface Props {
    onSend: (text: string, attachments: PendingAttachment[]) => void
    disabled: boolean
}

function ChatInput({onSend, disabled}: Props) {
    const [input, setInput] = useState('')
    const [pendingImages, setPendingImages] = useState<PendingAttachment[]>([])
    const composingRef = useRef(false)
    const textareaRef = useRef<HTMLTextAreaElement>(null)
    const fileInputRef = useRef<HTMLInputElement>(null)

    // Auto-focus on mount (including re-mount after busy→idle)
    useEffect(() => {
        if (!disabled) textareaRef.current?.focus()
    }, [disabled])

    // Auto-grow: resize to content, capped by CSS max-height (overflow scrolls).
    // +border compensates for box-sizing: border-box so content fits without scrollbar.
    useLayoutEffect(() => {
        const ta = textareaRef.current
        if (!ta) return
        ta.style.height = 'auto'
        const border = ta.offsetHeight - ta.clientHeight
        ta.style.height = (ta.scrollHeight + border) + 'px'
    }, [input])
    const historyRef = useRef<string[]>([])
    const historyIndexRef = useRef(-1)
    const draftRef = useRef('')

    function handleSend() {
        const text = input.trim()
        if ((!text && pendingImages.length === 0) || disabled) return
        if (text) {
            historyRef.current = [text, ...historyRef.current.filter(h => h !== text)].slice(0, 50)
            historyIndexRef.current = -1
            draftRef.current = ''
        }
        const attachments = [...pendingImages]
        setInput('')
        setPendingImages([])
        onSend(text, attachments)
        setTimeout(() => textareaRef.current?.focus(), 50)
    }

    function handleKeyDown(e: React.KeyboardEvent) {
        if (e.key === 'Enter' && e.metaKey && !composingRef.current) {
            e.preventDefault()
            handleSend()
            return
        }
        if (e.key === 'ArrowUp' && !composingRef.current) {
            const textarea = e.target as HTMLTextAreaElement
            if (textarea.selectionStart === 0) {
                e.preventDefault()
                const hist = historyRef.current
                if (hist.length === 0) return
                if (historyIndexRef.current === -1) draftRef.current = input
                const nextIdx = Math.min(historyIndexRef.current + 1, hist.length - 1)
                historyIndexRef.current = nextIdx
                setInput(hist[nextIdx])
            }
        }
        if (e.key === 'ArrowDown' && !composingRef.current) {
            // ArrowDown は「履歴ナビ中」の時だけ反応する。
            // historyIndexRef.current === -1 は履歴をたどっていない
            // 通常入力状態。この場合に走らせると、空の draftRef で
            // 入力が上書きされてユーザーの入力中テキストが消える。
            if (historyIndexRef.current < 0) return
            const textarea = e.target as HTMLTextAreaElement
            if (textarea.selectionStart === textarea.value.length) {
                e.preventDefault()
                if (historyIndexRef.current === 0) {
                    historyIndexRef.current = -1
                    setInput(draftRef.current)
                } else {
                    historyIndexRef.current--
                    setInput(historyRef.current[historyIndexRef.current])
                }
            }
        }
    }

    // Predicate: which files the chat input accepts as attachments.
    // v0.5 widens beyond image/* to include text/markdown and
    // text/plain. Some browsers (notably older Safari) don't set
    // f.type for .md / .txt; fall back to the file-name extension
    // so drag-drop of a .md file still works in those environments.
    function isAcceptedAttachment(f: File): boolean {
        if (f.type.startsWith('image/')) return true
        if (f.type === 'text/markdown' || f.type === 'text/plain') return true
        const lower = f.name.toLowerCase()
        if (lower.endsWith('.md') || lower.endsWith('.markdown')) return true
        if (lower.endsWith('.txt')) return true
        return false
    }

    // v0.5: 50 MB hard cap. Mirrors objstore.MaxAttachmentBytes on
    // the Go side. The frontend check exists so we can give a
    // friendlier error than the truncated stack trace that would
    // come back from SaveDataURL after the data-URL round-trip.
    const MAX_ATTACHMENT_BYTES = 50 * 1024 * 1024

    async function addImages(files: FileList | File[]) {
        // FileReader is async; reading files in a plain for-loop and
        // appending each onload result lets readers race — bigger
        // files finish later, so pendingImages can end up in a
        // different order than the user actually attached. Read all
        // in parallel, await, then append in the original order.
        const fileArr = Array.from(files).filter(isAcceptedAttachment)
        const oversized = fileArr.filter(f => f.size > MAX_ATTACHMENT_BYTES)
        if (oversized.length > 0) {
            const names = oversized.map(f => `${f.name} (${(f.size / 1024 / 1024).toFixed(1)} MB)`).join(', ')
            alert(`File too large to attach (limit 50 MB): ${names}\nFor larger documents, copy the file into the sandbox /work directory and use register-object instead.`)
        }
        const accepted = fileArr.filter(f => f.size <= MAX_ATTACHMENT_BYTES)
        const attached: PendingAttachment[] = await Promise.all(accepted.map(file =>
            new Promise<PendingAttachment>((resolve, reject) => {
                const reader = new FileReader()
                reader.onload = () => resolve({
                    dataURL: reader.result as string,
                    // file.name is empty for clipboard-paste images
                    // ("image/png" item with no filename) — leave it
                    // empty so objstore.OrigName stays "" and the
                    // bubble/data panel falls back to the object ID
                    // (matches the pre-v0.5 paste-image experience).
                    name: file.name || '',
                })
                reader.onerror = () => reject(reader.error)
                reader.readAsDataURL(file)
            })
        ))
        setPendingImages(prev => [...prev, ...attached])
    }

    function handlePaste(e: React.ClipboardEvent) {
        const accepted: File[] = []
        for (const item of Array.from(e.clipboardData.items)) {
            const file = item.getAsFile()
            if (file && isAcceptedAttachment(file)) {
                accepted.push(file)
            }
        }
        if (accepted.length > 0) {
            e.preventDefault()
            addImages(accepted)
        }
    }

    return (
        <div className="input-area"
            onDrop={e => { e.preventDefault(); if (e.dataTransfer.files.length > 0) addImages(e.dataTransfer.files) }}
            onDragOver={e => e.preventDefault()}>
            {pendingImages.length > 0 && (
                <div className="pending-images">
                    {pendingImages.map((att, i) => {
                        const isImage = att.dataURL.startsWith('data:image/')
                        const label = att.name || (
                            att.dataURL.startsWith('data:text/markdown') ? 'markdown'
                            : att.dataURL.startsWith('data:text/plain') ? 'text'
                            : 'document'
                        )
                        return (
                            <div key={i} className={isImage ? "pending-image" : "pending-image pending-attachment-doc"} title={att.name || undefined}>
                                {isImage
                                    ? <img src={att.dataURL} alt={att.name} />
                                    : <span className="pending-attachment-label">📝 {label}</span>}
                                <button className="pending-image-remove" onClick={() => setPendingImages(prev => prev.filter((_, j) => j !== i))}>&#x2715;</button>
                            </div>
                        )
                    })}
                </div>
            )}
            <div className="input-row">
                <input ref={fileInputRef} type="file" accept="image/*,text/markdown,text/plain,.md,.markdown,.txt" multiple style={{display: 'none'}} onChange={e => { if (e.target.files) addImages(e.target.files); e.target.value = '' }} />
                <div className="textarea-wrap">
                    <textarea
                        ref={textareaRef}
                        value={input}
                        onChange={e => { setInput(e.target.value); historyIndexRef.current = -1 }}
                        onKeyDown={handleKeyDown}
                        onPaste={handlePaste}
                        onCompositionStart={() => { composingRef.current = true }}
                        onCompositionEnd={() => { setTimeout(() => { composingRef.current = false }, 50) }}
                        placeholder={disabled ? 'Agent is busy...' : 'Type a message... (Cmd+Enter to send)'}
                        disabled={disabled}
                        rows={3}
                        autoCorrect="off"
                        autoCapitalize="off"
                        spellCheck={false}
                    />
                    <button className="attach-btn" onClick={() => fileInputRef.current?.click()} disabled={disabled} title="Attach image, markdown, or text">&#x1F4CE;</button>
                </div>
                <button onClick={handleSend} disabled={disabled || (!input.trim() && pendingImages.length === 0)}>
                    {disabled ? '...' : 'Send'}
                </button>
            </div>
        </div>
    )
}

export default memo(ChatInput)
