# Central Object Storage — Design Document

> Date: 2026-04-26
> Status: Draft
> Related: [Agent Data Flow](agent-data-flow.md) Section 5

## 1. Purpose

Central Object Storage (objstore) provides a single repository for all
binary and structured artifacts produced or consumed during agent sessions.
Objects are referenced by 12-character hex IDs throughout the system —
session records, LLM context, Markdown content, and frontend display.

### 1.1 What is an Object?

Any discrete artifact that:
- Has a binary or text representation on disk
- Needs to be referenced across session records
- May be viewed, downloaded, or embedded by the user

Examples: user-uploaded images, tool-generated charts, analysis result
exports, report documents, shell tool output files.

### 1.2 Why Centralize?

v1 had images stored as data URLs directly in session records, causing:
- Session JSON bloat (single image = 100KB+ of base64)
- Duplicate storage when the same image is referenced multiple times
- No way for the LLM to reference images by identifier
- Report images could not be inlined (no stable reference)

v2 stores only IDs in records. Binary data lives in objstore.

## 2. Object Model

### 2.1 ObjectMeta

```go
type ObjectMeta struct {
    ID        string     `json:"id"`
    Type      ObjectType `json:"type"`
    MimeType  string     `json:"mime_type"`
    Filename  string     `json:"filename"`
    CreatedAt time.Time  `json:"created_at"`
    SessionID string     `json:"session_id,omitempty"`
    Size      int64      `json:"size"`
}
```

### 2.2 Object Types

| Type | Constant | Created by | Examples |
|------|----------|-----------|----------|
| Image | `TypeImage` | User upload, tool output | PNG, JPG, WebP |
| Blob | `TypeBlob` | Shell tool artifacts | CSV, JSON, text files |
| Report | `TypeReport` | create-report tool | Markdown documents |

### 2.3 ID Generation

- 12-character hexadecimal (6 random bytes via `crypto/rand`)
- Collision risk: ~1 in 281 trillion
- No prefix or namespace encoding — type stored in metadata

## 3. Storage Layout

```
~/Library/Application Support/shell-agent-v2/
├── objects/
│   ├── index.json            # ObjectMeta array (authoritative)
│   └── data/                 # Binary files
│       ├── a1b2c3d4e5f6.png  # ID + extension from MIME
│       ├── f6e5d4c3b2a1.md
│       └── ...
├── sessions/
│   └── {session-id}/
│       ├── chat.json          # Records reference objects by ID
│       └── analysis.duckdb
├── pinned.json
├── findings.json
└── config.json
```

### 3.1 Index File

`index.json` is a JSON array of ObjectMeta. It is the authoritative
source of object metadata.

- **Written** after every mutation (Save, Delete)
- **Loaded** on startup
- **Rebuilt** from filesystem if index.json is missing or corrupted
- **Thread-safe** via `sync.RWMutex`

### 3.2 Data Files

Binary data stored as `{ID}.{extension}` in the `data/` directory.
Extension derived from MIME type at save time.

## 4. Scope: Global vs Session

### 4.1 Design Decision: Global with Session Affinity

Objects are stored in a **single global repository**, not per-session
directories. This matches v1's design and enables:

- Cross-session object sharing (Findings can reference images from
  other sessions)
- Simple implementation (one Store instance for the app)
- No object migration when moving between sessions

### 4.2 Session Affinity

`ObjectMeta.SessionID` tracks which session created the object.
This enables:

- **Cleanup on session deletion**: remove objects created by that session
- **Listing by session**: show only relevant objects in LLM tools
- **Future**: per-session storage quotas

Unlike v1 where SessionID was defined but never populated, v2 MUST
set SessionID on every Save call.

## 5. Lifecycle

### 5.1 Creation

```
Source                  → Save method          → Stored as
────────────────────────────────────────────────────────────
User image upload       → SaveDataURL()        → TypeImage
Shell tool artifact     → Save()               → TypeBlob
create-report output    → Save()               → TypeReport
query-sql result export → Save()               → TypeBlob
```

Every Save call:
1. Generates ID
2. Writes binary to `data/{ID}.{ext}`
3. Creates ObjectMeta with SessionID
4. Appends to index, persists index.json
5. Returns ID

### 5.2 Reference

Objects are referenced by ID in:

| Location | Format | Example |
|----------|--------|---------|
| Session Record | `ObjectIDs []string` field | `["a1b2c3d4e5f6"]` |
| LLM context | Text marker | `[Image ID: a1b2c3d4e5f6]` |
| Markdown content | URL scheme | `![desc](object:a1b2c3d4e5f6)` |
| Tool result | Artifact marker | `[Artifacts: a1b2c3d4e5f6]` |

### 5.3 Resolution

```
object:ID in Markdown
  → Frontend ReactMarkdown img component
    → src starts with "object:" ?
      → Call GetObjectDataURL(id) binding
        → objstore.LoadAsDataURL(id)
          → Read binary from disk
          → Encode base64
          → Return data:mime;base64,...
```

### 5.4 Deletion

Explicit deletion via:
- Session deletion (DeleteSessionDir) → delete all objects with matching SessionID
- Manual cleanup (future admin UI)

No automatic GC in v2 scope. Objects persist until explicitly deleted.

## 6. LLM Integration

### 6.1 Image Sending to LLM

Per v1's proven pattern:

```
Building messages for LLM:
  For each record with ObjectIDs:
    If record is the LATEST with images:
      → Load full data URL from objstore
      → Send as multimodal content (OpenAI Vision format)
      → Include label: "[Image ID: abc123, attached at 15:04:05]"
    
    If record has images but is NOT the latest:
      → Send as text reference only
      → "[Past image ID: abc123 — use get-object tool to view again]"
```

Rationale: sending all images as data URLs wastes context window and
confuses VLMs that try to analyze every image simultaneously.

### 6.2 LLM Tools

#### list-objects

```json
{
  "name": "list-objects",
  "description": "List all objects in the current session (images, artifacts, reports). Returns ID, type, filename, and creation time for each.",
  "parameters": {
    "type": "object",
    "properties": {
      "type_filter": {
        "type": "string",
        "enum": ["image", "blob", "report", "all"],
        "description": "Filter by object type (default: all)"
      }
    }
  }
}
```

Returns: Formatted list of objects with IDs, types, and descriptions.

#### get-object

```json
{
  "name": "get-object",
  "description": "Retrieve an object's content by ID. For images, returns a marker that the system will resolve to the actual image. For text/data, returns the content directly.",
  "parameters": {
    "type": "object",
    "properties": {
      "id": {
        "type": "string",
        "description": "Object ID (12-character hex)"
      }
    },
    "required": ["id"]
  }
}
```

Returns:
- For images: `__IMAGE_RECALL_BLOB__{id}__` marker (resolved by message builder)
- For text: content as string (truncated to 30KB)
- For binary: metadata description only

### 6.3 Object References in Reports

When the LLM creates a report with `create-report` tool:

1. LLM calls `list-objects` to discover session images
2. LLM writes report with `![description](object:abc123)` syntax
3. `create-report` handler stores report in objstore (TypeReport)
4. Report record stores both report ID and referenced image IDs
5. Frontend renders: ReactMarkdown resolves `object:` URLs via binding

### 6.4 Image Recall Markers

When LLM calls `get-object` for an image:
- Tool returns `__IMAGE_RECALL_BLOB__{id}__` (text marker)
- Message builder detects this marker in tool results
- Expands to actual data URL for the next LLM call
- Prevents data URL from polluting tool result records

## 7. Frontend Integration

### 7.1 Wails Bindings

```go
// Save a data URL (from drag/drop, paste) → returns ID
SaveImage(dataURL string) (string, error)

// Load object as data URL for display
GetObjectDataURL(id string) (string, error)

// Save report/content to file (via save dialog)
SaveObjectToFile(id string) error
```

### 7.2 ReactMarkdown Object Resolution

```tsx
<ReactMarkdown
  components={{
    img: ({src, alt}) => {
      if (src?.startsWith('object:')) {
        const id = src.slice(7)
        // Resolve via GetObjectDataURL binding
        return <ObjectImage id={id} alt={alt} />
      }
      return <img src={src} alt={alt} />
    }
  }}
/>
```

### 7.3 Lazy Loading

Frontend maintains an in-memory cache:
- First access: call `GetObjectDataURL(id)` → cache result
- Subsequent access: return from cache
- Cache invalidation: on session switch

## 8. v1 → v2 Differences

| Aspect | v1 | v2 |
|--------|-----|-----|
| SessionID | Defined but never set | Always set on Save |
| Record field | `Images []ImageEntry` | `ObjectIDs []string` |
| Image in record | `ImageEntry{ID, MimeType}` | Just the ID string |
| Reference syntax | `image:ID`, `blob:ID` | `object:ID` (unified) |
| GC | None | Delete by session |
| Report storage | Separate from objstore | In objstore as TypeReport |
| Index rebuild | Defaults all to TypeImage | Preserves type if possible |

## 9. Implementation Checklist

### Phase 1: Core objstore update
- [ ] Add ObjectType constants (TypeImage, TypeBlob, TypeReport)
- [ ] Add SessionID to ObjectMeta, set on every Save
- [ ] Add ListBySession(sessionID) method
- [ ] Add DeleteBySession(sessionID) method
- [ ] Update index rebuild to handle types

### Phase 2: Record migration
- [ ] Replace `ImageURLs []string` with `ObjectIDs []string` in Record
- [ ] Update AddUserMessage to accept object IDs, not data URLs
- [ ] Update bindings: SendWithImages saves to objstore first, passes IDs
- [ ] Update BuildMessages: resolve latest image from objstore

### Phase 3: LLM tools
- [ ] Implement list-objects tool
- [ ] Implement get-object tool with image recall markers
- [ ] Add view-image backward compat alias
- [ ] Update create-report to store in objstore

### Phase 4: Frontend
- [ ] ReactMarkdown object: URL resolver component
- [ ] Image cache with lazy loading
- [ ] Report save with object: URL resolution to inline base64
