// Manual test: run this in a browser/React environment to verify
// ReactMarkdown v10 img component behavior.
//
// Test cases:
// 1. Standard image URL → should render <img>
// 2. object: protocol URL → should it render <img>?
// 3. Custom components.img → does it fire?
// 4. urlTransform → does it affect object: URLs?

import ReactMarkdown from 'react-markdown'
import {defaultUrlTransform} from 'react-markdown'

// Test 1: Standard Markdown image
const test1 = `![alt text](https://example.com/image.png)`

// Test 2: object: protocol
const test2 = `![alt text](object:abc123)`

// Test 3: data: protocol (known to be stripped by default)
const test3 = `![alt text](data:image/png;base64,iVBOR)`

// Test 4: Simple text with no special protocol
const test4 = `Hello world`

// Test 5: Image only, no other content
const test5 = `![test](object:abc123)`

// defaultUrlTransform behavior test
console.log('=== defaultUrlTransform tests ===')
console.log('https:', defaultUrlTransform('https://example.com/img.png'))
console.log('object:', defaultUrlTransform('object:abc123'))
console.log('data:', defaultUrlTransform('data:image/png;base64,x'))

// The key question: does defaultUrlTransform strip object: URLs?
// If yes, we need urlTransform override.
// If no, the problem is elsewhere.

export default function TestMarkdown() {
    return (
        <div style={{padding: 20}}>
            <h2>Test 1: Standard URL</h2>
            <ReactMarkdown>{test1}</ReactMarkdown>

            <h2>Test 2: object: URL (default transform)</h2>
            <ReactMarkdown>{test2}</ReactMarkdown>

            <h2>Test 3: object: URL (custom transform - passthrough)</h2>
            <ReactMarkdown urlTransform={(url) => url}>{test2}</ReactMarkdown>

            <h2>Test 4: object: URL (custom transform + custom img)</h2>
            <ReactMarkdown
                urlTransform={(url) => url}
                components={{
                    img: ({src, alt, ...props}) => {
                        console.log('IMG COMPONENT CALLED:', src, alt)
                        return <img src={src} alt={alt || ''} style={{border: '2px solid red'}} {...props} />
                    }
                }}
            >
                {test2}
            </ReactMarkdown>

            <h2>Test 5: data: URL (custom transform)</h2>
            <ReactMarkdown urlTransform={(url) => url}>{test3}</ReactMarkdown>
        </div>
    )
}
