# ADR-0026: Surface the MCP guardian process exit status on Start failure

- Status: Accepted
- Deciders: magi
- Related: ADR-0024 (background guardian startup), mcp-guardian ADR-0002, `feedback_dropbox_synced_binary`

## 1. Context

When an MCP guardian process dies during `Guardian.Start()`, the only
error the user sees is the symptom of the closed pipe, not the cause:

```
MCP guardian "aws" start failed: initialize: write request: write |1: broken pipe
MCP guardian "slack" start failed: initialize: read response: EOF
```

These come from `call()`'s blocking `stdout.Scan()` returning EOF (the
process closed its stdout) or the stdin write hitting a broken pipe —
i.e. the guardian process exited before answering `initialize`. The
*reason* it exited is invisible.

This actually happened: the user installed a freshly-built
`mcp-guardian` into `~/bin` (a Dropbox-synced path) with `cp`; macOS
stamped it with `com.apple.provenance` + quarantine and **SIGKILLed it
on launch** (`exit 137`). Every guardian spawn died instantly, for all
profiles (including the stdio `aws` one), and kept failing across app
restarts. The opaque `read response: EOF` made it look like a
restart/code bug — a lot of time went into diagnosing what a single
`signal: killed` in the error would have made obvious. (See
`feedback_dropbox_synced_binary`.)

The guardian already drains the child's stderr to the debug log
(`mcp.go`), but on a `SIGKILL` the process produces no stderr, and the
exit status (`signal: killed`) is never captured or surfaced.

## 2. Decision

Capture the guardian process's exit status and include it in `Start`
failures, with a targeted hint when it was `signal: killed`.

The implementation must respect the `os/exec` rule that `cmd.Wait()`
must not race the pipe reads. We place the single `cmd.Wait()` call in
the **existing stderr-drain goroutine**, after its read loop ends:

```go
// Guardian gains:
//   exited  chan struct{}
//   exitErr error

// stderr drain goroutine (Start):
go func() {
    s := bufio.NewScanner(stderrPipe)
    ...
    for s.Scan() { logger.Debug("mcp[%s] stderr: %s", name, s.Text()) }
    // stderr EOF ⇒ the process has exited (or is exiting). This
    // goroutine owns the stderr read, and by now the init goroutine's
    // stdout read has also returned on the same EOF, so Wait no longer
    // races any pipe read. Wait is called from exactly one place.
    g.exitErr = g.cmd.Wait()
    close(g.exited)
}()
```

`Start`'s failure branch, when the init error is a closed-pipe error
(EOF / broken pipe / file already closed), waits briefly for `exited`
and enriches the returned error:

```go
case err := <-done:
    if err != nil {
        g.Stop()
        if isPipeClosed(err) {
            select {
            case <-g.exited:
            case <-time.After(2 * time.Second):
            }
            if g.exitErr != nil {
                return fmt.Errorf("guardian process exited before initialising: %v%s",
                    g.exitErr, gatekeeperHint(g.exitErr))
            }
        }
        return err
    }
    return nil
```

`gatekeeperHint` appends, only when the exit looks like a `SIGKILL`
(`signal: killed`), a one-line pointer:

> " — the process was killed on launch; if the binary lives under a
> quarantined or cloud-synced path (e.g. ~/bin via Dropbox), macOS may
> be SIGKILLing it: re-sign with `codesign --force --sign -` or move it
> out of the synced path."

Non-pipe errors (e.g. an `initialize` RPC error — mcp-guardian ADR-0002,
where the process is **alive** and returned a JSON-RPC error) are
returned unchanged; we do not wait on `exited` for those (the process
isn't dead).

### Why the stderr goroutine owns Wait

- It already owns the stderr pipe read, so calling `Wait` after its loop
  ends doesn't race that read.
- The stdout read lives in the init/`call` goroutine, which has already
  returned (its `Scan` saw the same EOF) by the time we inspect the
  failure — so `Wait` doesn't race the stdout read either.
- `Wait` is called from exactly one site ⇒ no double-`Wait`. `Stop()`
  keeps doing only `Kill` (no `Wait`); the kill makes the child exit,
  the stderr goroutine sees EOF and performs the single `Wait`.
- A still-alive guardian (success, or an RPC-error from ADR-0002) leaves
  the stderr goroutine blocked on `Scan`, so `Wait` is never called and
  `exited` never closes — which is exactly correct.

## 3. Consequences

- A guardian killed on launch now fails with
  `guardian process exited before initialising: signal: killed — …macOS
  may be SIGKILLing it: re-sign or move it out of the synced path`,
  pointing straight at the cause instead of `read response: EOF`.
- A guardian that exits non-zero surfaces `exit status N`.
- The death path also reaps the child (the `Wait`), so no zombie is left
  for the failure case.
- macOS-only hint wording, but harmless on other platforms (the hint is
  only appended for `signal: killed`, which is the relevant signal).

## 4. Rejected alternatives

- **A dedicated `cmd.Wait()` watcher goroutine started in `Start`.**
  Races the init goroutine's `StdoutPipe`/`StderrPipe` reads (the
  documented `os/exec` footgun) and risks closing pipes mid-read.
  Rejected in favour of letting the stderr-read owner call `Wait`.
- **Reap in `Stop()` (Wait after Kill) + a `RestartGuardians` reap.**
  Tempting, but the restart problem that triggered this was the
  SIGKILLed binary, not a reap race: a `Kill`ed child releases its fds
  / connections on death even unreaped (a zombie only holds a PID slot).
  So broad reaping isn't needed for correctness; keep this ADR scoped to
  diagnosability. (If zombie accumulation is ever observed, revisit.)
- **Pattern-match the child stderr for auth/kill strings.** Fragile; a
  `SIGKILL`ed process emits no stderr anyway. The exit status is the
  reliable signal.

## 5. Out of scope

- Auto-fixing quarantine / re-signing (environment concern; documented
  in `feedback_dropbox_synced_binary`).
- Changing `Stop()` / `RestartGuardians`.
- The 15s start-timeout path messaging (a hung-but-alive process is a
  different case; unchanged — though if we `Stop()`→kill it, the same
  exit-status capture is available).

## 6. Implementation

- `app/internal/mcp/mcp.go`:
  - `Guardian`: add `exited chan struct{}` and `exitErr error`; init
    `exited` in `Start` before spawning.
  - stderr goroutine: after the `Scan` loop, `g.exitErr = g.cmd.Wait()`
    then `close(g.exited)`.
  - `Start` failure branch: `isPipeClosed(err)` ⇒ wait on `exited`
    (≤2s) and wrap `g.exitErr` + `gatekeeperHint`.
  - add `isPipeClosed(err) bool` (matches `io.EOF` / "broken pipe" /
    "file already closed") and `gatekeeperHint(err) string`.
- `app/internal/mcp/guardian_test.go`: a stub upstream that
  `kill -KILL $$`es itself (or exits non-zero) ⇒ `Start` returns an
  error containing `signal: killed` (resp. `exit status N`); the
  existing happy-path and timeout tests stay green.

Verification: `go test ./internal/mcp/ -tags no_duckdb_arrow`; manual —
point a profile's `binary` at a quarantined/SIGKILLed binary and confirm
the MCP settings error reads the new message.
