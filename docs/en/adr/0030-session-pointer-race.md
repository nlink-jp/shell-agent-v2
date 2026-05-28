# ADR-0030: Synchronise `a.session` pointer access between agentLoop and title generation

- Status: Accepted
- Deciders: magi
- Related: ADR-0015 (deferred extraction + auto-dispatch), ADR-0021 (FSM consistency), Issue #13 (flaky tests that surfaced this race)

## 1. Context

Investigating issue #13 (flaky `TestAutoDispatch_DrainedByWait` and
`TestQueuedSend_ExtractionErrorStillDispatches`) the Go race detector
caught a separate, genuine data race in the production code:

```
WARNING: DATA RACE
Read at 0x… by goroutine 96 (title generation):
  agent.go:2920  generateTitleIfNeeded   ← a.session == nil || a.session.Title …
  agent.go:2194  postResponseTasks.func1.1   (title goroutine)

Previous write at same addr by goroutine 98 (auto-dispatched SEND):
  agent.go:1925  agentLoop                ← a.session = &memory.Session{…}
  agent.go:1157  SendWithAttachments
  agent.go:2272  postResponseTasks.func2.1.1   (auto-dispatch goroutine)
```

`postResponseTasks` spawns the title-generation goroutine, and the
extraction-completion cleanup can — in the same turn — auto-dispatch
a queued SEND on another goroutine. Both touch `a.session` without
synchronisation:

- `agentLoop` (line 1925): `if a.session == nil { a.session = &memory.Session{...} }`
- `generateTitleIfNeeded` (line 2920): `if a.session == nil || a.session.Title != ...`

In normal production use `a.session` is always initialised by
`LoadSession` before any SEND, so the `agentLoop` nil-init is a
defensive no-op and the race rarely manifests. But the same race
exists between `LoadSession`-driven writes and concurrent title-gen
reads — the test path just makes it deterministic by skipping
`LoadSession` entirely. Either way, the unsynchronised pointer
read/write is incorrect.

## 2. Decision

Apply a minimal **snapshot-under-lock** pattern at the two race sites:

### 2.1. `agentLoop` (line ~1923)

Move the nil-init under `a.mu` so the write is synchronised against
any concurrent reader:

```go
func (a *Agent) agentLoop(ctx context.Context, ...) (string, error) {
    a.mu.Lock()
    if a.session == nil {
        a.session = &memory.Session{ID: "default", Records: []memory.Record{}}
    }
    a.mu.Unlock()
    ...
}
```

### 2.2. `generateTitleIfNeeded` (line ~2918)

Snapshot `a.session` once at entry under the lock, then operate on
the local snapshot for the rest of the function:

```go
func (a *Agent) generateTitleIfNeeded(ctx context.Context) error {
    a.mu.Lock()
    sess := a.session
    a.mu.Unlock()
    if sess == nil || sess.Title != "New Session" {
        return nil
    }
    ...
    sess.Title = title
    _ = sess.Save()
    ...
    h(sess.ID, title)
}
```

The snapshot is a pointer; subsequent reads of `sess.Title` /
`sess.Records` and writes to `sess.Title` go to the pointee. That's
acceptable here because the pointee (the `memory.Session` struct) is
treated as owned by the title-gen goroutine during this call — no
other goroutine should be mutating that specific `*Session` during the
brief title-gen window. The race the detector caught is purely on the
`a.session` *field* (the pointer slot), not on the `Session` struct's
internal fields.

## 3. Why this is enough

- The race detector report is specifically about the `a.session` field
  being read and written concurrently. Snapshotting the pointer under
  the lock eliminates the data race on that field.
- The `Session` struct's internal mutability is a separate concern.
  Today `LoadSession` swaps `a.session` to a new pointer atomically
  (under `a.mu`), so a title goroutine that snapshotted the previous
  `*Session` continues to operate on the older session safely. This
  matches the existing implicit ownership model.
- A full audit of every `a.session` read/write across the package is
  out of scope for this ADR — the detector is the canonical signal,
  and this fix addresses exactly what it reported. If subsequent
  `-race` runs surface other pairs, they get their own ADR or are
  folded in as straight bug fixes.

## 4. Alternatives considered

- **Replace `a.session` with an `atomic.Pointer[memory.Session]`.**
  Cleaner for the pointer slot, but invasive (every reader/writer in
  the package needs updating) and changes the contract subtly (an
  atomic pointer permits readers to race with writers in a way the
  mutex-protected version doesn't). Rejected as over-engineering for
  one detected race pair.
- **Remove the `agentLoop` nil-init.** Would force tests to always
  call `LoadSession` first. Tempting but a wider refactor touching
  many tests; deferred. The defensive init is harmless once
  synchronised.
- **Lock `a.mu` around every `a.session` access in
  `generateTitleIfNeeded`.** The function calls `a.backend.Chat` —
  an LLM HTTP call — between the title-check and the title-write,
  which can take seconds. Holding `a.mu` across that call would block
  every other agent operation. The snapshot pattern keeps the lock
  window short.

## 5. Tests

The race is already caught by existing `-race` runs (e.g.
`TestQueuedSend_OverwriteMostRecentWins`). Post-fix:

- `go test -race -count=1 ./internal/agent/` (full package) — no
  WARNING: DATA RACE in the relevant goroutine pair.
- The two test fixes for issue #13 (deadline + barrier pattern)
  stay; they address a separate test-design race.

No new test is added: the detector is the regression mechanism, and
reverting the fix re-introduces the warning immediately.

## 6. Compatibility

- Pure internal synchronisation change; no API, on-disk, or
  user-visible behaviour difference.
- No new dependency or build tag.

## 7. Out of scope

- Wholesale `a.session` ownership model refactor (atomic pointer,
  RCU, copy-on-write, …).
- The `memory.Session` struct's own thread-safety contract — that
  is the responsibility of the package that owns it.
- Other potential races in `a.session` accessors that the detector
  has not yet flagged; they will be addressed when (and if) the
  detector surfaces them.
