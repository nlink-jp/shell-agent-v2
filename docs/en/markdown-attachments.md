# Markdown attachments (v0.5)

## 1. Motivation

The agent's analysis surface is currently bounded by what
`load-data` can ingest: CSV / JSON / JSONL. Any text-shaped
content that's not row-tabular has no first-class home —
users either paste it inline (truncated by the context
budget), drop it into the sandbox manually, or pre-convert it
into a CSV that defeats its semantic structure.

This release introduces **Markdown attachments** as a
first-class object type, plus three tools that operate on
them directly: `analyze-text`, `grep-text`, `get-text`. The
goal is to make documents (specs, audit reports, long-form
prose) navigable by the agent without forcing them through
the row-tabular pipeline.

PDF / DOCX / other binary formats are explicitly out of scope
for v0.5 — they require an external converter contract that
we want to design separately. v0.5 ships pure Markdown
support.

### 1.1. Use cases

1. **Single-document Q&A** — user attaches a Markdown
   document, asks questions, agent reads via `get-text` or
   summarises via `analyze-text`.
2. **Untable-able JSON as text** — partial support: user
   wraps non-tabular JSON in a fenced code block inside a
   `.md` file. Full JSON-as-text is deferred to a later
   release.
3. **Markdown Q&A** — direct attach, same flow as use case 1.
4. **Multi-document Q&A** — multiple attachments coexist in
   the session; the agent enumerates via `list-objects` and
   reads each via the new tools.

### 1.2. Bonus: existing reports become input data

`create-report` (the agent's report-generation tool) already
stores its output as `TypeReport` in objstore — Markdown
under the hood. The new tools accept `TypeReport` objects
**as well as** the new `TypeMarkdown`, which means analysis
of a previously-generated report (within the same session
or across sessions via `.shellagent` export/import) becomes a
single tool call without a new mechanism.

## 2. Design principles

- **Markdown-only.** No automatic conversion in v0.5. PDF
  pending for a later release. Avoids the security / resource
  / UX complexity of in-process subprocess invocation.
- **Determinism (case X).** Attachments are *not* injected
  into the system prompt. The LLM discovers attachments via
  the existing `list-objects` tool and reads them via the new
  tools. Reason: as Memory / Findings grow, eager injection
  would shrink the residual context budget non-deterministic-
  ally, causing the same attachment to be analysed differently
  in different sessions or at different points in the same
  session. Tool-fetch keeps behaviour reproducible.
- **ID unification.** Every storage object — image, blob,
  report, attached markdown — is addressed by the existing
  32-hex (or legacy 12-hex) `object:<id>` convention. No
  filename-as-handle. The LLM already speaks this language
  for images and reports; text attachments slot in
  seamlessly.
- **Minimal objstore extension.** One new `ObjectType`
  constant (`TypeMarkdown`), two new optional `ObjectMeta`
  fields (`Lines`, `Tokens`). Everything else (storage layer,
  session lifecycle, export/import, ID rewriting) is reused
  unchanged.

## 3. Rejected alternatives

### 3.1. Eager injection into system prompt (case Y)

System prompt includes attachment content up to a token
budget; large attachments hinted at with metadata only.

**Rejected** because Memory and Findings accumulate over a
session, shrinking the residual context. The same attachment
would inline-fit early in a session and overflow later, so
the same analysis prompt produces different windows of
context over time. Reproducibility is more important than the
1-turn ergonomics gained for small attachments.

### 3.2. Automatic line-table load on attach (case Z)

Attach immediately materialises the content into a DuckDB
`(line_number, text)` table.

**Rejected** because it conflates two distinct storage homes
— objstore for content, DuckDB for analysis projections —
into a single user gesture. Earlier iterations of this design
all converged on the same friction: users want one mental
model for "attach this", not "attach AND project to DB". If
the LLM later needs SQL over the lines, it can do it via the
sandbox (`awk '{print NR","$0}' | load-data`).

### 3.3. Filename-based tool handle

Tools take `analyze-text("audit.md", ...)` instead of
`analyze-text("object:abc123", ...)`.

**Rejected** because chat messages, reports, and image
references all use `object:<id>` as the canonical reference
form. A filename-based handle would create a parallel
taxonomy that the LLM would have to translate between,
introducing failure modes the existing convention doesn't
have (collision, encoding, ambiguity across sessions).

### 3.4. In-process PDF / DOCX converters

Bundle a PDF parser (Go library) or shell out to pandoc / etc.

**Rejected for v0.5** — would require subprocess execution
contract, security review, resource limits, error-handling
UX, and converter selection. Each is a non-trivial design
decision. Deferred to a focused release rather than bolted
onto this one.

### 3.5. `load-text-data` tool for explicit DB projection

A new tool to project a Markdown attachment into a DuckDB
line-table for SQL analysis.

**Rejected for v0.5** because (a) `analyze-text` and
`grep-text` cover summarisation and search natively, (b)
when SQL over lines is genuinely needed, the agent + sandbox
combination handles it via 1 shell command + existing
`load-data`, (c) adding the tool now would expand the
v0.5 tool surface without a use case that explicitly
demands it.

## 4. Storage model

### 4.1. ObjectMeta extension

```go
type ObjectMeta struct {
    ID        string     // unchanged: 32-hex (legacy: 12-hex)
    Type      ObjectType // unchanged + new TypeMarkdown
    MimeType  string     // unchanged
    OrigName  string     // unchanged (display label only)
    CreatedAt time.Time  // unchanged
    SessionID string     // unchanged
    Size      int64      // unchanged
    Lines     int        `json:"lines,omitempty"`  // NEW: for text/markdown types
    Tokens    int        `json:"tokens,omitempty"` // NEW: memory.EstimateTokens cache
}

const (
    TypeImage    ObjectType = "image"     // unchanged
    TypeBlob     ObjectType = "blob"      // unchanged
    TypeReport   ObjectType = "report"    // unchanged
    TypeMarkdown ObjectType = "markdown"  // NEW
)
```

### 4.2. Attach pipeline

1. User selects file (`.md` or `.txt`) via attach button /
   drag-drop / paste; frontend reads via FileReader as a
   data URL with the correct MIME (`text/markdown` or
   `text/plain`).
2. `bindings.SaveImage` is renamed (or shadowed) by a more
   accurate `bindings.SaveAttachment` that accepts any
   supported attachment MIME. Image-only flow stays for
   backward compat with frontend code paths that send only
   image MIMEs.
3. `objstore.SaveDataURL` extends its type inference:
   ```
   image/*           → TypeImage     (unchanged)
   text/markdown     → TypeMarkdown  (NEW)
   text/plain        → TypeMarkdown  (NEW; treated as markdown)
   application/json  → currently TypeBlob, no change
                       (deferred; v0.5.x or v0.6)
   else              → TypeBlob      (unchanged)
   ```
4. `objstore.SaveDataURL` delegates to `Store()` as today.
5. The resulting `ObjectMeta` is stored in objstore via
   the existing `Store()` path. The `OrigName` carries the
   user's filename for display (the LLM-facing handle stays
   the ID). `Lines` and `Tokens` are computed inside
   `Store()` itself (see §4.5) so the attach-side code does
   not duplicate that logic.

### 4.3. Size cap

50 MB hard limit at attach time (rejected before reading the
file content) — markdown documents rarely approach this, and
beyond it the data-URL round-trip and EstimateTokens scan
get noticeably expensive. Larger content should go via
sandbox + register-object after the user manually copies the
file into `/work`.

### 4.4. `Lines` / `Tokens` auto-fill inside `Store()`

The audit surfaced an asymmetry risk: `create-report`
(`agent/tools.go:596`) already writes its output as
`TypeReport` via `objstore.Store(reader, TypeReport,
"text/markdown", ...)`. If only the new `TypeMarkdown` path
computed `Lines` / `Tokens`, reports would render in
`list-objects` without those columns even though they hold
the same kind of content. The LLM would see asymmetric
metadata and behave inconsistently — and the `get-text` flow
on a report would lose its line-count anchor, regressing the
"don't make the LLM call `wc -l` in the sandbox" property
that motivated the field in the first place.

Fix: `Store()` itself auto-computes `Lines` and `Tokens`
when `mimeType` begins with `text/`. The logic is:

```go
// In Store(), after the io.Reader is fully consumed into
// a byte buffer (which it already is, for the write path):
if strings.HasPrefix(mimeType, "text/") && len(content) > 0 {
    meta.Lines = bytes.Count(content, []byte{'\n'}) + 1
    meta.Tokens = memory.EstimateTokens(string(content))
}
```

Effects for **new** writes:
- New `TypeMarkdown` attachments → `Lines` / `Tokens`
  populated ✓
- New `TypeReport` (via `create-report`) → `Lines` /
  `Tokens` populated ✓ (no change to `toolCreateReport`
  needed — the MIME-based dispatch picks it up
  automatically)

For **legacy** objects (pre-v0.5 `TypeReport` rows stored
before this release), `objstore.Load()` is extended with a
**lazy backfill**:

```go
// Sketch inside Load(), after the existing index.json read:
for _, meta := range s.index {
    if meta.Lines > 0 { continue }           // already populated
    if !isTextMIME(meta.MimeType) { continue } // image/blob skipped
    content, err := os.ReadFile(s.DataPath(meta.ID))
    if err != nil { continue }                // missing data file: tolerate
    meta.Lines  = bytes.Count(content, []byte{'\n'}) + 1
    meta.Tokens = memory.EstimateTokens(string(content))
    s.dirty = true
}
if s.dirty { _ = s.save() }  // persist filled metadata
```

Behaviour:
- First launch with v0.5: any legacy `TypeReport` (and any
  hypothetical `TypeBlob` with a `text/*` MIME) gets its
  `Lines` / `Tokens` filled on `objstore.Load()`. The
  updated index is saved back so the cost is paid exactly
  once.
- Subsequent launches: `meta.Lines > 0` short-circuits the
  loop body, so the backfill is effectively free.
- Read failures (data file deleted out-of-band, etc.) are
  tolerated — the entry stays at `Lines = 0`, the rest of
  the app keeps working.
- New v0.5 installs never hit the backfill because all new
  writes already go through the `Store()`-side auto-fill.

This makes the system self-healing on upgrade: no migration
UI, no user action, no permanent asymmetry. The `list-
objects` output is uniform across legacy and new objects
after the first run that exercises a session containing
those legacy reports.

### 4.5. Session lifecycle reuse

- **Export (`.shellagent` bundle, v0.4.0)**: includes any
  object with `SessionID == this session`. `TypeMarkdown`
  inherits automatically.
- **Import**: ID regeneration sweeps via existing
  `sessionio/rewriter.go` regex
  `\b(object:)?([a-f0-9]{32}|[a-f0-9]{12})\b` and rewrites
  references in `chat.json` / `summaries.json`. No new code.
- **Delete (v0.4.2)**: `a.objects.DeleteBySession` already
  removes all session-tagged objects regardless of type.
- **Rename (v0.4.5)**: rename only touches `a.session.Title`;
  attachments unaffected.
- **Private session (v0.3.0)**: privacy flag governs Memory
  promotion. Attachments are per-session by construction;
  no cross-session leakage path.

## 5. Tool specifications

All three tools accept an `object` argument whose value is
either `object:<id>` (preferred) or the bare `<id>`. The
object's `Type` must be `TypeMarkdown` or `TypeReport`.
Other types return an explicit error so the LLM can pick a
different approach (e.g., images go through vision; CSV
goes through `query-sql`).

### 5.1. `analyze-text`

```
analyze-text(object: string,
             perspective: string,
             lines?: string)  // e.g., "1000-5000"
```

Runs a sliding-window summarisation + finding extraction over
the (entire or range-restricted) document. Reuses the
existing `internal/analysis/summarizer.go` machinery with a
**text chunker** upstream (see §6). Findings are auto-
promoted to the session's findings store with
`source = SourceAnalyzeText` (NEW constant) so the Findings
panel can filter by origin.

Returns: markdown report similar to `analyze-data` (running
summary + grouped findings), capped at 10,000 chars with
truncation indicator (matching the existing v0.2.0
behaviour).

### 5.2. `grep-text`

```
grep-text(object: string,
          pattern: string,        // RE2 regex
          lines?: string,         // restrict search range
          max_matches?: int = 200,
          context_lines?: int = 2)
```

Returns matches as `<line_number>: <line content>` plus the
specified number of `-A` / `-B` context lines around each
match. If raw match count exceeds `max_matches`, returns a
structured error suggesting the LLM narrow the search
(`Too many matches (3,847 > 200). Narrow the pattern, or
restrict via the lines argument.`).

`pattern` is RE2 (Go's regexp); no PCRE features.

### 5.3. `get-text`

```
get-text(object: string,
         lines: string)  // required; e.g., "542-560"
```

Returns the specified line range verbatim, with each line
prefixed by its line number for unambiguous citation
(`542: ...\n543: ...\n`). Hard caps at 1,000 lines per
call (a larger range returns an error suggesting
`analyze-text` or multiple `get-text` calls).

### 5.4. Sandbox interop

Verified during the v0.5 audit: the existing
`sandbox-copy-object` tool (`sandbox_tools.go:60`) already
copies any object type from objstore to `/work`. Its
description is updated to mention markdown, but no new tool
is needed. The reverse direction (`/work` → objstore) is
handled by the existing `sandbox-register-object` /
`register-object` tools, whose `type` enums grow a
`"markdown"` entry so the LLM can correctly tag a `/work`
file when it has produced one.

## 6. Chunker for `analyze-text`

The summarizer in `internal/analysis/summarizer.go` consumes
`rows []string` and walks them with a configurable window
size. For markdown, the chunker produces `[]string` where
each element is one chunk of the document.

### 6.1. Chunking strategy

1. **Target chunk size**: ~2,000 tokens (estimated via
   `memory.EstimateTokens`), with 10% overlap. Configurable
   per call via an internal struct (not exposed in tool args
   for MVP).
2. **Line-atomic**: a chunk boundary never falls in the
   middle of a line.
3. **Heading-aware (markdown-specific)**: when a chunk
   boundary would fall within ~10% of a markdown heading
   (`#`, `##`, `###`), snap the boundary to the heading so
   each chunk starts on a section break when possible. Best-
   effort — degenerate inputs (no headings) fall back to
   pure line-atomic.
4. **`max_line_width` backstop**: a single line longer than
   10,000 chars (rare; minified JSON in a code fence,
   base64-encoded blobs, etc.) is force-broken at the
   width limit so it doesn't exceed a single chunk's
   token budget by itself.
5. **Cap on total chunks**: 1,000 chunks per analysis run.
   Above that, return an error suggesting the LLM use
   `lines: ...` to scope the analysis.

### 6.2. Window handoff

The existing `summarizer.Analyze` already maintains a
running summary string + accumulated findings list across
windows. No change to that logic. The chunker just feeds it
text chunks instead of SQL row-stringified rows.

## 7. LLM-facing surface

### 7.1. Discovery

The existing `list-objects` tool's output is extended:

```
- ID: a1b2c3d4e5f6 | Type: markdown | Name: audit.md
    | Size: 1234567 bytes | Lines: 45231 | Tokens: 312000
    | Created: 2026-05-11 10:30:00
```

`Lines` and `Tokens` columns appear for `markdown` and
`report` types; absent for `image` and `blob`. The
description for `list-objects` is updated to mention
markdown attachments.

### 7.2. Just-attached anchor

Mirroring the existing `Image (object ID: xxx):` convention
generated in `llm/backend.go:imageIDPrefix`, the chat
message preamble for a freshly-attached markdown gets a line:

```
Document (object ID: a1b2c3d4e5f6, name: audit.md, 312k tokens):
```

This line appears in the user-message turn that included the
attachment, giving the LLM in-context awareness of recent
attaches without requiring a `list-objects` round trip.

### 7.3. System prompt updates

The three new tools get descriptions in the tool definitions
themselves. No new "attached documents" section is injected
into the prompt body (case X — system prompt stays invariant
across session growth).

A two-paragraph block is added inside the existing object-
handling guidance so the LLM understands the provenance
distinction between the two markdown-bearing types and which
tools apply:

> Markdown content lives in the object store as two distinct
> types with different provenance:
>
> - **TypeReport** — markdown you (the agent) generated
>   previously via the `create-report` tool. These are your
>   own prior conclusions.
> - **TypeMarkdown** — markdown the user attached. These are
>   user-supplied source material.
>
> The three text tools (`analyze-text`, `grep-text`,
> `get-text`) operate on both types interchangeably; each
> takes an object ID. Use `list-objects` to enumerate, then:
> `analyze-text` for sliding-window summarisation of long
> content, `grep-text` for regex search, `get-text` for
> verbatim reading of a specific line range. Use
> `sandbox-copy-object` to expose either type to the sandbox
> `/work` directory when shell tools are needed.

The provenance distinction is deliberately surfaced so the
LLM can calibrate citations and trust appropriately — a
finding repeated from its own prior report carries less
incremental information than the same finding emerging from
user-supplied source material.

### 7.4. Tool filtering

The three new tools are **always visible** (no dynamic-on-
data-load gating). Consistent with `analyze-data` /
`query-sql` (always shown post-v0.1.20). The LLM judges
applicability from `list-objects` output.

## 8. Frontend / UI

### 8.1. Attach button extension

Existing attach button's MIME filter extended in
`frontend/src/chatpane/ChatInput.tsx` from `image/*` to
include `text/markdown` and `text/plain`. Drag-drop and
paste handlers gain the same filter widening.

### 8.2. Data disclosure panel + preview dispatch

`frontend/src/DataDisclosure.tsx` already renders
objects-by-type. `TypeMarkdown` gets a 📝 icon card (distinct
from the 📄 used for `TypeReport` so the user can tell at a
glance whether an artefact was attached or agent-generated).

The preview dispatch in `App.tsx` (around line 786) is
extended so `TypeMarkdown` flows through **the same branch as
`TypeReport`** — both load the object's text via
`GetObjectText`, derive a title from the first markdown
heading, and open `ReportViewer`:

```tsx
} else if (obj.type === 'report' || obj.type === 'markdown') {
    const text = await window.go.main.Bindings.GetObjectText(obj.id)
    const title = (text.split('\n')[0] || '').replace(/^#\s*/, '')
                  || obj.orig_name || obj.id
    setExpandedReport({title, content: text})
}
```

This deliberately treats user-attached Markdown and agent-
generated reports as interchangeable viewing targets — they
are both Markdown strings in objstore, and the existing
ReportViewer is the right component for both. Delete /
select-all / export flows pick up the new type automatically
via the same dispatch widening.

### 8.3. Size warning

When a user attempts to attach a markdown file > 50 MB the
frontend shows a clear error before the data-URL round-trip
("File too large; copy to sandbox /work and use
register-object instead").

## 9. Test plan

### 9.1. Unit (`internal/objstore/`)

- `TestObjectMeta_Migration_LegacyIndexLoadsWithoutLinesTokens`
  — load an old index.json (no Lines/Tokens) and verify
  fields default to 0 *before* backfill kicks in.
- `TestObjstoreLoad_BackfillsLegacyTextObjectMetadata` —
  hand-craft an index.json with a `TypeReport` entry whose
  `Lines=0`/`Tokens=0` plus a real data file containing
  known markdown; call `Load()`; assert (a) the in-memory
  meta has `Lines = expected` and `Tokens =
  memory.EstimateTokens(content)`, (b) the index.json on
  disk now contains the filled values (i.e. the dirty index
  was re-saved). Cover both `TypeReport` and a hypothetical
  legacy `text/plain` `TypeBlob` to verify the MIME-based
  predicate.
- `TestObjstoreLoad_DoesNotBackfillImagesOrBinaries` —
  add an image and a non-text blob with `Lines=0`; assert
  they stay untouched through Load.
- `TestObjstoreLoad_ToleratesMissingDataFile` — index
  entry references an ID whose data file was deleted; Load
  skips it without erroring; entry stays at `Lines=0`.
- `TestSaveDataURL_Markdown_PopulatesLinesAndTokens` — feed
  a known markdown body; assert Lines = expected, Tokens =
  `memory.EstimateTokens(body)`.
- `TestStore_TypeReportGetsLinesAndTokens` — call
  `Store(reader, TypeReport, "text/markdown", ...)` (mimics
  `toolCreateReport`); assert Lines/Tokens populated.
- `TestSaveDataURL_TextPlain_TreatedAsMarkdown` —
  `text/plain` MIME stores as `TypeMarkdown`.
- `TestSaveDataURL_OversizedRefused` — > 50 MB rejected.

### 9.2. Unit (`internal/analysis/`)

- `TestTextChunker_TokenBudget` — chunks stay under target.
- `TestTextChunker_LineAtomic` — no chunk splits mid-line.
- `TestTextChunker_HeadingAware` — boundary snaps to
  heading when within tolerance.
- `TestTextChunker_LongLineBackstop` — pathological single-
  line input gets force-broken at `max_line_width`.
- `TestTextChunker_RespectsTotalChunkCap` — > 1,000 chunks
  → error with hint.

### 9.3. Integration (`internal/agent/`)

- `TestAnalyzeText_Roundtrip` — attach a small markdown,
  run analyze-text, verify result has running summary +
  findings tagged with `SourceAnalyzeText`.
- `TestGrepText_HitsAndContext` — search for known
  patterns; verify match formatting + context lines.
- `TestGrepText_TooManyMatches` — exceed max_matches; verify
  error wording suggests narrowing.
- `TestGetText_RangeRead` — read a specific range; verify
  line-number prefix.
- `TestGetText_RangeTooLarge` — request > 1,000 lines;
  verify error.
- `TestTextTools_RejectWrongType` — call analyze-text on a
  `TypeImage` object; verify type-mismatch error.

### 9.4. Lifecycle

- `TestSession_ExportImport_TextAttachment_IDsRewritten`
  — export a session with a markdown attachment, import
  into a fresh data dir, verify the new session's
  `chat.json` references the regenerated ID.

### 9.5. Frontend

- Type-check only (`npm run build`). Manual smoke:
  drag-drop markdown, paste markdown text (becomes attached),
  click data-panel card → ReportViewer opens.

## 10. Compatibility

- **Public API**: purely additive — one `ObjectType`
  constant, two optional `ObjectMeta` fields, one new
  Findings source constant, three new agent tools, three
  unchanged-but-extended tools (`list-objects`,
  `sandbox-copy-object`, `register-object` enum).
- **On-disk format**: backward compatible. Old `index.json`
  entries load with `Lines = 0`, `Tokens = 0`, no migration
  step.
- **Session bundles (`.shellagent`)**: existing v0.4.0
  format already includes typed objects; the new
  `TypeMarkdown` flows through unchanged. Import
  ID-rewrite regex already covers the relevant references.
- **`.shellagent` bundles from < v0.5**: load fine. They
  simply have no markdown attachments.

## 11. Out of scope (v0.5)

- **PDF / DOCX / other binary formats** — deferred. Requires
  external converter contract design (subprocess execution,
  security, timeout, resource limits, error UX, converter
  selection). Worth its own design pass.
- **JSON-as-text** — partial support only (user wraps in
  code fence inside `.md`). A dedicated `text/json` →
  `TypeMarkdown` mapping with pretty-printing pre-store is
  the obvious follow-on once we have field evidence.
- **`load-text-data` (line-table projection)** — agent +
  sandbox already covers the rare case; not adding a
  dedicated tool until use cases emerge.
- **Per-attachment auto-summary** — generating a one-line
  description at attach time via a quick LLM call. Costs
  one extra LLM round trip per attach with marginal gain;
  the user can ask the agent to summarise on demand via
  `analyze-text`.
- **Streaming attach (file picker for > 50 MB files)** —
  current data-URL round-trip works for everything under
  the cap; larger files take the sandbox detour. A
  `bindings.SaveFile(path)` for native paths is a v0.6
  candidate.
- **Configurable chunker parameters via Settings UI** —
  chunker defaults are pinned in code (2k tokens, 10%
  overlap, 10k char max line, 1k chunk cap). Surface
  later if field experience demands tuning.

## 12. Open questions resolved during design

| ID | Question | Resolution |
|----|----------|------------|
| D1 | `analyze-text` Findings source | New `SourceAnalyzeText` constant — distinct from `SourceAnalyzeData` so Findings panel can filter by origin. |
| D5 | Dynamic tool filtering for text tools | Always visible. Matches `analyze-data` / `query-sql` post-v0.1.20 behaviour. |
| D7 | System prompt updates | One sentence inside existing object-handling guidance; no new section. |

## 13. Risks

- **Local LLM tool-call reliability**: weaker local backends
  may not call `list-objects` consistently when an attachment
  was added several turns earlier. Mitigation: the just-
  attached anchor line in the user turn provides immediate
  in-context awareness; older attachments rely on the LLM
  remembering to enumerate. Field testing required.
- **EstimateTokens drift vs. backend tokenizer**: the cached
  `Tokens` value uses the local CJK-aware estimator and may
  diverge from Vertex's actual tokenizer by 10-30%.
  Acceptable for the LLM's read/skip decision; not used for
  context budget enforcement.
- **`text/plain` over-claiming as markdown**: a `.txt` file
  with no markdown structure renders fine through markdown
  renderers (no semantic loss) but the LLM might describe it
  as "markdown" in responses. Cosmetic — no functional
  failure.
