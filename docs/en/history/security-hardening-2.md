# Security Hardening Round 2 — Design Document

> Date: 2026-05-02
> Status: Proposed
> Scope: 15 confirmed findings from the 2026-05-02 audit
> (4 Critical, 11 High). Phased into five commits so each can
> be reviewed and reverted independently. Continues the work
> shipped in v0.1.18 (`security-hardening.md`).

## 1. Background

A second-pass security review on 2026-05-02 covered the
agent / tool layer, sandbox / MCP layer, LLM / chat / context
build, storage / analysis, and Wails bindings / frontend. Of
the raw findings, several overlapped with v0.1.18's
hardening (notably the per-request MITL channel and
symlink-aware `safeWorkPath`) and a few were false positives
under closer reading — those are listed in §8.

This document covers only the findings that survived
re-verification against the current v0.1.19 codebase.

## 2. Findings (verified)

### Critical

- **C1 — SQL injection in `refreshTableMeta`.**
  `internal/analysis/engine.go:572-573` builds
  `SELECT comment FROM duckdb_tables() WHERE table_name = '%s'`
  by string concatenation. The other call sites in this file
  use `sanitizeIdentifier`; this one was missed. Table names
  reach this path through `SetTableDescription`, which
  accepts an LLM-supplied name. A malicious table name (e.g.
  `t' OR '1'='1`) leaks metadata or aborts the call.
- **C2 — MCP guardian stderr never drained.**
  `internal/mcp/mcp.go:77-92` wires stdin and stdout pipes
  but leaves `cmd.Stderr` nil. A guardian that writes more
  than the kernel pipe buffer (~64 KB) to stderr blocks on
  write; the parent is meanwhile blocked on `stdout.Scan`,
  producing a deadlock that hangs the entire chat session.
- **C3 — Sandbox stdout/stderr unbounded.**
  `internal/sandbox/cli.go:525-528` captures both streams
  into `bytes.Buffer` with no size cap. An LLM-issued
  `sandbox-run-shell` of e.g. `cat /dev/zero` allocates the
  full output into RAM before the timeout fires, creating an
  OOM / availability hole.
- **C4 — Summary cache write is non-atomic.**
  `internal/contextbuild/cache.go:128` calls `os.WriteFile`
  directly. A crash mid-write or two writers arriving close
  together produces a torn file. Other JSON stores in the
  app already use the data path's natural session-scoped
  isolation; this one does not.

### High

- **H1+H2 (escalated) — Analysis-tool MITL gate is wired
  to the UI but not honoured by the dispatcher.**
  `ListTools()` (`internal/agent/agent.go:710-720`) exposes
  every analysis tool to the Settings → Tools list, where
  the UI renders a per-tool MITL toggle backed by
  `cfg.Tools.MITLOverrides[name]`. The dispatcher's
  switch-case branch for analysis tools
  (`internal/agent/agent.go:1227-1243`) calls
  `executeAnalysisTool` directly and **never consults
  `MITLOverrides`**. The result:

  | Tool | UI MITL toggle behaviour |
  |---|---|
  | `load-data`, `reset-analysis`, `promote-finding`, `describe-data`, `list-tables`, `query-preview`, `suggest-analysis`, `quick-summary`, `create-report` | Toggle ON → still runs without prompting |
  | `query-sql`, `analyze-data` | Toggle OFF → still prompts (hard-coded MITL in `tools.go:230, 251`) |

  The proper API (`IsToolMITLRequired`,
  `internal/agent/agent.go:1377`) does read
  `MITLOverrides`, but is only called for `sandbox-*` and
  `mcp__*` tools (`agent.go:1247, 1257`). The analysis
  branch bypasses it entirely. Effective severity: the user
  has **no working control** over MITL on any analysis tool,
  contrary to what the Settings UI implies. The
  load-data/reset-analysis/promote-finding gaps from the
  initial review (no MITL by default for destructive paths)
  are sub-symptoms of this larger contract violation.
- **H3 — MCP tool-name parsing brittle.**
  `internal/agent/agent.go:1262` does
  `strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)`,
  which mis-splits if a guardian or upstream tool name
  contains `__`. Guardian names are config-controlled (low
  exploitability) but upstream tool names come from the MCP
  server (less control). The mis-split silently routes the
  call to the wrong guardian or returns a misleading "not
  found" error.
- **H4 — MCP response ID not validated.**
  `internal/mcp/mcp.go` `call()` increments a request ID but
  never checks `resp.ID == sentID`. A misbehaving or malicious
  guardian can return responses out of order; the client
  reads whichever line arrives first and routes its body
  back to whichever caller was waiting. Cross-contamination
  between concurrent MCP calls is silent.
- **H5 — Sandbox image is a mutable tag.**
  `internal/sandbox/engine.go:177` defaults to
  `python:3.12-slim`. `ensureImage` pulls on demand. A
  registry compromise (or DNS / MITM during pull) replaces
  the image and the LLM is then handed code execution
  inside it. Users can override but nothing prevents them
  from picking a mutable tag either.
- **H6 — `ToolCall.Arguments` from local backend is unvalidated.**
  `internal/llm/local.go:146-151` stores
  `tc.Function.Arguments` as a raw string without checking
  it is valid JSON, that it matches the tool's parameter
  schema, or that it is bounded in size. Downstream
  `executeAnalysisTool` `json.Unmarshal` errors propagate up
  to the LLM, but a multi-megabyte garbage Arguments field
  blocks the agent loop and pollutes the session record.
- **H9 — Findings ID race.**
  `internal/findings/findings.go:69` builds an ID from
  `len(s.findings)+1` with no mutex. The store is currently
  accessed sequentially through the agent loop, but
  `promote-finding` runs from the dispatcher and the
  post-task WG also touches finding state; any future
  parallelisation produces duplicate IDs and
  `DeleteByIDs` would silently delete the wrong record.
- **H10 — No `fsync` / atomic rename on findings, pinned, objstore index.**
  `findings.go:62`, `pinned.go:61`, `objstore.go:106` use
  `os.WriteFile` then return. An unclean shutdown (force
  quit, panic, OS crash) loses the latest write — losing
  user work the chat just told the model was saved.
- **H11 — 12-char hex object IDs (48-bit entropy).**
  `internal/objstore/objstore.go:285-289`. Birthday-bound
  collision around 16 M objects, ~0.18 % collision risk by
  ~1 M. Realistic only for long-lived heavy users, but
  collision means the second `Store()` silently overwrites
  the first object's index entry (the file write uses
  `O_TRUNC`, so even the bytes are clobbered).
- **H12 — Local backend `Chat()` reads response unbounded.**
  `internal/llm/local.go:371` `io.ReadAll(resp.Body)` with
  no `LimitReader`. The streaming path already caps error
  bodies at 512 B, but the success path is uncapped — a
  rogue or misconfigured local endpoint can OOM the app.
- **H14 — Path expansion follows symlinks.**
  `internal/config/config.go:281-288` expands `~/`, then
  `internal/analysis/engine.go:228-253`'s `validateFilePath`
  checks `os.Stat`, which follows links. A symlink in the
  app's data dir (placed by another process the user runs)
  redirects file operations to host paths the analysis layer
  was meant to refuse.

### Low (carried forward but de-prioritised)

- **L1 — guard.Wrap silent fallback.** Both
  `chat.go:172-174` and `contextbuild/render.go:59-61`
  silently fall back to the unwrapped string if
  `guard.Tag.Wrap` returns an error. `nlk/guard` uses
  `crypto/rand` for the nonce; failure is essentially a
  process-level catastrophe (rand source unavailable). We
  adopt fail-closed semantics for defence in depth.

## 3. Goals / Non-goals

### Goals

1. Eliminate the SQL injection in `refreshTableMeta` (C1).
2. Make MCP guardian I/O deadlock-free (C2) and response
   routing trustworthy (H4).
3. Bound sandbox output (C3) and local-backend response
   bodies (H12) to fixed memory caps.
4. Make every JSON store on the data path crash-safe (C4,
   H10).
5. Close the MITL gap on destructive analysis tools (H1,
   H2).
6. Harden MCP tool-name parsing against `__` collisions
   (H3).
7. Pin sandbox images by digest, surface a UI warning when
   the active image is a mutable tag (H5).
8. Validate `ToolCall.Arguments` from local backends — JSON
   well-formedness and size (H6).
9. Make findings ID generation race-free (H9), expand object
   ID entropy and add collision detection on write (H11).
10. Reject symlinks at the path-expansion / file-open layer
    used by analysis (H14).
11. Switch `guard.Wrap` to fail-closed (L1).

### Non-goals

- **No new sandbox API or new tools.** Same shapes; the fix
  is enforcement-side only.
- **No on-disk format change.** All JSON stores keep their
  schema; we change *how* we write them.
- **No retry-policy refactor.** The retry layer remains as
  shipped in v0.1.18.
- **No new threat model.** Same as security-hardening.md
  §1: untrusted LLM output / tool args / MCP server output;
  trusted host user; user-edited Dockerfile and MCP profile
  paths are user-authority and not validated by us.
- **No re-litigation of v0.1.18 fixes.** Per-request MITL
  channel, symlink-aware `safeWorkPath`, conditional `:Z`,
  startup container sweep, and SaveSettings sandbox restart
  are already in place; this round does not touch them.

## 4. Detailed design

### 4.1 Phase A — DuckDB metadata, MCP wire, sandbox/local I/O caps (C1, C2, C3, H4, H12)

Pure backend, no UI surface.

**4.1.1 C1 — parameterise `refreshTableMeta`**

```go
// engine.go
commentRow := e.db.QueryRow(
    "SELECT comment FROM duckdb_tables() WHERE table_name = $1",
    tableName,
)
```

Adds a regression test in `engine_test.go` that calls
`SetTableDescription` with a name containing `'`,
`%`, NUL byte; expects no error and that the comment
survives a round-trip.

**4.1.2 C2 — drain guardian stderr**

```go
// mcp.go Start()
stderrPipe, err := g.cmd.StderrPipe()
if err != nil {
    return fmt.Errorf("stderr pipe: %w", err)
}
go func() {
    s := bufio.NewScanner(stderrPipe)
    s.Buffer(make([]byte, 0, 64*1024), 256*1024)
    for s.Scan() {
        logger.Debug("mcp[%s] stderr: %s", g.name, s.Text())
    }
}()
```

Drain goroutine exits naturally when the pipe closes on
process exit. No additional shutdown coordination needed.

Test: `mcp_test.go` adds a fake guardian script that emits
1 MB to stderr, then a JSON-RPC response on stdout; assert
the response arrives within timeout (no deadlock).

**4.1.3 C3 — bound sandbox output**

Introduce `internal/sandbox/limitedbuffer.go`:

```go
type limitedBuffer struct {
    buf       bytes.Buffer
    cap       int
    truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
    n := len(p)
    if l.buf.Len() >= l.cap {
        l.truncated = true
        return n, nil // discard, but report success to keep child happy
    }
    remaining := l.cap - l.buf.Len()
    if len(p) > remaining {
        l.buf.Write(p[:remaining])
        l.truncated = true
        return n, nil
    }
    l.buf.Write(p)
    return n, nil
}
```

Cap is `cfg.Sandbox.MaxOutputBytes` (new field, default
8 MB). `Exec` passes `&limitedBuffer{cap: ...}` for both
streams. On truncation, the result string includes a
trailing marker `"\n... [stdout truncated at 8 MB]"` so the
LLM can see what happened.

Tests: `cli_test.go` adds
`TestExec_TruncatesOversizedStdout` driving a Python tool
that prints a large blob.

**4.1.4 H4 — validate MCP response ID**

After `json.Unmarshal` of a response, compare `resp.ID`
against the request ID we sent. On mismatch, return a
transport error. To preserve round-trip semantics under any
future reordering, also reject a response that arrives
without an `id` field for a request that had one.

Single in-flight call per guardian (current contract — we
don't pipeline). Future pipelining would require a response
demux; out of scope here, but the ID check is a prerequisite
either way.

Tests: `mcp_test.go`
`TestCall_RejectsResponseIdMismatch` uses a fake guardian
that responds with `id: 999` to every request.

**4.1.5 H12 — bound local Chat() response body**

```go
// local.go Chat()
data, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalResponseBytes))
```

`maxLocalResponseBytes` constant (16 MB; large enough for
realistic LLM responses, small enough to bound memory).
Mirror in `ChatStream`'s success path (currently
unbounded; the 512 B cap there only applies to error
bodies).

Tests: `local_http_test.go` adds
`TestChat_RejectsOversizedResponse` using `httptest.Server`
that streams a 32 MB body.

### 4.2 Phase B — unify analysis-tool MITL through `IsToolMITLRequired`, MCP name parsing (H1+H2, H3)

Touches the agent dispatcher. The fix is contract repair,
not a per-tool checklist.

**4.2.1 H1+H2 — route analysis tools through the same MITL
gate the UI already advertises**

Two coordinated changes:

(a) Define the **default MITL category** for every analysis
tool in one place, alongside the existing shell-tool
`Category` semantics:

```go
// internal/agent/tools.go — new
var analysisToolMITLDefault = map[string]bool{
    "load-data":         true,  // host-file ingest
    "reset-analysis":    true,  // destructive
    "promote-finding":   true,  // global state mutation
    "create-report":     false, // local artefact
    "describe-data":     false, // pure read
    "list-tables":       false,
    "query-sql":         true,  // SQL preview already required
    "query-preview":     false, // NL → SQL, no execution
    "suggest-analysis":  false,
    "quick-summary":     false,
    "analyze-data":      true,  // analysis plan already required
}
```

(b) Extend `IsToolMITLRequired` to consult this map for
analysis tools, in priority order:

```go
func (a *Agent) IsToolMITLRequired(toolName string) bool {
    if override, ok := a.cfg.Tools.MITLOverrides[toolName]; ok {
        return override // user override wins
    }
    if strings.HasPrefix(toolName, "mcp__") {
        return true
    }
    if strings.HasPrefix(toolName, "sandbox-") {
        return true
    }
    if def, ok := analysisToolMITLDefault[toolName]; ok {
        return def
    }
    // Shell tools: caller already consults tool.NeedsMITL
    return false
}
```

(c) **Replace** the hard-coded MITL calls inside
`executeAnalysisTool` (currently in `query-sql` and
`analyze-data` branches) with a single dispatcher-level
gate matching the existing shell-tool pattern at
`agent.go:1294`:

```go
case "load-data", "describe-data", "query-sql", ..., "analyze-data":
    if a.analysis == nil {
        return "Error: no analysis engine available", ActivityStatusError
    }
    if a.IsToolMITLRequired(tc.Name) {
        category := "write"
        if tc.Name == "load-data" || tc.Name == "reset-analysis" {
            category = "execute"
        } else if tc.Name == "query-sql" {
            category = "sql_preview"
        } else if tc.Name == "analyze-data" {
            category = "analysis_plan"
        }
        if rejection := a.requestMITL(tc.Name, tc.Arguments, category); rejection != "" {
            return rejection, ActivityStatusError
        }
    }
    result, err := a.executeAnalysisTool(ctx, tc.Name, tc.Arguments)
    ...
```

The category strings `sql_preview` and `analysis_plan` are
existing UI codes the frontend already special-cases for
the SQL-preview and analysis-plan dialogs; preserved
verbatim.

`executeAnalysisTool` then drops its internal
`requestMITL` calls. MITL becomes the dispatcher's
responsibility for every tool source, eliminating the
two-layer split.

**Migration of existing user MITLOverrides**: existing
configs may already have `{"load-data": true}` etc. (saved
by the Settings UI). Those entries now start working —
exactly the user expectation. No migration code needed.

**Behavioural change for users who unchecked `query-sql`
or `analyze-data`** in Settings expecting it to suppress
the prompt: now it actually does. Documented in CHANGELOG
under a "Fixed" subsection. This is the correct behaviour;
the UI was lying before.

Tests: `agent_test.go` adds
`TestAnalysisTool_MITLOverrideRespected` (toggle a tool's
override and assert the prompt fires / doesn't fire),
`TestAnalysisTool_DefaultsMatchTable`,
`TestAnalysisTool_HardCodedQuerySQLBypassRemoved`.

**4.2.3 H3 — robust MCP name parsing**

Validate guardian and tool names at registration:

- Guardian name must match `^[a-zA-Z0-9-]+$`. Validation
  in `config.Load` rejects (logs + drops profile) on
  malformed entry.
- Tool name from upstream may contain `__` legitimately;
  we keep the dispatcher tolerant by switching to a
  `strings.SplitN(rest, "__", 2)` *plus* a guardian-name
  lookup that uses the longest matching prefix when the
  trivial split fails.

```go
rest := strings.TrimPrefix(tc.Name, "mcp__")
guardianName, toolName, ok := splitMCPName(rest, a.guardians)
if !ok {
    return "Error: invalid MCP tool name format", ActivityStatusError
}
```

`splitMCPName` first tries the naive split; if the resulting
guardian isn't registered, walks every registered guardian
name (already in the map) and picks the longest prefix that
matches. Constant-time worst case (small map).

Tests: `agent_test.go` adds
`TestSplitMCPName_NaiveSplit`,
`TestSplitMCPName_GuardianContainsDoubleUnderscore`,
`TestSplitMCPName_ToolContainsDoubleUnderscore`.

### 4.3 Phase C — crash-safe writes, findings race (C4, H9, H10)

Pure backend, no UI surface.

**4.3.1 Shared atomic-write helper**

New `internal/atomicio/writefile.go`:

```go
// WriteFileAtomic writes data to path via tmp+rename so
// readers always see either the old or new file, never
// partial. Caller-supplied perm. fsync of the renamed file
// is a portability hazard on macOS APFS (fsync of a file
// doesn't fsync its directory entry); we additionally fsync
// the parent dir on POSIX systems where it is meaningful.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error
```

Used by:
- `objstore.saveLocked`
- `findings.SaveLocked`
- `pinned.Save`
- `contextbuild/cache.Save`
- `memory.Session.Save` (already? confirm; if not, included)

Tests: `atomic_test.go` covers the rename happy path, the
case where the source file already exists, and a simulated
power-fail (no rename) leaves the previous file readable.

**4.3.2 H9 — findings ID generation under lock**

Add `sync.Mutex` to `Store`. `Add` takes the lock around
the read of `len(s.findings)`, the construction of the ID,
the append, and the in-memory state mutation. A subsequent
on-disk save call is serialised by the same lock.

Switch the ID format to `f-YYYYMMDD-NNN` (current) but
fall back to `f-YYYYMMDD-NNNNNN-<6 hex>` if the day's count
ever exceeds 999. The hex suffix prevents collision with
legacy IDs (none have a third dash) and avoids needing a
on-disk migration.

Tests: `findings_test.go`
`TestStore_AddIsThreadSafe` runs 64 goroutines; asserts
unique IDs and final count matches goroutine count.

### 4.4 Phase D — MCP image pin warning, ToolCall validation, objstore ID widening, symlink rejection (H5, H6, H11, H14)

**4.4.1 H5 — image-pin advisory**

We do not enforce digest pinning (would break existing
configs). Instead:

1. `imagebuild` exposes `IsImageDigestPinned(tag string) bool`
   (regex `@sha256:[a-f0-9]{64}$`).
2. `GetSandboxImageStatus` adds a new field
   `ActivePinnedByDigest bool`. The Settings dialog
   (Sandbox tab) displays a warning banner when the active
   image is set and not pinned: "This image uses a mutable
   tag. A registry or network compromise could replace it.
   Consider pinning by digest." Banner is dismissible per
   image (stored in `cfg.UI.DismissedSandboxWarnings`).

Tests: `imagebuild_test.go` covers the regex; frontend
component test in `SettingsDialog.test.tsx` (if it exists;
otherwise add a snapshot test for the new banner).

**4.4.2 H6 — validate `ToolCall.Arguments`**

In `local.go` Chat() and ChatStream() result assembly:

```go
// Default 1 MiB. This is a garbage / attack detection
// threshold, not a tight resource limit — the 16 MiB
// response-body cap (H12) is the actual memory defence. Real
// tool calls (sandbox-write-file with HTML/CSV/Python,
// create-report with long markdown) routinely reach
// 100–500 KB; 1 MiB leaves headroom and stays well under
// the response cap. Override per-backend via cfg.LLM.*.MaxToolCallArgsBytes.
const defaultMaxToolArgsBytes = 1024 * 1024

for _, tc := range msg.ToolCalls {
    args := tc.Function.Arguments
    if len(args) > l.maxToolArgsBytes() {
        return nil, fmt.Errorf("tool call %q arguments exceed %d bytes",
            tc.Function.Name, l.maxToolArgsBytes())
    }
    if !json.Valid([]byte(args)) {
        return nil, fmt.Errorf("tool call %q arguments are not valid JSON",
            tc.Function.Name)
    }
    result.ToolCalls = append(result.ToolCalls, ToolCall{
        ID: tc.ID, Name: tc.Function.Name, Arguments: args,
    })
}
```

`config.LocalConfig` and `config.VertexAIConfig` both gain a
`MaxToolCallArgsBytes int` field (0 → default 1 MiB).
Surfaced in Settings only via direct config-file edit; not
worth UI surface for the rare user who needs to push it
higher.

A malformed argument from a local LLM bubbles up to the
agent loop, which already classifies LLM errors and re-asks
with a hint — no new error path.

Tests: `local_test.go`
`TestChat_RejectsOversizedToolArguments`,
`TestChat_RejectsInvalidJSONToolArguments`.

**4.4.3 H11 — widen objstore IDs + collision check**

Bump `generateID` to 16 bytes (32-char hex). 128-bit space
makes collisions astronomically improbable. Existing
12-char IDs continue to load (the read path doesn't care
about length); new IDs use the wider format.

Add a defensive check in `Store()`: if the chosen ID
already exists in the index, regenerate up to 3 times,
then return an error. Should never trigger in practice;
makes a future shrink to a smaller ID space safer.

Tests: `objstore_test.go`
`TestStore_RejectsCollidingID` (forces a collision via a
test seam) and `TestGenerateID_NewIdsAre32Hex`.

**4.4.4 H14 — reject symlinks in analysis path validation**

`internal/analysis/engine.go` `validateFilePath`:

```go
info, err := os.Lstat(path)
if err != nil {
    return fmt.Errorf("stat: %w", err)
}
if info.Mode()&os.ModeSymlink != 0 {
    return fmt.Errorf("symlinks are not allowed: %q", path)
}
if !info.Mode().IsRegular() {
    return fmt.Errorf("not a regular file: %q", path)
}
```

`config.ExpandPath` for `~/` is unchanged (`~` resolves to
the current user's home — that's a documented behaviour),
but we now resolve and recheck after expansion: if the
expanded path crosses a symlink leading outside `DataDir()`
or the user's home, refuse.

Tests: `engine_test.go`
`TestValidateFilePath_RejectsSymlink` constructs a symlink
in `t.TempDir()` and asserts rejection.

### 4.5 Phase E — guard fail-closed (L1)

`chat.go:172-174` and `contextbuild/render.go:59-61`:

```go
wrapped, err := e.guardTag.Wrap(content)
if err != nil {
    // crypto/rand failure or similar catastrophic state.
    // Refuse to proceed — better to return an error to the
    // caller than silently feed unwrapped untrusted content
    // to the LLM with our system prompt.
    return llm.Message{}, fmt.Errorf("guard wrap: %w", err)
}
content = wrapped
```

`BuildMessages` becomes `(messages, error)`; callers
propagate the error. `Engine.WrapUserToolContent` returns
`(string, error)` and contextbuild treats a wrap error as
fatal for the whole build.

Tests: `chat_security_test.go` adds a fake guard that
returns an error; assert `BuildMessages` returns the error
and an empty message slice.

## 5. Touched files

| Phase | File | Change |
|---|---|---|
| A | `internal/analysis/engine.go` | parameterise `refreshTableMeta` |
| A | `internal/analysis/engine_test.go` | regression for SQL escaping |
| A | `internal/mcp/mcp.go` | drain stderr; validate response ID |
| A | `internal/mcp/mcp_test.go` | stderr-flood + ID-mismatch tests |
| A | `internal/sandbox/limitedbuffer.go` (new) | bounded Writer |
| A | `internal/sandbox/cli.go` | use limitedBuffer in `Exec` |
| A | `internal/sandbox/cli_test.go` | truncation test |
| A | `internal/llm/local.go` | LimitReader on Chat success body |
| A | `internal/llm/local_http_test.go` | oversized-response test |
| B | `internal/agent/tools.go` | `analysisToolMITLDefault` map; remove hard-coded MITL from query-sql / analyze-data internal handlers |
| B | `internal/agent/agent.go` | extend `IsToolMITLRequired` with analysis-tool defaults; route analysis branch through it |
| B | `internal/agent/tools_test.go` | MITL coverage + override-respected tests |
| B | `internal/agent/agent.go` | `splitMCPName` helper |
| B | `internal/agent/agent_test.go` | three split tests |
| B | `internal/config/config.go` | guardian-name regex on Load |
| C | `internal/atomicio/writefile.go` (new) | tmp+rename helper |
| C | `internal/atomicio/writefile_test.go` (new) | atomicity tests |
| C | `internal/objstore/objstore.go` | atomic save |
| C | `internal/findings/findings.go` | mutex + atomic save + ID overflow format |
| C | `internal/findings/findings_test.go` | concurrency test |
| C | `internal/memory/pinned.go` | atomic save |
| C | `internal/contextbuild/cache.go` | atomic save |
| D | `internal/sandbox/imagebuild/bundle.go` | `IsImageDigestPinned` |
| D | `bindings.go` | `ActivePinnedByDigest` field; new dismissal config |
| D | `internal/config/config.go` | `DismissedSandboxWarnings` |
| D | `frontend/src/SettingsDialog.tsx` | mutable-tag banner |
| D | `internal/llm/local.go` | argument-size + JSON-validity check |
| D | `internal/llm/local_test.go` | oversized / malformed args tests |
| D | `internal/objstore/objstore.go` | 16-byte IDs + collision regen |
| D | `internal/objstore/objstore_test.go` | width + collision tests |
| D | `internal/analysis/engine.go` | `validateFilePath` symlink reject |
| D | `internal/analysis/engine_test.go` | symlink rejection test |
| E | `internal/chat/chat.go` | `BuildMessages` returns error; fail-closed wrap |
| E | `internal/contextbuild/render.go` | propagate wrap error |
| E | `internal/contextbuild/builder.go` | propagate wrap error |
| E | `internal/agent/agent.go` | handle BuildMessages error (surface to user, log) |
| E | `internal/chat/chat_security_test.go` | fail-closed test |

## 6. Test plan

### Unit (no external dependencies)

- C1: SQL injection-style table names round-trip safely.
- C2: 1 MB stderr blast doesn't deadlock guardian's stdout.
- C3: oversized stdout is truncated with marker.
- C4 / H10: torn-write simulation — kill the process
  mid-rename, on next start the previous file is intact.
- H4: ID-mismatch response is rejected.
- H6: oversized / malformed args from local backend rejected.
- H9: 64-goroutine concurrent `Add` produces unique IDs.
- H11: ID width = 32 hex; forced collision rejected.
- H12: oversized success body rejected.
- H14: symlink rejected at validateFilePath.
- L1: guard.Wrap error → BuildMessages returns error.

### Integration (require podman/docker on PATH)

- Existing `internal/sandbox/integration_test.go` continues
  to pass.
- New `TestIntegration_StdoutCapBlocksOOM`: Python tool
  writes >> cap; assert the result truncates and the agent
  loop returns within the timeout.

### Manual

- Start the app, ingest a CSV via `load-data`; confirm the
  MITL prompt now appears.
- In Settings → Sandbox, with active image set to
  `python:3.12-slim`, confirm the new mutable-tag banner is
  visible; pin to a digest, confirm it disappears.
- Force-quit during a `promote-finding` round; reopen,
  confirm findings.json is intact (either old or new
  state, never partial).

## 7. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Atomic-write helper introduces extra `fsync` latency on every JSON store update | The stores write small files and are already off the LLM hot path. A microbenchmark in `atomic_test.go` measures actual cost; if >5 ms median we relax the dir fsync. |
| MITL prompt on `load-data` annoys users who currently expect immediate ingestion | The user can disable per-tool via `MITLOverrides` (existing knob). Document in CHANGELOG and README. |
| `splitMCPName` longest-prefix walk is slow for huge guardian sets | `len(guardians)` is human-typed config, realistically ≤ 10. O(n) walk is fine. |
| Dropping unwrapped content on guard failure breaks chat for users hitting a one-time crypto/rand glitch | crypto/rand failure on macOS in practice means the kernel is unreadable — the app has bigger problems. Surface a clear error message and let the user retry. |
| Local backend response cap (16 MB) cuts off legitimate huge responses | Configurable via `cfg.LLM.Local.MaxResponseBytes`; default 16 MB documented in README. |
| Object ID width change forces a re-link of existing references in markdown reports | No — read path is length-tolerant; old objects keep their old IDs. Only newly created IDs are 32 hex. |

## 8. Out of scope (explicitly)

- **C5 — MITL request-ID binding.** The audit flagged this
  but v0.1.18 Phase 3 already replaced the buffered
  channel with a per-request slot, and the agent loop is
  single-threaded so two MITL requests can't be in flight
  at once. No additional fix.
- **H7 — guard wrap fallback.** Folded into L1
  (fail-closed behaviour) above.
- **H8 — `splitIdx = i + 1` budget off-by-one.** Re-reading
  the slice arithmetic, the current code is correct: `raw[:splitIdx]`
  with `splitIdx = i+1` produces the records 0..i inclusive
  (the record at index i didn't fit, so it goes into the
  older tail to be summarised). This was a misread in the
  initial review.
- **H13 — Dockerfile direct edit by user.** This is the
  explicit Sandbox UX (the user defines their own
  execution environment); user is the trust authority.
- **MCP guardian orphan on parent crash.** Existing
  v0.1.18 startup-sweep handles container leaks, but MCP
  guardians are stdio child processes — a parent crash
  closes their stdin, which is the documented signal for
  them to exit. Verified empirically in v0.1.18; no fix
  needed.
- **Vertex AI error wrapping.** Re-checked — the SDK
  errors we propagate already redact project IDs in
  practice. No change.
- **Loop-detector evasion.** The detector is an *advisory*
  hint, not a safety limit; max-rounds caps the actual
  loop. Not worth the complexity.
- **Frontend ESLint rule for `rehypeRaw`.** Worth doing as
  a hygiene PR; out of scope here because it's not a code
  fix but a guard rail. Captured in TODO.md.
- **`MITLOverrides` key validation.** User-controlled
  config; user is the trust authority. Keep as-is.

## 9. Phasing

Five small commits, in order. Each ships with its tests and
a CHANGELOG entry. We verify against a real session between
phases. v0.1.20 release after Phase E.

1. **fix(security): bound MCP and sandbox I/O, parameterise DuckDB metadata query (C1, C2, C3, H4, H12).** Highest impact; the C-tier issues. Backend-only.
2. **fix(security): make Settings → Tools MITL toggles actually work for analysis tools; robust MCP name parsing (H1+H2, H3).** Routes analysis tools through the same `IsToolMITLRequired` gate the UI already advertises; removes the hard-coded MITL bypass on query-sql / analyze-data; documents the behavioural change. Verified against a manual ingestion run with the toggle flipped both ways.
3. **fix(security): atomic JSON writes and findings ID race (C4, H9, H10).** New `internal/atomicio` package; mechanical sweep across stores.
4. **feat(security): mutable-tag warning banner; widen objstore IDs; reject symlinks in analysis paths; validate ToolCall args (H5, H6, H11, H14).** UI surface in (D); the rest is backend.
5. **fix(security): guard wrap is fail-closed (L1).** Smallest change; ships last because it changes a public function signature.

Each phase passes `make test` (with `-tags no_duckdb_arrow`)
and an integration smoke. AGENTS.md, README.md, README.ja.md,
CHANGELOG.md updated in the same commit as the behaviour
change per project convention.

## 10. Verification follow-ups

Two issues surfaced while running the v0.1.20 smoke tests
against a real session and were folded into the same release
because the underlying gaps were inseparable from the work in
this round. Documenting them here keeps the design ↔ shipped
mapping honest.

### 10.1 Settings → Tools toggle reflects the dispatcher's actual default (commit `324f93f`)

Phase B unified MITL routing through `IsToolMITLRequired` and
introduced the `analysisToolMITLDefault` map. The frontend's
`SettingsDialog.tsx`, however, was still computing the
"default" toggle state locally as
`category === 'write' || category === 'execute' || source === 'mcp'`
— a calculation that pre-dated `analysisToolMITLDefault` and
silently went out of sync when it was added. Concrete symptom:
`load-data` (category `read`, source `analysis`) rendered as
toggle-OFF; clicking it toggled to ON and back, but neither
state diverged from the locally-computed default so no override
was persisted; the dispatcher then prompted anyway because
`IsToolMITLRequired("load-data")` returned true via the new
map.

Fix:

- `agent.ToolInfoItem` and `bindings.ToolInfo` both gain a
  `MITLDefault bool` field.
- `Agent.ListTools` populates it via a new `Agent.toolMITLDefault`
  helper that mirrors `IsToolMITLRequired`'s rules with no
  override consulted.
- `SettingsDialog.tsx` reads `t.mitl_default` directly instead
  of recomputing.
- Same audit revealed `IsToolMITLRequired` returned `false`
  for shell tools while the dispatcher's shell-tool branch
  consulted `tool.NeedsMITL()` directly — different paths,
  same intent. Centralised by extending `IsToolMITLRequired`
  to consult the registry as its shell-tool fallback and
  routing the dispatcher's shell branch through it. Now
  every tool source resolves MITL through one function.
- `TestListTools_MITLDefaultMatchesGate` pins the contract.

### 10.2 `load-data` expands `~/` (commit `f67e436`)

§4.4.4 (H14) noted that `config.ExpandPath` "is unchanged"
without flagging that `validateFilePath` itself never called
it. In practice, LLMs pass `~/Desktop/foo.csv` through verbatim
when the user types it that way; `filepath.Abs` leaves the `~`
in place; `os.Lstat` then reports "file not accessible" and
the LLM apologises. Fix: call `config.ExpandPath(path)` before
`filepath.Abs(path)` in `validateFilePath`. One line of code,
plus `TestValidateFilePath_ExpandsTilde` to keep it from
regressing. This now mirrors how MCP profile paths have
always been expanded.
