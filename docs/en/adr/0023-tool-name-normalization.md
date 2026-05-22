# ADR-0023: Canonical `snake_case` tool names + boundary normalization

- Status: Proposed
- Deciders: magi
- Related: Issue #12 (the trigger), ADR-0007 (tool registry refactor)

## 1. Context

[Issue #12](https://github.com/nlink-jp/shell-agent-v2/issues/12)
reports that Gemma served via Ollama cannot emit hyphenated tool
names. Gemma's official function-calling format is a Python
`tool_code` block:

````
```tool_code
my_function(arg="x")
```
````

Python identifiers do not allow `-` (`my-function(args)` parses as
the expression `my - function(args)`), so a model constrained to
emit valid `tool_code` cannot produce a hyphenated identifier no
matter what we tell it in the schema or prompt. The constraint is
structural, not soft. Google's Gemma function-calling docs
explicitly recommend `snake_case` or `camelCase` and call hyphens
out as unsupported.

LM Studio masks this by wrapping Gemma in an OpenAI-compatible
JSON tool-call shim with constrained decoding. Ollama runs
Gemma's native template, so the constraint surfaces.

The dispatcher in `executeTool()` does exact-string lookup against
the registry (`app/internal/agent/agent.go:2266` →
`dispatchDescriptor` → `toolDescriptorByName`). When Gemma emits
`list_objects` against a registry keyed on `list-objects`, the
lookup returns `(zero, false)` and we fall through to the
"unknown tool" error.

### 1.1. Four tool sources, four levels of control

| Source | Where the name comes from | Under our control? |
|---|---|---|
| Descriptors (28) | Hard-coded in `app/internal/agent/tool_descriptors_*.go` | **Fully** — we wrote them, we ship them |
| Bundled shell scripts (6) | `# @tool:` header in `app/internal/bundled/tools/*.sh` (5 of 6 currently kebab) | **Fully** — we wrote them, we embed and scaffold them |
| User-authored shell scripts | `# @tool:` header in arbitrary user files under `~/.../tools/` | **No** — user-controlled |
| MCP tool registry | Upstream MCP server's tool name, prefixed `mcp__<guardian>__<tool>` | **No** — external server-controlled |

Our previous default was "do nothing about naming; emit verbatim
to the LLM; dispatch by exact match." That made the first two
rows the same as the last two — even though we own them. The
result is that things we control violate the constraint of a
backend we want to support, propped up by no mechanism at all.

### 1.2. Other backends have a soft version of the same bias

Qwen / Llama / Mistral served via Ollama are not strictly bound
to Python syntax, but they are heavily trained on `snake_case`
tool names and exhibit the same `-` → `_` substitution as a soft
bias when grammar enforcement is off. LM Studio and Vertex AI /
Gemini (JSON tool-call format) pass names through verbatim and
are unaffected today, but become affected the moment a different
local backend is added.

## 2. Decision

A two-part change, applied together in a single release. Each
part addresses a different row in §1.1.

**Part A — Canonical form at source for what we control.**
Rename every tool name we own from kebab-case to `snake_case`:

- All 28 descriptors in `tool_descriptors_{builtin,analysis,sandbox}.go`
- The `# @tool:` header lines of the 5 kebab-named bundled
  shell scripts under `app/internal/bundled/tools/`:
  `file-info.sh`, `get-location.sh`, `list-files.sh`,
  `preview-file.sh`, `write-note.sh`. The script filenames stay
  unchanged — `ScanDir` (`internal/toolcall/toolcall.go:90`)
  keys the registry on the header value, not the filename, so a
  filename rename adds churn without affecting dispatch. Keeping
  filenames also avoids creating a discrepancy with already-
  scaffolded user data dirs (those still hold `file-info.sh`;
  new installs will too, just with a `file_info` header).

This is the principled fix: source spells things in the form the
LLM actually receives. No "the schema says X but Gemma emits Y"
cognitive dissonance.

**Part B — Boundary normalization for what we don't control.**
Add a single canonical function and apply it at the registry
boundaries:

```go
// app/internal/agent/tool_name.go (new file)
func canonicalToolName(name string) string {
    return strings.ReplaceAll(name, "-", "_")
}
```

Applied at the 5 boundary points in §3. This guarantees three
things that Part A alone does not:

1. **History compatibility** — sessions persisted under the old
   names (`tc.Name = "list-objects"`) continue to dispatch
   correctly after the rename, without a migration step.
2. **User shell tool tolerance** — a user who writes
   `# @tool: foo-bar` is not punished with an "unknown tool"
   error on Gemma; the script is dispatched as `foo_bar`
   internally.
3. **MCP tolerance** — an upstream MCP server that publishes
   `foo-bar` works through Gemma without us asking the server
   to change.

Part A is correctness at source; Part B is defense at the
boundary. Both are needed.

## 3. Boundary points (the 5 sites for Part B)

| # | Site | File:line | Direction | Reasoning |
|---|---|---|---|---|
| 1 | `rebuildToolDescriptorIndex` | `tool_descriptor.go:125` | key on canonical | Belt-and-suspenders after Part A: even if a descriptor source slips back to kebab, the index stays correct |
| 2 | `toolDescriptorByName` | `tool_descriptor.go:112` | normalize query | Any caller passing a raw name still hits the right entry |
| 3 | `executeTool` | `agent.go:2257` | normalize `tc.Name` once on entry | Symmetric with the existing `tc.Arguments = normalizeToolArgs(...)` at `agent.go:2258`. Covers session-history replay, user shell scripts, and MCP returns in one place |
| 4 | `buildToolDefs` | `agent.go:2351` (+ descriptor, shell, MCP sub-paths) | emit canonical names to LLM | User shell tools and MCP tools whose authors used kebab are emitted as snake to the LLM |
| 5 | `ListTools` (UI binding) | `agent.go:1373` area | emit canonical names to UI | Settings → Tools shows the same name the LLM sees — never `foo-bar` in UI while `foo_bar` is on the wire |

After Part A, sites #1–#2 are mostly redundant for descriptors
(the source is already canonical) — they earn their keep for
user shell scripts and MCP tools, and as a defense-in-depth layer
against a future descriptor accidentally being added with a `-`.

### 3.1. MCP separator is unaffected

MCP tool names reach the LLM as `mcp__<guardian>__<tool>`. The
`__` separator survives `strings.ReplaceAll(_, "-", "_")`
unchanged — only `-` characters are replaced. Therefore
`splitMCPName` (`agent_mcp.go`) needs no change to its parsing
logic; the canonicalization happens to the `<guardian>` and
`<tool>` segments individually before they are joined.

## 4. What does NOT change

- **User-authored shell scripts.** A user with
  `# @tool: foo-bar` keeps their script working — dispatched as
  `foo_bar` via Part B.
- **Bundled scripts already scaffolded into user data dirs.**
  `Install()` (`internal/bundled/bundled.go`) is one-shot: it
  does not overwrite existing files. Users who installed an
  earlier version still have `file-info.sh` with
  `# @tool: file-info` in their `~/.../tools/` dir; Part B keeps
  these working. New installs get the canonical names from day
  one.
- **Upstream MCP servers.** A server publishing `foo-bar`
  continues to work via Part B.
- **Session history on disk.** Old records with
  `tc.Name = "list-objects"` continue to dispatch correctly;
  Part B's site #3 normalizes on the way in. No data migration.
- **MITL records, deferred-extraction queue, export/import
  JSON.** Read paths flow through dispatch / display sites that
  inherit the normalization.

## 5. Rejected alternatives

For each option considered and rejected, the reasoning is
recorded so a future revisit doesn't re-litigate from scratch.

### 5.1. Boundary normalization only, no rename

The earlier draft of this ADR. Keep descriptor source and
bundled shell scripts in kebab-case, fix everything with the
normalization layer.

Rejected because: the descriptors and bundled scripts are under
our control. Writing them in a form that we *know* a supported
backend cannot emit, and then propping them up with conversion,
is dishonest about what the LLM sees. A future reader looking at
`tool_descriptors_analysis.go` and seeing `Name: "analyze-data"`
has to know that the canonical layer rewrites it to
`analyze_data` before the LLM ever sees it — which is exactly
the kind of indirection that costs comprehension every time
someone touches the file. Better to spell the source the way the
wire works and reserve the normalization layer for inputs we
truly cannot dictate.

### 5.2. Rename only, no normalization layer

The mirror of 5.1: rename all 28 descriptors and 5 bundled
scripts, drop the canonical layer.

Rejected because: this breaks three things on its own.

- **Old session histories** carry `tc.Name = "list-objects"`; a
  registry keyed only on `list_objects` would fail dispatch on
  replay. A one-shot migration is conceivable but error-prone
  (DuckDB column rewrites across all session DBs, plus
  export/import JSON in user backups).
- **Existing scaffolded bundled scripts** in users' data dirs
  retain their kebab `# @tool:` headers (scaffold is one-shot).
  Dispatch by exact match would suddenly stop finding them.
- **User shell scripts** with kebab names break without warning.
- **MCP servers** publishing kebab names continue to fail on
  Gemma.

The normalization layer is what makes the rename safe and
honest about external inputs we cannot rewrite.

### 5.3. Per-backend rewrite

When the active backend is Gemma/Ollama, rewrite tool names to
`snake_case` before sending the schema, and reverse-map on
receive. Localizes the workaround.

Rejected because: adds backend-specific special-casing in two
directions for every affected backend. Today only Ollama+Gemma is
loud; tomorrow it's Ollama+Qwen, then a new local backend. We'd
be writing per-backend codecs forever. Soft-bias backends
(Qwen / Llama) get inconsistent treatment — if we don't add a
codec, they emit `_` against a `-` schema and fail
probabilistically. The canonical-everywhere approach handles all
of these uniformly.

## 6. Implications and risks

### 6.1. Cross-source collision

Today the uniqueness test (`TestToolDescriptors_UniqueNames` in
`tool_descriptor_structural_test.go`) only checks that no two
*descriptors* share the same `Name`. It does **not** check
against shell tools or MCP tools.

After this change:
- Descriptor source is canonical-form already, so descriptor-
  internal collisions are exactly the same set as today.
- A user shell tool `foo-bar` and a descriptor `foo_bar` both
  resolve to `foo_bar` — collision.
- An MCP server publishing `foo-bar` under a guardian that also
  has a tool literally named `foo_bar` would collide within the
  guardian's `mcp__<guardian>__foo_bar` namespace.

Mitigation: extend `TestToolDescriptors_UniqueNames` to assert
canonical uniqueness across the descriptor source. Add a
non-fatal runtime warning at shell-tool registration time when a
shell tool's canonical name collides with an existing descriptor;
descriptor wins. MCP tools are namespaced by `mcp__<guardian>__`
so cross-guardian collision is impossible; within-guardian
collision is a pathological case — log a warning and drop the
later entry.

### 6.2. UI / docs / comments — one-shot search-replace

After the rename, every place that names a tool by string
literal needs to be updated. Surveyed scope:

- 28 descriptor source `Name:` literals
- 5 bundled script `# @tool:` headers (filenames unchanged)
- `app/frontend/src/dialogs/MITLDialog.tsx:70–73` — small
  dispatch-by-name table for special MITL UI argument fields
  (`'sandbox-run-shell': 'command'` etc.)
- User-facing TS copy mentioning tool names by name:
  `FindingsDisclosure.tsx:171`, `ChatInput.tsx:155`,
  `SettingsDialog.tsx:816, 887`
- Comments in Go and TS that mention tool names verbatim
  (mechanical replace)
- Test files in `app/internal/...` referencing tool names by
  string

This is mechanical work, but the diff will be wide. The
canonical layer ensures none of the rename steps individually
breaks anything mid-PR: if a stale reference still passes
`list-objects` somewhere, Part B normalizes it to `list_objects`
and dispatch still succeeds.

### 6.3. Documentation

README.md, README.ja.md, and AGENTS.md mention tool names
verbatim in examples ("the `analyze-data` tool…"). Update to
`analyze_data` style.

### 6.4. Backwards compatibility for sessions and scaffold dirs

See §4. Old session history and already-scaffolded bundled
scripts continue to work via the normalization layer. No data
migration, no scaffold-rewriting step.

## 7. Implementation plan

Single PR for both Parts A and B; together they form one logical
change and shipping them apart would either leave the dishonest-
source state (A missing) or break old sessions / scaffolded
scripts (B missing).

Estimated ~400 LoC including the renames.

1. **Part B foundation first** — `app/internal/agent/tool_name.go`
   with `canonicalToolName`, and the 5 boundary applications in
   `tool_descriptor.go` (×2) and `agent.go` (×3). At this commit,
   all existing tests still pass because canonicalization is a
   no-op for already-canonical inputs and a harmless rewrite for
   the existing kebab ones.
2. **Part A descriptor rename** — 28 `Name:` literals in
   `tool_descriptors_{builtin,analysis,sandbox}.go`. Update any
   in-file comments that reference the old name.
3. **Part A bundled-script header rename** — update the
   `# @tool:` header of 5 bundled scripts to snake_case.
   Filenames stay unchanged: dispatch keys on the header value
   (`ScanDir` at `internal/toolcall/toolcall.go:90`), and the
   already-scaffolded copies in user data dirs are not touched
   either way. `bundled_test.go` (which asserts on filenames)
   needs no change.
4. **Frontend MITL dispatch table** —
   `app/frontend/src/dialogs/MITLDialog.tsx:70–73` keys updated
   to canonical form.
5. **Mechanical cleanup** — comments and user-facing TS copy
   that name tools verbatim, test files referencing names by
   string. Single search-replace pass per file.
6. **Tests**:
   - Strengthen `TestToolDescriptors_UniqueNames` to assert
     canonical uniqueness.
   - New `tool_name_test.go` — table-driven `canonicalToolName`
     test plus a regression test exercising the `list-objects`
     → `list_objects` dispatch round-trip (verifies session-
     history compat).
7. **Docs** — README.md, README.ja.md, AGENTS.md updated to use
   canonical names in examples. AGENTS.md gains a short note
   stating: "Tool names are `snake_case`; the registry normalizes
   `-` to `_` at the boundary so user shell scripts or MCP names
   using hyphens continue to work."
8. **CHANGELOG.md** — new section for the next version, framed as
   `fix(agent): accept snake_case tool calls from Gemma/Ollama (issue #12)`
   with a note that built-in / bundled tools were also renamed.

`go test ./internal/... -tags no_duckdb_arrow` must pass on
each commit.

## 8. Out of scope

- **Rewriting users' already-scaffolded bundled scripts** in
  their data dirs on next launch. The scaffold is one-shot by
  design and we are not introducing a "fix-up old installs"
  step; Part B's normalization covers them.
- **Constrained-decoding support in the local backend.** Would
  fix Gemma at a different layer entirely. Separate concern.
- **Renaming MCP guardian names** (which are constrained to
  kebab-case by AGENTS.md). Guardian names appear inside the
  `mcp__<guardian>__<tool>` envelope; they are themselves never
  the function name the LLM emits, only a routing component.
- **CamelCase tolerance.** If a future backend produces
  `listObjects`, that's a separate normalization decision. This
  ADR commits only to `-` ↔ `_` equivalence.
