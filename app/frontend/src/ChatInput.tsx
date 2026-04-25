import {useState, useRef, memo} from 'react'

interface Props {
    onSend: (text: string, images: string[]) => void
    disabled: boolean
}

function ChatInput({onSend, disabled}: Props) {
    const [input, setInput] = useState('')
    const [pendingImages, setPendingImages] = useState<string[]>([])
    const composingRef = useRef(false)
    const fileInputRef = useRef<HTMLInputElement>(null)
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
        const images = [...pendingImages]
        setInput('')
        setPendingImages([])
        onSend(text, images)
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
            const textarea = e.target as HTMLTextAreaElement
            if (textarea.selectionStart === textarea.value.length) {
                e.preventDefault()
                if (historyIndexRef.current <= 0) {
                    historyIndexRef.current = -1
                    setInput(draftRef.current)
                } else {
                    historyIndexRef.current--
                    setInput(historyRef.current[historyIndexRef.current])
                }
            }
        }
    }

    async function addImages(files: FileList | File[]) {
        for (const file of Array.from(files)) {
            if (file.type.startsWith('image/')) {
                const reader = new FileReader()
                reader.onload = () => {
                    setPendingImages(prev => [...prev, reader.result as string])
                }
                reader.readAsDataURL(file)
            }
        }
    }

    function handlePaste(e: React.ClipboardEvent) {
        const imageFiles: File[] = []
        for (const item of Array.from(e.clipboardData.items)) {
            if (item.type.startsWith('image/')) {
                const file = item.getAsFile()
                if (file) imageFiles.push(file)
            }
        }
        if (imageFiles.length > 0) {
            e.preventDefault()
            addImages(imageFiles)
        }
    }

    return (
        <div className="input-area"
            onDrop={e => { e.preventDefault(); if (e.dataTransfer.files.length > 0) addImages(e.dataTransfer.files) }}
            onDragOver={e => e.preventDefault()}>
            {pendingImages.length > 0 && (
                <div className="pending-images">
                    {pendingImages.map((img, i) => (
                        <div key={i} className="pending-image">
                            <img src={img} alt="" />
                            <button className="pending-image-remove" onClick={() => setPendingImages(prev => prev.filter((_, j) => j !== i))}>&#x2715;</button>
                        </div>
                    ))}
                </div>
            )}
            <div className="input-row">
                <button className="attach-btn" onClick={() => fileInputRef.current?.click()} disabled={disabled} title="Attach image">&#x1F4CE;</button>
                <input ref={fileInputRef} type="file" accept="image/*" multiple style={{display: 'none'}} onChange={e => { if (e.target.files) addImages(e.target.files); e.target.value = '' }} />
                <textarea
                    value={input}
                    onChange={e => { setInput(e.target.value); historyIndexRef.current = -1 }}
                    onKeyDown={handleKeyDown}
                    onPaste={handlePaste}
                    onCompositionStart={() => { composingRef.current = true }}
                    onCompositionEnd={() => { setTimeout(() => { composingRef.current = false }, 50) }}
                    placeholder={disabled ? 'Agent is busy...' : 'Type a message... (Cmd+Enter to send)'}
                    disabled={disabled}
                    rows={3}
                />
                <button onClick={handleSend} disabled={disabled || (!input.trim() && pendingImages.length === 0)}>
                    {disabled ? '...' : 'Send'}
                </button>
            </div>
        </div>
    )
}

export default memo(ChatInput)
