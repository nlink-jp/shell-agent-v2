# Sandbox Execution — Design Document

> Date: 2026-04-28
> Status: Draft for review
> Scope: New `internal/sandbox/` package, new `sandbox-*` tools,
>   config additions, agent loop integration

## 1. Problem & Motivation

Shell tools (`list-files`, `weather`, etc.) execute *on the user's
host machine* with the user's privileges. That model is fine for
small bundled scripts the user vets, but it's a poor fit for letting
the LLM run arbitrary shell or Python:

- Side effects on the user's filesystem.
- Inconsistent runtime — different OS, different installed packages,
  different python versions across users.
- No way to bound resource usage (a misbehaving LLM-generated loop
  can eat the host).
- No clean snapshot to discard if a step misfires.

A **container-based sandbox** addresses all four:

- Filesystem access scoped to a per-session work directory (mounted).
- Reproducible runtime (single image, defined Linux user-space).
- CPU / memory / time limits enforced by the engine.
- Discardable state — `podman rm -f` resets a session's environment.

## 2. Goals

- LLM can run shell commands and Python in a sandbox per session.
- Side effects (files, env mutations) survive within a session, are
  isolated between sessions, and disappear when the session is
  deleted.
- Works with either Podman or Docker, auto-detected.
- Disabled by default; opt-in per install via Settings.
- MITL approval required for every execution (same category as other
  write/execute tools).

## 3. Non-Goals

- Multi-host orchestration / Kubernetes.
- GPU / hardware passthrough.
- Persistent images / commit-and-publish.
- Network egress beyond a configurable allow/deny knob (default off).
- Streaming stdout into the chat UI in real time (Phase 2).

## 4. Architecture

```
agent loop
  │
  └─ tool: sandbox-run-shell / sandbox-run-python / sandbox-write-file
        sandbox-copy-object / sandbox-register-object / sandbox-info
       │
       ├─ MITL approval (category: execute)
       │
       └─ sandbox.Engine
            │
            ├─ ensureContainer(sessionID)
            │    │
            │    └─ first call:
            │         podman/docker run -d --name shell-agent-v2-<sid>
            │              --workdir /work
            │              --volume <session>/work:/work
            │              --user $UID  --network none (default)
            │              --memory 1g  --cpus 2
            │              <image>  sleep infinity
            │
            └─ exec(sessionID, lang, code) -> ExecResult
                 │
                 └─ podman/docker exec -i <container>
                       --workdir /work
                       <interpreter> -c <code>
```

Lifecycle hooks:

- Agent.New → no container action.
- First `run-*` call for a session → `ensureContainer`.
- Session delete → `Engine.Stop(sessionID)` → container removed.
- App shutdown → `Engine.StopAll()`; orphaned containers are reaped
  on next launch by listing with the project label.

## 5. Engine Abstraction

```go
package sandbox

type Engine interface {
    // Detect returns the resolved engine ("podman" or "docker") and
    // whether it is available on PATH.
    Detect() (string, bool)

    // EnsureContainer creates and starts the container for sessionID
    // if it isn't already running. Idempotent.
    EnsureContainer(ctx context.Context, sessionID string) error

    // Exec runs the given command inside the session's container and
    // returns combined output, exit code, and any startup error.
    Exec(ctx context.Context, sessionID string, args ExecArgs) (*ExecResult, error)

    // Stop tears down the session's container.
    Stop(ctx context.Context, sessionID string) error

    // StopAll reaps every container belonging to this app.
    StopAll(ctx context.Context) error
}

type ExecArgs struct {
    Language string   // "shell" | "python"
    Code     string
    Timeout  time.Duration
}

type ExecResult struct {
    Stdout   string
    Stderr   string
    ExitCode int
    TimedOut bool
}
```

A single concrete implementation `cliEngine` wraps the chosen CLI
(`podman` or `docker`); the surface is identical aside from minor
flag differences, both shelled out via `os/exec`.

### Container naming and labelling

```
name:   shell-agent-v2-<sessionID>
label:  app=shell-agent-v2
```

`StopAll` filters by the label so we only ever touch containers
this app created — `podman ps -a -q --filter label=app=shell-agent-v2`.

## 6. Per-Session Work Directory

Layout under the existing app data dir:

```
~/Library/Application Support/shell-agent-v2/
  sessions/
    <sessionID>/
      chat.json
      summaries.json
      work/                       ← mounted at /work in the container
        <files the LLM creates>
```

The directory is created on first `EnsureContainer` call (lazy) and
removed by the existing `DeleteSessionDir` cascade (we extend it to
cover the work subtree).

Mount: `--volume <abs-path>:/work:Z` (Z label keeps SELinux happy on
Linux hosts; harmless on macOS).

The LLM's `WORKDIR` is `/work`, so relative paths in
`sandbox-run-shell` / `sandbox-run-python` resolve there. Files
written here survive the container
restart but go away with session deletion.

## 7. Tool Surface (LLM-facing)

All sandbox tools share the `sandbox-` prefix so the model sees them
as a coherent suite that share state through the session's `/work`
directory. Five tools total: two execute, three move data across the
sandbox boundary.

### `sandbox-run-shell`

```json
{
  "name": "sandbox-run-shell",
  "description": "Execute a shell command inside this session's sandbox container. Files in /work persist across calls within the session and are isolated between sessions. Side effects do not affect the host. Use for filesystem operations, package installs (pip), and orchestrating subprocesses.",
  "parameters": {
    "type": "object",
    "properties": {
      "command": {"type": "string", "description": "Shell command to execute."}
    },
    "required": ["command"]
  }
}
```

Default timeout: `cfg.Sandbox.TimeoutSeconds` (60 s).

### `sandbox-run-python`

```json
{
  "name": "sandbox-run-python",
  "description": "Execute Python code inside this session's sandbox container. Working directory is /work; files there persist across calls. Each call is a fresh interpreter, but the filesystem and any installed packages persist within the session.",
  "parameters": {
    "type": "object",
    "properties": {
      "code": {"type": "string", "description": "Python source to execute."}
    },
    "required": ["code"]
  }
}
```

Implementation: shell pipes `code` to `python3 -c "$(cat)"` inside the
container, so we don't need to escape quotes ourselves.

### `sandbox-write-file`

LLM → sandbox. Lets the model put text content (CSV, JSON, source,
Dockerfile, …) into `/work/<path>` without escaping it through a
heredoc inside `sandbox-run-shell`.

```json
{
  "name": "sandbox-write-file",
  "description": "Write text content to /work/<path> inside this session's sandbox. Use to seed the sandbox with data the LLM has already produced (CSVs, source files, configs) without escaping it through run-shell heredocs. Path must be relative to /work; parent directories are created if missing. Existing files are overwritten.",
  "parameters": {
    "type": "object",
    "properties": {
      "path":    {"type": "string", "description": "Relative path under /work, e.g. 'data.csv' or 'src/script.py'."},
      "content": {"type": "string", "description": "Text content to write."}
    },
    "required": ["path", "content"]
  }
}
```

Implementation: write through the host-side mounted directory (no
container hop needed). Reject `..` traversal and absolute paths.

### `sandbox-copy-object`

objstore → sandbox. Copies an object from the central repository
into the sandbox so the LLM can analyse user-uploaded images, prior
reports, or stored blobs with `sandbox-run-python` (PIL, pandas, …).

```json
{
  "name": "sandbox-copy-object",
  "description": "Copy a stored object (image / blob / report) from the central object repository into /work/<path> inside this session's sandbox. Use to bring user-uploaded images or earlier reports into the sandbox for analysis. Use list-objects to find a valid object_id.",
  "parameters": {
    "type": "object",
    "properties": {
      "object_id": {"type": "string", "description": "Object ID from list-objects."},
      "path":      {"type": "string", "description": "Destination path under /work. Defaults to the object's orig_name when omitted."}
    },
    "required": ["object_id"]
  }
}
```

### `sandbox-register-object`

sandbox → objstore. Promotes a sandbox-produced file into the
central object repository and returns its object ID, so the LLM can
reference it in reports as `![alt](object:<id>)`. Closes the analyse →
visualise → report loop without manual file shuffling.

```json
{
  "name": "sandbox-register-object",
  "description": "Register a file from /work (typically an output from sandbox-run-python — chart, generated CSV, etc.) into the central object repository. Returns the object ID, which can be referenced in reports as ![alt](object:ID).",
  "parameters": {
    "type": "object",
    "properties": {
      "path":      {"type": "string", "description": "Source path under /work."},
      "type":      {"type": "string", "description": "image | blob | report. Defaults to inference from MIME."},
      "name":      {"type": "string", "description": "Friendly name (orig_name) shown in the Objects panel; defaults to filename."}
    },
    "required": ["path"]
  }
}
```

Implementation reads the file via the host-side mounted directory,
detects MIME if `type` not given, then `objects.Store(reader, type,
mime, name, sessionID)`.

### `sandbox-info`

Introspection. Returns a compact summary the LLM can call once early
in a session to learn what's available without wasting turns
probing with shell commands. Cheap (cached after the first hit per
container).

```json
{
  "name": "sandbox-info",
  "description": "Return a description of this session's sandbox: engine, image, Python version, key pre-installed packages, network policy, resource limits, and the contents of /work (path, size, mtime). Use this to discover the runtime before running code.",
  "parameters": {"type": "object", "properties": {}}
}
```

Result format (LLM-facing text):

```
engine:    podman 4.9.4
image:     python:3.12-slim
python:    3.12.5
network:   off
limits:    cpus=2 memory=1g timeout=60s

packages (pip):
  pandas==2.2.2
  matplotlib==3.9.0
  numpy==1.26.4
  ...

work directory (/work):
  data.csv        12.4 KB  2026-04-28 22:45
  src/script.py    1.1 KB  2026-04-28 22:46
```

Implementation:

- Engine + version: `podman --version` (cached at `Detect`).
- Image, network, limits: from `cfg.Sandbox`.
- Python + packages: `python3 -c "import sys; print(sys.version)"` +
  `pip list --format=freeze`, executed inside the container on first
  call and cached on the engine struct keyed by sessionID.
- Work directory: walked on the host side via the mounted
  directory, no container hop needed; truncated to ~50 entries.

Cache invalidation: cleared on `Stop(sessionID)` and after any
successful `sandbox-run-shell` (because `pip install` may have run);
`run-python` does not invalidate (it can't add packages without
calling pip via shell).

### Reading files back

A dedicated `sandbox-read-file` tool is intentionally **not**
provided — `sandbox-run-shell` `cat /work/<path>` already covers it,
and the existing per-backend `MaxToolResultTokens` truncation
applies. Adding a second tool would only duplicate that path.

### Result format

For execute tools the LLM-facing string concatenates stdout, stderr
(if non-empty), and a footer:

```
<stdout>

[stderr]
<stderr>

[exit: 0]
```

`exit: <n>` always present; truncation cap matches the per-backend
`MaxToolResultTokens` (reusing the existing render-time truncation
in contextbuild / chat).

For data-movement tools the result is a one-line confirmation, e.g.
`wrote 1.2 KB to /work/data.csv` or `registered as object 67ecaa…`.

## 8. Configuration

Adds to `config.Config`:

```go
type SandboxConfig struct {
    Enabled        bool   `json:"enabled"`               // default false
    Engine         string `json:"engine"`                // "auto" | "podman" | "docker"
    Image          string `json:"image"`                 // default "python:3.12-slim"
    Network        bool   `json:"network"`               // default false (no egress)
    CPULimit       string `json:"cpu_limit,omitempty"`   // default "2"
    MemoryLimit    string `json:"memory_limit,omitempty"`// default "1g"
    TimeoutSeconds int    `json:"timeout_seconds"`       // default 60
}
```

Settings UI: new "Sandbox" section under General.

## 9. Security Model

Defence in depth, in order from coarsest to finest:

1. **Off by default.** `Enabled=false` means `run-shell` /
   `run-python` are not exposed in the tool list at all.
2. **MITL.** Every call goes through the existing approval prompt.
   Category is `execute`, so the user always sees the code/command
   before it runs (consistent with shell-tool MITL).
3. **Container isolation.** Code can't see the host filesystem,
   apart from `/work` which is mounted from the session's directory.
4. **No-network default.** `--network=none` on the run command. A
   user who turns network on accepts the broader risk; we do NOT
   wire up DNS allow-lists or a proxy.
5. **Resource limits.** `--memory`, `--cpus`, plus the per-call
   timeout. `TimedOut=true` is reported to the LLM.
6. **Non-root user.** Container runs as the host's UID via `--user`.
   Image must support running as a non-root UID; for `python:3.12-slim`
   this works because the working dir is the mounted volume.

What we do **not** protect against:

- A compromise of `podman` / `docker` itself.
- Side-channel attacks across containers (the sandbox is for
  user-asked-LLM-to-run-this isolation, not adversarial separation).
- Information leakage through the work directory across sessions
  *if the user manually copies files between session work dirs*.

## 10. Configuration Resolution & Defaults

```go
func (c *Config) SandboxConfig() SandboxConfig {
    s := c.Sandbox
    if s.Engine == "" { s.Engine = "auto" }
    if s.Image == "" { s.Image = "python:3.12-slim" }
    if s.TimeoutSeconds == 0 { s.TimeoutSeconds = 60 }
    if s.CPULimit == "" { s.CPULimit = "2" }
    if s.MemoryLimit == "" { s.MemoryLimit = "1g" }
    return s
}
```

`engine="auto"`: prefer `podman` if on PATH, else `docker`. If
neither, the agent emits a single startup warning and `Enabled` is
forced to false at runtime regardless of config.

## 11. Agent Loop Integration

- `agent.New` constructs the sandbox engine if `cfg.Sandbox.Enabled`
  and either binary is on PATH.
- `agent.buildToolDefs` appends `run-shell` / `run-python` only when
  the engine is non-nil.
- `agent.executeTool` dispatches `run-shell` / `run-python` cases to
  `engine.Exec`.
- `agent.LoadSession` does *not* eagerly start a container; first
  tool use triggers `EnsureContainer`.
- `agent.deleteSession` (after `objstore.DeleteBySession`) calls
  `engine.Stop(sessionID)` and `os.RemoveAll(workDir)`.
- `bindings.shutdown` calls `engine.StopAll(ctx)`.

## 12. Verification

### Unit

- `cliEngine` against `/bin/sh -c …` stubs (`Detect`,
  argument-building helpers, label filter parsing).
- Render of the LLM-facing result string given various stdout/stderr
  combinations and exit codes.

### Integration (skipped if `podman`/`docker` absent)

- `EnsureContainer` then `Exec` round-trip with a trivial command.
- File persistence between two `Exec` calls (write → read).
- File invisibility across two distinct session IDs.
- `Stop` actually removes the container; subsequent `Exec` fails.
- Timeout: `sleep 5` with `Timeout: 1s` returns `TimedOut: true`
  and a non-zero exit.

### Manual

- Smoke test in dev mode: enable sandbox, ask LLM to compute
  something with `run-python`, then read the file with `run-shell`.
- Verify network=false really does block: `curl` from inside fails.
- Session delete cleans up: `podman ps` shows no orphan.

## 13. Phased Rollout

| Phase | Scope | Default behaviour |
|------|-------|-------------------|
| 1 | `internal/sandbox` package + tests, no agent integration | none (dormant) |
| 2 | Agent / config / Settings UI hooks behind `Enabled` flag | opt-in via Settings |
| 3 | Auto-image pull check on first use, friendly error if missing | unchanged |
| 4 | Real-time stdout streaming to the chat (status indicator) | nice-to-have |

Phase 1+2 are the first patch release goal.

## 14. Open Questions

1. **Single image vs configurable.** Recommendation: one good default
   (`python:3.12-slim` with `pip install pandas matplotlib jupyter`
   pre-baked) shipped as a separate small image. Configurable by
   the user but with an "are you sure" hint in Settings.
2. **Pre-pull on first launch?** A `podman pull` on Sandbox enable
   removes a ~3 s lag from the first tool call but pulls a few
   hundred megabytes of image without explicit consent. Recommendation:
   defer until first use; surface the wait via the existing
   `tool-event` indicator.
3. **Should `run-shell` and `run-python` appear in every session, or
   only in "enabled" sessions?** Recommendation: all sessions when
   the global flag is on; the per-session work dir + container is
   transparent to the user.
4. **Credential injection.** Some Python work needs API keys (OpenAI,
   GCP). Out of scope for v1; the user can paste them inline in
   their prompt or set them in `/work/.env` and source it. We do not
   forward host env vars by default.
5. **macOS Docker Desktop performance.** Mount IO is slower than
   Linux. Acceptable for the use cases described; documented.
6. **Concurrent calls.** The agent loop is single-threaded for tool
   calls per session, so we don't need a per-session call lock.
   `EnsureContainer` is idempotent and safe under concurrent calls
   from different sessions because the container name is keyed by
   session ID.

## 15. Touchpoints Summary

| File | Change |
|------|--------|
| `internal/sandbox/engine.go` | New: interface + cliEngine |
| `internal/sandbox/cli.go`    | New: podman/docker shell-out |
| `internal/sandbox/result.go` | New: ExecResult formatting |
| `internal/sandbox/*_test.go` | New: unit + integration |
| `internal/config/config.go`  | Add SandboxConfig + defaults |
| `internal/agent/agent.go`    | Wire engine, dispatch, lifecycle |
| `bindings.go`                | Settings surface, StopAll on shutdown |
| `frontend/src/App.tsx`       | Settings → General → Sandbox section |
| `docs/{en,ja}/sandbox-execution{,.ja}.md` | This doc |

## 16. Summary

A new `internal/sandbox` package owns a per-session container
managed via the user's `podman` or `docker`. Two LLM tools
(`run-shell` / `run-python`) execute code inside, with files
persisting in a session-scoped `work/` directory mounted at `/work`.
Off by default, MITL-gated, network-off by default, resource-bounded.
Lifecycle is lazy (first tool use creates the container) and tied to
the session (delete kills the container and the work dir). Phased
rollout keeps the package dormant until both unit and integration
tests are green.
