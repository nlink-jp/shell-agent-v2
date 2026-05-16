# Object-link rendering — `[name](object:ID)` previews for text / markdown — Design Note

**Status:** Implemented in v0.9.0 (2026-05-16).
**Target:** v0.9.0 (minor bump on top of v0.8.0 — new user-visible
rendering behaviour, one new Wails binding, no breaking changes,
two parallel-list drift sites collapsed to one).
**Reported by:** User — "when the LLM points at text or markdown
objects from inside a report, the embedded-object behaviour
breaks." Inline images (`![alt](object:ID)`) work; non-image
references (`[name](object:ID)` pointing at `TypeMarkdown` /
`TypeReport`) render as inert `<a href="object:ID">` links that
do nothing on click and mis-cooperate with the export resolver.

This note specifies a **symmetric `a`-component override** that
matches the existing `img` override, a new `ObjectLink` frontend
component, a tiny `GetObjectMeta` backend binding for type
discrimination, a type-aware fix in `resolveObjectRefsForExport`,
and the matching tool-descriptor / system-prompt edits that teach
the LLM the now-supported `[name](object:ID)` reference form.

---

## 1. Problem

shell-agent-v2 stores four object types
(`internal/objstore/objstore.go:30`):

| Type             | Provenance                                         | Natural inline form |
|------------------|----------------------------------------------------|---------------------|
| `TypeImage`      | user attachment / `generate-image` / `register-object` | `![alt](object:ID)` |
| `TypeMarkdown`   | user-attached `.md` / `.txt` (v0.5.0, ADR-0006)    | `[name](object:ID)` |
| `TypeReport`     | `create-report` output                             | `[name](object:ID)` |
| `TypeBlob`       | any other binary                                   | `[name](object:ID)` (download) |

The chat renderer (`MessageItem.tsx`,
`dialogs/ReportViewer.tsx`, `App.tsx`'s cmd-popup
`ReactMarkdown`) only overrides the `img` component for
`object:` URLs:

```tsx
// MessageItem.tsx:79
img: ({src, alt}) => {
    if (src?.startsWith('object:')) {
        const id = src.slice(7)
        return <ObjectImage id={id} alt={alt || ''} onClick={onLightbox} />
    }
    return <img src={src} alt={alt || ''} className="message-image" onClick={...} />
},
```

`urlTransform` allows `object:` through ReactMarkdown's
sanitiser at all three sites, but there is **no `a`-component
override**. The consequences:

1. **Inert link.** LLM writes `[my-doc.md](object:abc123…)` for
   a `TypeMarkdown` source. ReactMarkdown emits
   `<a href="object:abc123…">my-doc.md</a>`. The browser does
   not understand the `object:` protocol; clicking is a no-op
   (or shows a "Cannot open" dialog in some Wails / WebKit
   builds). The user sees an underlined link that does nothing.

2. **Image-via-link misrendering.** Some LLMs (especially after
   reading `list-objects`) will emit
   `[![](object:imgID)](object:reportID)` — an image wrapped in
   a link. The image renders fine via the existing
   `ObjectImage` override, but the outer `<a>` is inert, so the
   user clicks the image and nothing happens (or gets a broken
   browser navigation attempt).

3. **Mistyped `![alt](object:ID)` for a non-image.** LLM
   confuses syntax and writes `![my-doc.md](object:abc123…)`
   where `abc123…` is `TypeMarkdown`. `ObjectImage` calls
   `Bindings.GetImageDataURL(id)` → `objects.LoadAsDataURL`
   returns `data:text/markdown;base64,…` → `<img>` cannot
   render markdown → broken-image glyph. The report's other
   image embeds still render, but this one is "broken" and the
   user reads the whole report as flaky.

4. **Export resolver type-blindness.** `Bindings.
   resolveObjectRefsForExport` (`bindings.go:1595`) scans for
   `(object:` indiscriminately, then calls `LoadAsDataURL` on
   every match. For a `[name](object:ID)` link where `ID` is
   `TypeMarkdown` / `TypeReport`, the link's `href` is rewritten
   to `data:text/markdown;base64,…` — which is even less useful
   than the inert `object:` href, because external markdown
   readers don't render `data:` URLs as inline previews. The
   exported `.md` ends up with kilobyte-long inline `href` blobs
   for every text reference, and the link still doesn't do
   anything useful when opened.

5. **Drift surface area.** Three React sites each carry a
   parallel copy of `urlTransform` plus the `img` override. The
   bug above bites in all three, and any new component
   (`a`, `code`, future `details`) has to be added to three
   files. This is the same parallel-list drift pattern v0.6.0
   resolved on the tool-descriptor side with the registry refactor
   (ADR-0007).

The capability gap is purely on the rendering side. The
plumbing for "reference an object by ID" already exists end to
end: object IDs are stable 32-hex strings persisted in the
record, `Bindings.GetObjectText` already fetches markdown bodies
on click for the document-attachment chip
(`MessageItem.tsx:42 openDocumentAttachment`), and ReportViewer
already accepts a `{title, content}` pair from any source. We
need to wire that same flow to inline links.

---

## 2. Goals

1. **Symmetric component override.** `[name](object:ID)` renders
   as a clickable preview chip whose behaviour depends on the
   referenced object's type, mirroring how `![alt](object:ID)`
   renders as an image whose behaviour depends on the referenced
   object's MIME. Both forms become first-class.
2. **Single source of truth for markdown defaults.** The
   `urlTransform` function and the object-aware `img` / `a`
   component overrides live in **one module** (`frontend/src/
   markdown/objectMarkdown.tsx`); the three current sites import
   from it. New components can be added in one place and propagate
   automatically.
3. **Graceful fallback for type mismatches.** An
   `![alt](object:ID)` whose ID is non-image renders as an
   `ObjectLink` chip (not a broken-image glyph); a
   `[name](object:ID)` whose ID is image renders as an inline
   `<ObjectImage>` (not an inert link). The renderer normalises
   the LLM's intent against the object's actual type.
4. **Type-aware export.** `resolveObjectRefsForExport` inlines
   `data:` URLs only for `TypeImage`; other types leave the
   `object:ID` href untouched (re-openable inside the app) or, in
   a later phase, are expanded as `<details>` blocks. No more
   kilobyte `data:text/markdown` blobs in saved `.md` files.
5. **Tool / prompt clarity.** The `create-report` descriptor and
   the agent system prompt name the two reference forms explicitly
   side by side. LLMs that read either source learn both
   conventions in one pass.

Non-goals:

- **Transclusion / `{{embed:ID}}` directive.** Not in v0.9.0. See
  §5.1. The click-to-preview UX from goals 1–3 covers the common
  case (provenance + jump-to-source); inline expansion is a
  separate ADR if a real need surfaces.
- **Nested-report back stack.** Opening Report B from inside
  Report A's `ReportViewer` simply replaces the viewer content
  (§3.1.4). Stacked / breadcrumb navigation is deferred.
- **Export as multi-file bundle.** A single self-contained `.md`
  remains the export format; only the `data:`-URL behaviour for
  non-images is corrected. Multi-file export (linked `.md` siblings
  in a folder) is out of scope.
- **Cross-session object visibility tightening.** Whatever
  visibility model `Bindings.GetObjectText` already enforces is
  reused unchanged; this ADR does not change which objects an
  active session can see.

---

## 3. Design

### 3.1 Frontend

#### 3.1.1 New module: `frontend/src/markdown/objectMarkdown.tsx`

Single source of truth for `object:`-aware ReactMarkdown wiring.
Exports:

```ts
// Allowed protocols + object: passthrough. Drop-in replacement
// for the three current copies of urlTransform.
export function urlTransform(url: string): string

// Factory: returns the ReactMarkdown `components` map populated
// with object:-aware img and a overrides. Callers supply the
// callbacks they own (lightbox / report-viewer).
export function objectComponents(opts: {
    onLightbox: (src: string) => void;
    onExpandReport: (r: {title: string; content: string}) => void;
}): Components
```

Internally, `objectComponents` returns:

```ts
{
    img: ({src, alt}) => /* see 3.1.2 */,
    a:   ({href, children}) => /* see 3.1.3 */,
}
```

Both overrides delegate to a shared `useObjectMeta(id)` hook (also
exported, used by `ObjectLink`) that fetches `GetObjectMeta(id)`
once per ID, dedupes via the same in-memory cache the existing
`objectCache` (`ObjectImage.tsx:4`) already uses for data URLs.
Meta cache and data-URL cache are sibling maps keyed by ID;
`clearObjectCache` (called on session switch) clears both.

#### 3.1.2 `img` override behaviour

`img` with `src` starting `object:` — extract ID, fetch meta:

| Object type             | Render                                                    |
|-------------------------|-----------------------------------------------------------|
| `TypeImage`             | existing `<ObjectImage>` (data URL → `<img>` + lightbox)  |
| `TypeMarkdown` / `TypeReport` | `<ObjectLink>` chip (the LLM mistyped — degrade)    |
| `TypeBlob`              | `<ObjectLink>` chip (download)                            |
| meta fetch fails        | tiny "Object {id} not found" badge — existing error span  |

Behaviour identical to current code for the `TypeImage` happy
path; only the mismatch fallback is new.

#### 3.1.3 `a` override behaviour

`a` with `href` starting `object:` — extract ID, fetch meta:

| Object type    | Render                                                                                                |
|----------------|-------------------------------------------------------------------------------------------------------|
| `TypeImage`    | `<ObjectImage>` inline (matches user's mistyped intent; same lightbox affordance as `![…](object:…)`) |
| `TypeMarkdown` | `<ObjectLink>` chip with `📝` icon — click → `openDocumentAttachment`-equivalent → `ReportViewer`    |
| `TypeReport`   | `<ObjectLink>` chip with `📄` icon — click → `ReportViewer` with the report's stored content         |
| `TypeBlob`     | `<ObjectLink>` chip with `📎` icon — click → existing `Bindings.ExportObject(id)` (save-as dialog)   |
| meta missing   | `<ObjectLink>` chip in muted state titled "missing object"; click is a no-op                          |

For all non-`object:` `href` values, fall back to a regular
`<a target="_blank" rel="noreferrer">`.

#### 3.1.4 New component: `frontend/src/ObjectLink.tsx`

Sibling of `ObjectImage`. Owns the visual chip and the click
handler dispatch. Props:

```ts
interface Props {
    id: string;
    children: React.ReactNode;       // the link text the LLM wrote
    onExpandReport: (r: {title: string; content: string}) => void;
    onLightbox: (src: string) => void;
}
```

Internally calls `useObjectMeta(id)`:
- **Loading**: muted chip "Loading…" (matches `ObjectImage` skeleton).
- **Error / missing**: muted chip with title "Object {id} not found",
  click no-op.
- **Resolved**: icon + `children` (the LLM-provided label, falling
  back to `meta.OrigName` then `meta.ID[:8]`). Click dispatches:
  - markdown/report: call `Bindings.GetObjectText(id)`, parse the
    first heading for a title (reuse the regex in
    `MessageItem.openDocumentAttachment:72`), invoke
    `onExpandReport({title, content})`.
  - image: call `Bindings.GetImageDataURL(id)`, invoke
    `onLightbox(dataURL)`.
  - blob: call `Bindings.ExportObject(id)` (existing save-as flow).

Nested case: clicking an `ObjectLink` from inside `ReportViewer`
calls the *same* `onExpandReport` `App` already wires. The
existing `ReportViewer` is mounted in `App.tsx`'s root tree and
displays whichever `expandedReport` state currently holds — so
the nested click simply replaces the visible report. This is the
deliberate v0.9.0 behaviour; a back-stack belongs in a later UX
ADR if users ask for it.

#### 3.1.5 Migration of the three current `ReactMarkdown` sites

Each site replaces its inline `urlTransform` + `components`
prop with an import:

```diff
- import ReactMarkdown, {defaultUrlTransform} from 'react-markdown'
- function urlTransform(url) { … }
- <ReactMarkdown urlTransform={urlTransform} components={{ img: … }}>
+ import ReactMarkdown from 'react-markdown'
+ import {urlTransform, objectComponents} from './markdown/objectMarkdown'
+ const components = useMemo(
+   () => objectComponents({onLightbox, onExpandReport}),
+   [onLightbox, onExpandReport],
+ )
+ <ReactMarkdown urlTransform={urlTransform} components={components}>
```

Sites touched: `MessageItem.tsx` (chat bubble), `ReportViewer.tsx`
(full-screen overlay), `App.tsx` (cmd-popup). After this change,
adding a future `code` or `details` override means editing
`objectMarkdown.tsx` once.

### 3.2 Backend

#### 3.2.1 New binding: `Bindings.GetObjectMeta(id) ObjectInfo`

`bindings.go` already has `ObjectInfo` (`bindings.go:1327`) and a
list-style `ListObjects` / `GetSessionObjects`. The frontend
currently has no point-lookup; it would have to scan `ListObjects`
for one ID, which is O(N) per markdown link. Add the natural
sibling:

```go
// GetObjectMeta returns one object's metadata, or an error if the
// id is unknown. Used by the frontend ObjectLink component to
// decide how to render an object:ID reference (image inline vs.
// document chip vs. download chip).
func (b *Bindings) GetObjectMeta(id string) (ObjectInfo, error) {
    m, ok := b.objects.Get(id)
    if !ok {
        return ObjectInfo{}, fmt.Errorf("object %s not found", id)
    }
    return ObjectInfo{ /* same field mapping as ListObjects */ }, nil
}
```

No new error path; `b.objects.Get` already exists and is the
canonical lookup. Tests: a happy-path unit test
(`bindings_test.go` already covers `GetObjectText` similarly) and
a not-found case.

#### 3.2.2 Type-aware `resolveObjectRefsForExport`

Current implementation (`bindings.go:1595`) calls `LoadAsDataURL`
for every `(object:ID)` match. Replace with:

```go
func (b *Bindings) resolveObjectRefsForExport(content string) string {
    if b.objects == nil || !strings.Contains(content, "object:") {
        return content
    }
    // Walk forward by index; rewrite only matches whose object is
    // TypeImage (the only type a self-contained .md can inline).
    result := content
    pos := 0
    for {
        idx := strings.Index(result[pos:], "(object:")
        if idx < 0 {
            break
        }
        absIdx := pos + idx
        end := strings.Index(result[absIdx:], ")")
        if end < 0 {
            break
        }
        id := result[absIdx+8 : absIdx+end]
        meta, ok := b.objects.Get(id)
        if !ok {
            // Unknown ID → mark and skip (avoid infinite loop).
            result = result[:absIdx] + "(missing-object:" + result[absIdx+8:]
            pos = absIdx + len("(missing-object:") + len(id) + 1
            continue
        }
        if meta.Type != objstore.TypeImage {
            // Non-image: keep the object: href untouched. The
            // exported file is still re-openable when re-imported
            // into the app; external readers see an unrendered
            // link, which is no worse than a missing internal URL.
            pos = absIdx + end + 1
            continue
        }
        du, err := b.objects.LoadAsDataURL(id)
        if err != nil || du == "" {
            pos = absIdx + end + 1
            continue
        }
        result = result[:absIdx+1] + du + result[absIdx+end:]
        pos = absIdx + 1 + len(du) + 1
    }
    return result
}
```

Key change: the type check via `b.objects.Get(id)` short-circuits
the `LoadAsDataURL` call for non-images. The forward-walking
`pos` cursor replaces the prior "always restart from zero" loop —
necessary because non-images no longer mutate the slice, so we
must advance past them to avoid an infinite loop.

Tests (`bindings_test.go`):
- Image-only content: identical output to v0.8.0 (regression
  guard for the `(object:imageID)` → `(data:image/...)` rewrite).
- Mixed content: image refs rewritten, `[name](object:markdownID)`
  href left intact, `[name](object:reportID)` href left intact.
- Unknown ID: rewritten to `missing-object:` (same as today),
  cursor advances correctly (no infinite loop with multiple
  unknowns in a row).
- Blob ref: href left intact.

### 3.3 Tool descriptor update

`internal/agent/tool_descriptors_analysis.go:88` (create-report
Description), append one sentence to the current text:

> "Reference images with `![alt](object:ID)`. **Reference other
> documents (markdown / reports) with `[title](object:ID)` — they
> render as clickable preview chips that open the linked content
> in the report viewer.**"

No other descriptor needs editing — the three text tools
(`analyze-text` / `grep-text` / `get-text`) already document
`object:<id>` as their ID parameter (descriptors at
`tool_descriptors_analysis.go:119,148,185`), and that parameter
form is unrelated to the chat-rendering markdown form.

### 3.4 System prompt update

`internal/agent/agent.go:2242-2246`, extend the "To reference
objects" block. Current text:

```
To reference objects from the session:
1. For images attached in the current message: read the anchor immediately preceding each image
2. For other objects (older images, reports, files): use the list-objects tool to discover available objects, then get-object to retrieve them
3. In reports, reference images with: ![description](object:ID)
Never fabricate image URLs or object IDs.
```

Replace item 3 with two items:

```
3. In reports, reference images with: ![description](object:ID)
4. In reports, reference other documents (markdown / reports) with: [title](object:ID) — the renderer turns this into a clickable preview chip that opens the linked content
```

Additionally, at agent.go:2240 the prompt already states that
`Image (object ID: ...):` is an **input** anchor and must never be
emitted in the LLM's own output. Extend that paragraph by one
sentence to cover the document equivalent:

```
The same rule applies to "Document (object ID: ..., name: ..., Nk tokens):" lines that prefix user-attached markdown / text — that anchor is also INPUT-only. To cite a document in your reply or in a report, use the markdown form [title](object:ID); do not paste the anchor line verbatim.
```

Rationale: items 1–2 cover discovery and retrieval; items 3–4
now cover the two inline-reference syntaxes that render
correctly. The expanded anchor-rule sentence closes the
input-vs-output ambiguity for documents the same way the
existing sentence closes it for images. The "never fabricate"
guardrail stays at the bottom unchanged.

No prompt edits elsewhere. The "TypeReport vs TypeMarkdown" block
at agent.go:2248-2253 already explains both types; we don't need
to repeat the reference syntax there.

### 3.5 Document-anchor convention clarification

**Codifies an existing implicit rule rather than changing
behaviour.** The `Document (object ID: …):` anchor (v0.5.0,
ADR-0006) is produced by `PrependDocumentAnchors`
(`internal/llm/backend.go:152-182`) and applied at
`internal/contextbuild/render.go:85` under a single guard:

```go
if r.Role == "user" && len(r.DocumentIDs) > 0 && opts.ObjectLookup != nil {
    content = llm.PrependDocumentAnchors(content, r.DocumentIDs, opts.ObjectLookup)
}
```

This means the anchor fires **only on user-attached documents**
(populated `Record.DocumentIDs`, set when the user drops a `.md`
/ `.txt` into a chat). Reports generated by `toolCreateReport`
do **not** get a document anchor on subsequent turns because:

- The report's full text is already inlined into the records as
  a `Role: "report"` row (`tools.go:374-382 pendingReport`
  → agent loop appends after `AddToolResult`). The LLM sees the
  content directly; there is nothing to "stand in for".
- The anchor is a **surrogate** for content the LLM cannot see
  in the message text (user attachments are not inlined — the
  LLM reads them on demand via `analyze-text` / `grep-text` /
  `get-text`). Reports are not in that category.

The asymmetry between user-attached MD and LLM-generated reports
is therefore **intentional and correct**; the historical confusion
("why don't my own reports show up as anchors?") came from the
absent `[title](object:ID)` rendering this ADR adds. With the
new render path in place, the canonical reference shape for a
prior report from inside a new report is `[title](object:ID)`,
not a fabricated anchor line.

The rules we ratify (and document in
`reference/architecture.md` § "Object reference conventions",
new section added in the same PR):

1. **`Image (object ID: ID):`** — input-only anchor for user-
   attached images. LLM must never emit. To cite an image, use
   `![alt](object:ID)`.
2. **`Document (object ID: ID, name: …, N tokens):`** — input-
   only anchor for user-attached markdown / text. LLM must never
   emit. To cite a document, use `[title](object:ID)`.
3. **`![alt](object:ID)`** — LLM output for inline images.
   Renders as image (or, if the ID resolves to non-image, as a
   document/blob chip — §3.1.2 fallback).
4. **`[title](object:ID)`** — LLM output for inline document /
   report / blob references. Renders as a clickable chip
   (§3.1.3). Acceptable in chat replies as well as in reports;
   the chip behaviour is identical in both contexts.
5. **Reports never gain anchors retroactively.** No code path
   adds `DocumentIDs` to a `Role: "report"` record, and no future
   path should — the surrogate anchor mechanism is for content
   the LLM cannot see, and reports are always visible inline.

If a future use-case surfaces where the LLM wants to ground a
new report in a prior report whose content is **not** in the
current context window (compacted out by a summary), the right
mechanism is **document attachment promotion**: a future tool
that takes an `object:ID` and adds it to the *next* user
message's `DocumentIDs`, so the anchor fires naturally via the
existing render path. That belongs in a follow-up ADR
(probably tied to context-window pressure on long sessions);
not in v0.9.0.

### 3.6 Tool descriptor cross-link audit

The descriptors for the three text tools already say
"`object:<id>` or bare ID" for their `object` parameter
(`tool_descriptors_analysis.go:119,148,185`). No changes needed
there — the **input** parameter syntax is unrelated to the
**output** markdown form this ADR introduces.

One audit item: the `list-objects` descriptor returns IDs in its
tool result, which the LLM may now turn into `[name](object:ID)`
references in subsequent reports. That's the intended new
behaviour (improved provenance UX). The "memory feedback: tool
result internal IDs" caution remains valid for *chat-reply*
contexts (don't volunteer ID links the user didn't ask for); it
does not apply to *inside-report* references, where citing
sources is the whole point. No descriptor edit required — the
distinction is captured by the system-prompt items 3–4 from §3.4.

### 3.7 Behaviour matrix (rendered output)

End state after this ADR. **W** = what the LLM writes; **R** =
what the user sees rendered.

| W                          | Object type   | R (v0.8.0 — current)                         | R (v0.9.0 — after this ADR)                  |
|----------------------------|---------------|----------------------------------------------|----------------------------------------------|
| `![alt](object:ID)`        | image         | inline image + lightbox click ✓              | inline image + lightbox click (unchanged)    |
| `![alt](object:ID)`        | markdown/report | broken-image glyph ✗                       | document chip → opens in ReportViewer        |
| `![alt](object:ID)`        | blob          | broken-image glyph ✗                         | download chip                                |
| `![alt](object:ID)`        | missing       | broken-image glyph ✗                         | "Object not found" badge (existing)          |
| `[title](object:ID)`       | image         | inert link ✗                                 | inline image (the LLM's intent honoured)     |
| `[title](object:ID)`       | markdown/report | inert link ✗                               | document chip → opens in ReportViewer ✓      |
| `[title](object:ID)`       | blob          | inert link ✗                                 | download chip → save-as dialog ✓             |
| `[title](object:ID)`       | missing       | inert link ✗                                 | muted chip "missing object", no-op click     |
| `[![](object:imgID)](object:reportID)` | mixed | image renders, outer link inert ⚠️  | image renders, outer link → ReportViewer ✓   |

Export (`SaveReport` / `ExportObject`):

| W in stored report          | Object type   | Exported `.md` (v0.8.0)                          | Exported `.md` (v0.9.0)                              |
|-----------------------------|---------------|--------------------------------------------------|------------------------------------------------------|
| `![alt](object:ID)`         | image         | `![alt](data:image/...;base64,…)` ✓              | unchanged ✓                                          |
| `[title](object:ID)`        | markdown      | `[title](data:text/markdown;base64,…)` ✗ (broken) | `[title](object:ID)` — left intact                  |
| `[title](object:ID)`        | report        | `[title](data:text/markdown;base64,…)` ✗         | `[title](object:ID)` — left intact                  |
| `[title](object:ID)`        | blob          | `[title](data:application/octet-stream;base64,…)` ✗ | `[title](object:ID)` — left intact               |
| `[title](object:ID)` × N    | mix unknown   | one unknown → infinite loop risk on subsequent matches ⚠️ | forward-walking cursor, no loop risk ✓     |

---

## 4. Edge cases

1. **LLM writes the wrong inline form for the object type.**
   Covered by the rendering fallbacks in §3.1.2 / §3.1.3. The
   renderer normalises intent to object type — no broken-image
   glyphs, no inert links, no special LLM training required.

2. **Meta fetch round-trip latency.** Every `ObjectLink` /
   `ObjectImage` triggers `GetObjectMeta` (and possibly
   `GetObjectText` on click). Wails IPC is in-process; one
   call is a few hundred microseconds at most. The
   `useObjectMeta` cache (§3.1.1) dedupes within a session so a
   long report with 50 references to the same object pays the
   cost once. On session switch, `clearObjectCache` already runs
   (`App.tsx` session-change effect); add a sibling clear for the
   meta cache.

3. **Nested report-from-report.** §3.1.4: clicking a link inside
   `ReportViewer` calls the same `onExpandReport` that the
   outer chat pane wired; `expandedReport` state in `App.tsx` is
   replaced; `ReportViewer` re-renders with new `{title, content}`.
   No back-stack. If the user lands in a deep chain and wants the
   original, they re-open it from the chat. Documented in
   `reference/architecture.md` under "Report navigation".

4. **Cycles.** Report A links to Report B which links to Report A.
   With replace-only navigation (no auto-recursion, click is a
   user action), cycles are not a runtime concern. The export
   resolver never follows non-image references (§3.2.2), so no
   recursion there either.

5. **Object deleted while a report still references it.** The
   chip renders in muted "missing object" state (§3.1.4 error
   branch). The report markdown is unchanged on disk; the reference
   simply doesn't resolve. Consistent with how `ObjectImage`
   already handles missing images.

6. **Cross-session reference.** `Bindings.GetObjectText` /
   `GetImageDataURL` / `GetObjectMeta` all use
   `b.objects.Get(id)`, which is the global lookup —
   `b.objects` is a singleton object repository, not per-session.
   The same visibility rules that govern v0.8.0 (an object exists
   if it was ever stored) apply; this ADR does not narrow or
   widen them. Sessions can already reference objects from other
   sessions today; the new behaviour is symmetric.

7. **Private session interaction.** Private sessions
   (ADR-0010-not-quite — actually privacy controls were v0.3.0)
   already exclude objects they didn't create from any list-objects
   tool result, but the underlying `Bindings.Get*` calls don't
   enforce that filter. The current behaviour for `GetObjectText`
   and `GetImageDataURL` is unchanged by this ADR; if a report
   from one session is opened in another via export/import, and
   it embeds an object ID that the importing session doesn't have,
   the chip shows "missing object" — the existing fallback
   behaviour.

8. **Tool result emission of `object:` IDs.** Memory feedback
   "tool result internal IDs" cautioned against tool results
   exposing IDs that prompt the LLM to write redundant links.
   That guidance stands; this ADR does not change tool result
   shapes. `create-report` already deliberately omits the
   report's own ID from its result (`tools.go:384-389`), which
   remains the right call — the report is shown to the user
   immediately. The new `[title](object:ID)` form is for
   referencing *other* objects from inside the report's content,
   not for the LLM to self-link reports in chat replies.

9. **Markdown table cell containing `[name](object:ID)`.** Same
   behaviour as outside a table — ReactMarkdown applies the same
   `components.a` override to anchor elements inside a
   `<table>`. Verified via remark-gfm's standard pipeline. The
   chip layout uses `display: inline-flex` so the cell flows
   normally.

10. **Code fence containing `[name](object:ID)`.** Code fences
    suppress markdown link parsing; the literal text appears
    verbatim. No special handling needed — this is the desired
    behaviour (the user wanted to *show* the syntax, not invoke it).

11. **LLM accidentally echoes the `Document (object ID: …):`
    anchor in its output.** The system-prompt edit at §3.4 now
    forbids this explicitly, mirroring the long-standing
    prohibition on echoing the `Image (object ID: …):` anchor
    (agent.go:2240). If the LLM still emits it (model drift or
    older context), it renders as literal text — readable but
    cosmetically wrong, no functional break. No runtime guard;
    relying on prompt discipline matches how the image anchor is
    handled.

12. **`DocumentIDs` retroactive backfill for old reports.** Out
    of scope (§3.5 rule 5). No code path will add `DocumentIDs`
    to a `Role: "report"` record. Old reports continue to live
    in records as inlined `role="report"` rows; new
    `[title](object:ID)` references from inside *new* reports
    are how LLMs cite them going forward.

---

## 5. Rejected alternatives

### 5.1 Transclusion directive `{{embed:object:ID}}`

Considered: a new syntax that the report renderer (or
`toolCreateReport` pre-processing) expands inline. Rejected
because:

- The click-to-preview UX (goals 1–3) covers the common case:
  the user wants provenance + jump-to-source, not bulk inline
  expansion. Real reports cite five sources; inline-expanding
  five long documents drowns the report's own narrative.
- Recursion / cycle / size-blow-up handling adds non-trivial
  complexity (`tools.go` would need a depth limit, cycle
  detector, content-budget cap). Premature for an unproven need.
- New syntax is a backward compatibility liability — LLMs
  trained on standard markdown will not naturally produce it,
  and we'd have to teach it in the system prompt (which costs
  prompt tokens forever).
- If a user later needs literal inline expansion, the right
  shape is **report pre-processing**: have `toolCreateReport`
  parse for an opt-in directive and substitute. That work
  belongs in a dedicated ADR with a real use-case driver, not
  here.

### 5.2 Smart "always inline non-image objects" rendering

Considered: when the user sees `[name](object:ID)` for a
markdown object, expand the content inline by default. Rejected:
inline expansion turns every report into a megabyte of content;
the click-to-preview chip preserves the report's visual
structure while still being one click from the source. Same
trade-off as in 5.1.

### 5.3 Resolve `object:` href client-side via redirect

Considered: register a service worker / interceptor that
catches `object:`-href navigations and routes them to a Wails
binding. Rejected: Wails' webview embeds don't expose a clean
protocol-handler hook on macOS, and the implementation would
diverge between dev (vite) and build (packaged) targets. The
component-override approach is the React-native solution; it
needs no protocol plumbing and works identically in dev and
build.

### 5.4 Inline content in export instead of leaving `object:` href

Considered for §3.2.2: when exporting a `.md`, replace
`[name](object:reportID)` with a `<details><summary>name</summary>
…content…</details>` block. Rejected for v0.9.0:

- Mixes presentation (collapsible block) with linkage (URL).
  Some external readers don't render `<details>` at all.
- Recursive expansion (report linking report) requires the same
  cycle / depth handling as 5.1.
- The `object:` href has a clean re-import behaviour: when the
  exported `.md` is imported back into shell-agent (via
  `.shellagent` bundle, ADR-0001) or via a future
  "import markdown" path, the link resolves again.
- A future ADR can add a richer export mode if the demand
  surfaces. Defer.

### 5.5 Single combined component handling both `img` and `a`

Considered: collapse `ObjectImage` and `ObjectLink` into one
`ObjectReference` component that picks the inline rendering
based on type. Rejected: the two components have meaningfully
different render outputs (`<img>` vs. chip), prop shapes
(`alt` vs. `children`), and click handlers (`onLightbox` vs.
`onExpandReport` + `onLightbox`). Sharing one hook
(`useObjectMeta`) is enough; sharing the component would
require an internal switch that obscures both code paths.

---

## 6. Tests / invariants

### 6.1 Frontend (`vitest` if added; otherwise component-level smoke)

- `ObjectLink` renders the expected icon per type (mock
  `GetObjectMeta` returning each of the four `ObjectType`s).
- Click on `TypeMarkdown` calls `onExpandReport` with content
  fetched from `GetObjectText`; first line stripped of `# ` becomes
  the title.
- Click on `TypeImage` calls `onLightbox` with the data URL from
  `GetImageDataURL`.
- Click on `TypeBlob` calls `Bindings.ExportObject(id)`.
- Missing object: chip muted, click is a no-op (no callback fires).
- `objectComponents` factory: `urlTransform` allows `object:`;
  `defaultUrlTransform` strips e.g. `javascript:` (regression for
  the sanitisation behaviour we inherit from ReactMarkdown).

### 6.2 Backend (`bindings_test.go`)

- `GetObjectMeta` happy path: known ID returns populated
  `ObjectInfo` with the right `Type`, `Lines`, `Tokens`.
- `GetObjectMeta` not found: returns `(ObjectInfo{}, error)`.
- `resolveObjectRefsForExport`:
  - Image-only report → all `(object:imgID)` rewritten to
    `(data:image/...;base64,…)`. (Regression for v0.8.0
    behaviour.)
  - Mixed report → image refs rewritten, markdown / report /
    blob href left as `(object:ID)`.
  - Two unknown IDs in a row → both become
    `(missing-object:ID)`; no infinite loop, forward cursor
    advances past each.
  - No `object:` substring → identical output (early return
    guard).

### 6.3 Structural

- `objectMarkdown.tsx` is the only file that imports
  `defaultUrlTransform` from `react-markdown` (a lint or
  grep-based invariant test). If a fourth site springs up and
  duplicates the override, the test catches it before merge.
- `bindings.go` has exactly one `LoadAsDataURL` call inside the
  export resolver (the type-aware switch). Grep-test invariant.

---

## 7. Compatibility

No breaking changes.

- **Existing stored reports** (`TypeReport` content in objstore):
  unchanged on disk. Their `![alt](object:imgID)` refs render
  identically; their `[name](object:ID)` refs (if any) render as
  preview chips instead of inert links — a strict improvement.
- **Exported `.md` files from older versions** that contain
  `[name](data:text/markdown;base64,…)` (the v0.8.0 broken
  output): unaffected by this change (we don't re-process old
  exports). Future exports will be clean.
- **Tool surface**: `create-report` description gains one
  sentence; existing schema, params, and result shape unchanged.
- **System prompt**: gains one bullet about `[title](object:ID)`;
  existing instructions unchanged.
- **`Bindings.GetImageDataURL` / `GetObjectText` /
  `ExportObject`**: unchanged signatures, unchanged semantics.
  Used by both the existing `ObjectImage` /
  `openDocumentAttachment` paths and the new `ObjectLink` path.
- **`Bindings.GetObjectMeta`**: new addition. Wails will
  regenerate the `Bindings.d.ts` typings on next build.

No migration scripts. No schema changes. No bundle format
changes (`.shellagent` already carries the objstore unchanged).

---

## 8. Phasing

Single PR. The pieces are tightly coupled (the rendering changes
need the `GetObjectMeta` binding, and the prompt / descriptor
edits are pointless without the rendering changes). Commits
inside the PR:

1. `feat(objstore): GetObjectMeta binding + tests`
2. `refactor(frontend): centralise object-aware markdown defaults
   in objectMarkdown.tsx; migrate MessageItem / ReportViewer /
   App.tsx cmd-popup to import`
3. `feat(frontend): ObjectLink component + a-component override`
4. `fix(export): type-aware resolveObjectRefsForExport (image
   only) + forward-walking cursor`
5. `feat(agent): create-report descriptor mentions
   [title](object:ID); system prompt gains item 4 and extended
   anchor-rule sentence (covers Document anchor input-only rule)`
6. `docs: ADR-0014 + CHANGELOG entry + INDEX.md / INDEX.ja.md
   update + reference/architecture.md "Object reference
   conventions" section codifying §3.5 rules 1–5`
7. `test: bindings_test for GetObjectMeta + resolveObjectRefs
   matrix; frontend smoke for ObjectLink; grep-invariant test
   that objectMarkdown.tsx is the only defaultUrlTransform
   importer`

`docs-mirror-check.sh` enforces the EN/JA mirror as usual.

---

## 9. Out of scope (follow-ups for later ADRs if needed)

- Transclusion directive `{{embed:object:ID}}` (§5.1).
- Multi-file export bundle with separate `.md` files for each
  referenced document (§5.4).
- Back-stack navigation inside `ReportViewer` for nested chains
  (§4.3).
- Per-type visibility narrowing for private sessions (§4.7).
- Renderer support for `<details>` / `<sub>` etc. — separate from
  object-link rendering; current "no raw HTML" rule stands.
