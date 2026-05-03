# Security Hardening — Design Document

> Date: 2026-05-01
> Status: Shipped in v0.1.18 (Phase 1 87252c2, Phase 2 56cddeb, Phase 3 2be166a, Phase 4 2743947). M4 deferred — see TODO.md.
> Scope: Three HIGH-severity findings and four
> MEDIUM-severity findings from the 2026-05-01 audit.
> Phased into four commits so each can be reviewed and
> reverted independently.

## 1. Background

The 2026-05-01 audit covered the Go backend, frontend, and
sandbox. Issues by severity:

- **HIGH**:
  - H1 — symlink traversal through `/work` bind mount lets a
    hostile LLM read or write arbitrary host files
    (`internal/agent/sandbox_tools.go:466-489`,
    `internal/sandbox/cli.go:155`).
  - H2 — `objstore.Store` map has no mutex; the Go runtime
    panics with `concurrent map writes` under realistic
    workloads (`internal/objstore/objstore.go:42-63`).
  - H3 — `:Z` SELinux mount label is appended unconditionally;
    silently wrong on macOS / docker-desktop and can clobber
    labels on shared parents on Linux+docker
    (`internal/sandbox/cli.go:154`).
- **MEDIUM**:
  - M1 — `mitlChan` is a buffered channel reused across MITL
    prompts; a stray Approve click stays in the buffer and
    auto-approves the next prompt (`bindings.go:96, 314-335`).
  - M2 — `agent.guardians` map is written without
    `guardiansMu.Lock()` in `startGuardians`, racing with
    readers (`internal/agent/agent.go:417-447`).
  - M3 — sandbox containers leak on SIGKILL/panic; the
    "startup `StopAll` sweep" mentioned in code comments is
    not actually wired up (`bindings.go:114-134`,
    `internal/agent/agent.go:139-165`).
  - M5 — Settings change to `Network` does not restart a
    running sandbox container, leaving config drift
    (`internal/sandbox/cli.go:111-114`).
  - M6 — `sandbox-load-into-analysis` passes a stat'd path to
    DuckDB which resolves symlinks itself; needs the same
    EvalSymlinks-then-prefix-check as H1
    (`internal/agent/sandbox_tools.go:369`).

`M4` (DuckDB `LoadFile` uses string-concat SQL with single-quote
doubling) is **out of scope** for this round. The current
defence works; parameterising would require restructuring the
analysis package and is better addressed when DuckDB's bind API
is more battle-tested in our stack.

## 2. Goals / Non-goals

### Goals

1. Eliminate the symlink-traversal escape from `/work`
   (H1, M6) — both writes and reads.
2. Eliminate the `objstore.Store` race so the runtime can no
   longer panic under concurrent agent activity (H2).
3. Stop appending `:Z` on engines / hosts where it is
   incorrect or harmful (H3).
4. Eliminate stale MITL responses (M1).
5. Lock `agent.guardians` writes consistently (M2).
6. Sweep stale containers at startup, and react to live
   sandbox-config changes (M3, M5).

### Non-goals

- **No new sandbox API**. Existing six tools, same shapes.
  The fix is enforcement-side only.
- **No SQL parameterisation refactor** (M4 deferred).
- **No SELinux test rig**. Conditional `:Z` is verified by
  unit tests on the flag-builder, not by spinning up a
  SELinux VM.
- **No new threat model**. Same: untrusted LLM output / tool
  args / MCP server output, trusted host user.

## 3. Detailed design

### 3.1 Phase 1 — concurrency correctness (H2, M2)

Pure Go. No UI, no integration surface.

**3.1.1 `objstore.Store` mutex**

Add `sync.RWMutex` to `Store`. Lock policy:

| Method                       | Lock |
|------------------------------|------|
| `Store`, `Save`              | `Lock` |
| `Delete`, `DeleteBySession`  | `Lock` |
| `Get`, `All`, `ListBySession`, `ListByType`, `ReadData` | `RLock` |

`ReadData` returns an `io.ReadCloser` that lives past the lock
release. The lookup of metadata is done under `RLock`; opening
the file is **outside** the lock so a slow consumer doesn't
block writers. The metadata pointer is captured into a local
before unlocking, so the caller cannot observe a torn state.

`Save` writes `index.json` under `Lock` since it iterates the
map; the marshalled bytes are written to disk under `Lock` too
to avoid a write-then-deleted-key race.

**3.1.2 `agent.guardians` write lock**

Wrap the map mutation in `startGuardians` with
`a.guardiansMu.Lock()` / `defer Unlock()`. Same pattern as
`RestartGuardians` already uses.

**3.1.3 Tests**

- `objstore` package: `TestStore_ConcurrentStoreAndList`
  spawns 32 goroutines, half writing, half listing, asserts
  no panic and no torn read. Run with `-race`.
- `agent` package: existing `TestRestartGuardians` extended
  to run concurrently with `ListTools`, `-race`.

### 3.2 Phase 2 — sandbox isolation (H1, H3, M3, M5, M6)

Higher risk because it touches the sandbox surface, but each
sub-fix is locally scoped.

**3.2.1 `safeWorkPath` symlink check (H1)**

Replace the lexical-only path with:

```go
func (a *Agent) safeWorkPath(sid, rel string) (string, error) {
    workDir := a.sandbox.WorkDir(sid)             // host path of /work for this session
    cleaned := filepath.Clean(filepath.Join(workDir, rel))
    if !strings.HasPrefix(cleaned, workDir+string(filepath.Separator)) && cleaned != workDir {
        return "", fmt.Errorf("path escapes /work: %q", rel)
    }

    // Resolve symlinks on every existing component.
    // EvalSymlinks errors on a non-existent leaf; for write
    // operations the leaf may not yet exist, so we resolve the
    // parent and rejoin the leaf.
    parent := filepath.Dir(cleaned)
    leaf := filepath.Base(cleaned)
    resolvedParent, err := filepath.EvalSymlinks(parent)
    if err != nil {
        // Parent doesn't exist either → cleaned path is fine
        // as long as Mkdir below stays inside workDir, which
        // the lexical check above already guaranteed.
        if !errors.Is(err, fs.ErrNotExist) {
            return "", err
        }
        return cleaned, nil
    }
    if !strings.HasPrefix(resolvedParent, workDir) {
        return "", fmt.Errorf("path escapes /work via symlink: %q", rel)
    }
    final := filepath.Join(resolvedParent, leaf)

    // If the leaf itself exists and is a symlink, reject —
    // even a same-directory symlink is an attack surface for
    // future operations on the same session.
    if info, err := os.Lstat(final); err == nil && info.Mode()&os.ModeSymlink != 0 {
        return "", fmt.Errorf("path is a symlink: %q", rel)
    }
    return final, nil
}
```

Applied at every `os.Open` / `os.Create` / `os.WriteFile` /
`os.Stat` site that takes an LLM-controlled path:

- `toolSandboxWriteFile` (current `os.WriteFile`)
- `toolSandboxCopyObject` (current `os.Create`)
- `toolSandboxRegisterObject` (current `os.Open`)
- `toolSandboxLoadIntoAnalysis` (current `os.Stat` and the path
  passed to DuckDB) — covers M6.

After `safeWorkPath` returns, the caller passes the resolved
path verbatim. `LoadIntoAnalysis` passes the resolved path to
DuckDB so DuckDB has nothing to resolve.

**3.2.2 Conditional `:Z` mount label (H3)**

Stash `engineKind` (podman / docker) and `osKind` (linux /
darwin / other) on `cliEngine`. In `buildRunArgs`, append `:Z`
only when `engineKind == podman && osKind == linux`. Otherwise
append `:rw` (explicit; current default). No new config knob
— the right answer is determined by environment.

**3.2.3 Startup sweep + `Close` reliability (M3)**

In `maybeStartSandbox`, immediately after `sandbox.NewCLI`
succeeds and before `EnsureContainer`, call
`eng.StopAll(ctx)`. Log how many were swept (zero on a clean
launch). The sweep filters on the existing
`label=app=shell-agent-v2`, so foreign containers stay
untouched.

We also wire a `signal.Notify(SIGINT, SIGTERM)` handler in
`main.go` that calls `agent.Close()` before `os.Exit(0)`. The
existing Wails `shutdown` hook stays — the signal handler is
a belt-and-braces for cases where the OS terminates the
process before Wails's hook fires (e.g., `Cmd-Q` triggers
shutdown; `kill -TERM` may not).

`SIGKILL` cannot be intercepted; that case is covered by the
startup sweep.

**3.2.4 SaveSettings restarts the sandbox on config drift (M5)**

`Bindings.SaveSettings` already calls `RestartLLMBackend` when
backend config changes. Add a parallel
`RestartSandboxIfChanged`: compare the previous
`config.SandboxConfig` to the new one (`reflect.DeepEqual` is
fine — small struct, no maps); if different, call
`agent.RestartSandbox(ctx)` which `StopAll`s and lets the next
tool call recreate.

If `Sandbox.Enabled` flips from `true` to `false`, the
existing tools are unregistered. From `false` to `true`, the
tools become available on next `buildToolDefs`. We don't try
to mid-flight cancel an in-progress sandbox exec — the user
can always abort.

**3.2.5 Tests**

- `sandbox_tools_test.go`: `TestSafeWorkPath_BlocksAbsolute`,
  `TestSafeWorkPath_BlocksDotDot`,
  `TestSafeWorkPath_BlocksSymlinkLeaf`,
  `TestSafeWorkPath_BlocksSymlinkInParent`,
  `TestSafeWorkPath_AcceptsValidNewLeaf`. The symlink tests
  use `t.TempDir()` and `os.Symlink` to construct the trap.
- `cli_test.go`: `TestBuildRunArgs_ZLabelOnPodmanLinuxOnly`
  drives `engineKind`/`osKind` permutations and asserts the
  emitted argv list.
- `bindings_test.go`: `TestSaveSettings_RestartsSandboxOnChange`
  uses a fake sandbox engine that records `StopAll` calls.

### 3.3 Phase 3 — MITL channel hardening (M1)

Replace `b.mitlChan chan agent.MITLResponse` (single,
buffer 1, lifetime of `*Bindings`) with a per-request
channel:

```go
type Bindings struct {
    ...
    mitlMu  sync.Mutex
    mitlReq *mitlSlot
}
type mitlSlot struct {
    req agent.MITLRequest
    ch  chan agent.MITLResponse
}
```

In the MITL handler closure (`bindings.go` `agent.SetMITLHandler`):

```go
func(req agent.MITLRequest) agent.MITLResponse {
    ch := make(chan agent.MITLResponse, 1)
    b.mitlMu.Lock()
    b.mitlReq = &mitlSlot{req: req, ch: ch}
    b.mitlMu.Unlock()

    wailsRuntime.EventsEmit(b.ctx, "mitl:request", ...)

    resp := <-ch

    b.mitlMu.Lock()
    b.mitlReq = nil
    b.mitlMu.Unlock()
    return resp
}
```

Approve / Reject / RejectWithFeedback resolve via the slot:

```go
func (b *Bindings) ApproveMITL() {
    b.mitlMu.Lock()
    slot := b.mitlReq
    b.mitlMu.Unlock()
    if slot == nil { return } // stale click, no-op
    select {
    case slot.ch <- agent.MITLResponse{Approved: true}:
    default: // already resolved
    }
}
```

Stray clicks now find `mitlReq == nil` and silently no-op.
Double-click on Approve hits the `default` branch on the
second attempt. The next MITL request gets a fresh channel,
so no buffered-stale-value path exists.

**Tests**:
`bindings_test.go`: `TestMITL_StrayApproveBeforeRequest_NoOps`,
`TestMITL_DoubleApproveSameRequest_Idempotent`,
`TestMITL_TwoRequestsInSeries_NoLeakBetween`.

### 3.4 Phase 4 — out of scope

`M4` (SQL parameterisation in analysis package) deferred. No
work in this round.

## 4. Touched files

| Phase | File | Change |
|---|---|---|
| 1 | `internal/objstore/objstore.go` | RWMutex |
| 1 | `internal/objstore/objstore_test.go` | concurrency test |
| 1 | `internal/agent/agent.go` | `startGuardians` lock |
| 2 | `internal/agent/sandbox_tools.go` | `safeWorkPath` rewrite + apply at LoadIntoAnalysis |
| 2 | `internal/sandbox/cli.go` | conditional `:Z`, expose `StopAll` from `engine` interface (already there) |
| 2 | `internal/agent/agent.go` | startup sweep in `maybeStartSandbox`, SaveSettings drift detection |
| 2 | `main.go` | SIGINT/SIGTERM handler |
| 2 | `bindings.go` | SaveSettings sandbox-restart wiring |
| 2 | tests as above |
| 3 | `bindings.go` | mitlReq slot, mutex |
| 3 | `bindings_test.go` | three new tests |

## 5. Test plan

### Unit (no external dependencies)
- objstore concurrent stress (`-race`).
- guardians map race (`-race`).
- safeWorkPath positive / negative cases including symlinks.
- buildRunArgs `:Z` permutations.
- MITL slot races.

### Integration (require podman/docker on PATH)
- Existing `internal/sandbox/integration_test.go` continues
  to pass.
- New `TestIntegration_SymlinkAttempt` — inside the
  container, create `/work/foo → /etc/passwd`, then attempt
  `sandbox-write-file path=foo` from outside; assert host
  `/etc/passwd` is unchanged and the tool returns an error.

### Manual
- Start the app with no `podman machine` running; confirm
  startup sweep does nothing harmful.
- Run a session, force-kill the app (Activity Monitor), restart;
  confirm previous container is swept on next startup.
- Open MITL prompt, click Approve twice fast; confirm only
  one resolution.
- Click Approve when no MITL is pending (would require dev-mode
  binding access); confirm no-op.

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| `safeWorkPath` rewrite breaks legitimate dot-paths or paths with intermediate components | Add positive tests for `subdir/file.csv`, `./file.csv`, `a/b/c.txt`; explicitly accept `os.IsNotExist` on the parent for write operations targeting a brand-new directory |
| Conditional `:Z` introduces a runtime os.Hostname-style probe each `Exec` | Probe once at `NewCLI`, cache on the cliEngine struct |
| Startup sweep accidentally removes a still-useful container from a still-running second instance of the app | shell-agent-v2 is a single-instance macOS app (`.app` bundle); two instances aren't supported. If we ever support it, gate sweep on a leader-election sentinel. Out of scope. |
| MITL rewrite changes the timing of `mitl:request` event emission | The event still fires before the handler blocks on `<-ch`; UI behaviour identical. Tests assert ordering. |
| Existing user sessions still have stored objects whose paths assumed the lexical-only check | No on-disk format change. Old sessions just see the new check on the next access — and the only thing the check is stricter about is symlinks, which user sessions wouldn't be relying on. |

## 7. Phasing

Four small commits, in order:

1. **chore(security): concurrency locks for objstore and guardians (H2, M2).** Pure correctness; lowest risk.
2. **fix(security): block symlink traversal in /work and conditional :Z mount label (H1, H3, M6).** Highest impact, scoped to sandbox layer.
3. **feat(sandbox): startup container sweep + signal-driven shutdown + restart on config drift (M3, M5).** Reliability, depends on Phase 2.
4. **fix(security): per-request MITL channel (M1).** Independent of phases 1-3, can ship anytime; placed last because lowest active threat (requires user UI race + missing-prompt-state, harder to trigger than the others).

Each commit ships with its tests; we verify against a real
session between phases. v0.1.18 release after Phase 4.

## 8. Out of scope

- M4 (DuckDB SQL parameterisation) — current defence works.
- Container `--pids-limit` (LOW finding) — fork-bomb impact is
  bounded by `--cpus`/`--memory` already.
- ChatInput SVG filter tightening — defence-in-depth only;
  separate cleanup commit if/when convenient.
- TestSendReturnsToIdle environmental flake — already in TODO.md.
