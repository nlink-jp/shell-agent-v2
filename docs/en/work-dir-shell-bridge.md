# Shell Tool ↔ /work Bridge — Design Document

> Date: 2026-05-02
> Status: Proposed for the next release
> Scope: Let host-side shell tools share a workspace directory
> with the sandbox container so artefacts produced by either side
> can be passed to the other and into objstore. Add a built-in
> `register-object` tool so the shell-only flow (sandbox disabled)
> can also surface its outputs in the chat UI.

## 1. Background

The session work directory at
`<DataDir>/sessions/<sessionID>/work/` is currently a sandbox-only
concept. The container bind-mounts it at `/work`; the
`sandbox-register-object` tool reads files from there and registers
them into objstore so the chat UI can render them as
`object:<ID>`.

Host-side shell tools (registered via the toolcall package) have
no equivalent. The example `examples/generate-image.sh` writes to
`/tmp/shell-agent-images/` (a global path), and even when the
write succeeds, the resulting image:

- Doesn't appear in the Data panel's `/work` listing
- Can't be referenced as `object:<ID>` in chat
- Isn't reachable from sandbox-side tools
- Returns just a JSON status string the LLM has no way to render

The user observed during v0.1.24 verification that the work
directory is physically the same on host and inside the container
(it's a bind mount, not a copy), so a shell tool writing into it
on the host immediately becomes a sandbox-side `/work/<file>` —
**no data movement needed**. The piece missing is (a) telling the
shell tool where to write, and (b) a sandbox-free way to register
the result into objstore.

## 2. Goals / Non-goals

### Goals

1. Shell tools learn the host path of the session work directory
   via an environment variable (`SHELL_AGENT_WORK_DIR`).
2. The work directory is created at session load, not as a side
   effect of starting a sandbox container — so shell-only users
   (sandbox disabled) get the same convention.
3. A built-in tool `register-object` mirrors
   `sandbox-register-object`'s effect (read a file from work dir,
   register into objstore, return `object:<ID>`) but doesn't
   require the sandbox to be running.
4. `generate-image.sh` is rewritten to use the new flow and serve
   as the canonical example of "shell tool → work dir → register-object
   → chat-visible image".

### Non-goals

- **No deprecation of `sandbox-register-object`.** It keeps
  working unchanged. Both tools end up doing the same thing
  (reading from the same physical directory), but the prefix
  conveys context to the LLM and avoids breaking existing user
  configurations.
- **No automatic objstore registration of every shell-tool
  output.** That requires a stdout-marker protocol or filesystem
  watcher, both of which are larger designs. For now, the LLM is
  responsible for following up `generate-image` with `register-object`.
- **No additional env vars in this round.** Only `SHELL_AGENT_WORK_DIR`.
  If `SHELL_AGENT_SESSION_ID` etc. become useful later, add then.
- **No global "delete work-dir on session close"** — the dir
  persists like the rest of the session data.

## 3. Detailed design

### 3.1 Env var injection (`internal/toolcall`)

`Execute` gains an optional `workDir` argument. When non-empty,
it sets `cmd.Env = append(os.Environ(), "SHELL_AGENT_WORK_DIR="+workDir)`
on the spawned subprocess. When empty, behaviour is unchanged
(no env modification, parent environment inherited as before).

Signature options considered:

- (a) Add a third positional arg → breaks every test caller.
- (b) Change to a single `Options` struct → larger refactor, more
  callers touched.
- (c) Add a variadic options pattern → small, additive, easy to
  extend.

Choosing **(c)**:

```go
type ExecOption func(*execConfig)

func WithWorkDir(path string) ExecOption {
    return func(c *execConfig) { c.workDir = path }
}

func Execute(ctx context.Context, tool *Tool, argsJSON string, opts ...ExecOption) (string, error) {
    cfg := execConfig{}
    for _, o := range opts {
        o(&cfg)
    }
    ...
    if cfg.workDir != "" {
        cmd.Env = append(os.Environ(), "SHELL_AGENT_WORK_DIR="+cfg.workDir)
    }
    ...
}
```

Existing `Execute(ctx, tool, args)` call sites compile and behave
exactly as before; the new agent code passes
`toolcall.WithWorkDir(workDir)`.

### 3.2 Work dir creation at session load

Today the work dir is created inside `sandbox.cliEngine.EnsureContainer`,
keyed on the sandbox flow. Shell-only users (sandbox disabled)
never call into that path, so `$SHELL_AGENT_WORK_DIR` would point
at a non-existent directory.

Fix: agent creates the dir on session load, regardless of sandbox
state.

```go
// agent.LoadSession (or a helper invoked from there)
workDir := filepath.Join(memory.SessionDir(s.ID), "work")
if err := os.MkdirAll(workDir, 0700); err != nil {
    logger.Error("agent: workdir create: %v", err)
}
```

The sandbox's existing `EnsureContainer` MkdirAll is left as-is
(idempotent, no harm).

### 3.3 New built-in tool: `register-object`

Mirrors `sandbox-register-object` but lives in the analysis-source
group (always exposed once a session is active, no sandbox
dependency).

**Tool definition** (in `analysisTools`):

```go
{
    Name: "register-object",
    Description: "Register a file already present in the session work directory ($SHELL_AGENT_WORK_DIR — same physical path that the sandbox sees as /work) into the central object store, returning an object:<ID> reference the chat can render. Use this to surface artefacts produced by shell tools (e.g. generate-image) — write to $SHELL_AGENT_WORK_DIR from the shell tool, then call this with the same filename. For artefacts produced by sandbox-run-python / sandbox-run-shell, prefer sandbox-register-object; both end up doing the same thing physically since /work is the same host directory.",
    Parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "path": map[string]any{
                "type": "string",
                "description": "Path inside the work directory. Relative paths only; '..' traversal is rejected. e.g. 'sunset.png'",
            },
            "name": map[string]any{
                "type": "string",
                "description": "Human-readable name for the object (shown in the Data panel)",
            },
            "type": map[string]any{
                "type": "string",
                "enum": []string{"image", "blob", "report"},
                "description": "Object type. If omitted, inferred from the file's MIME (image/* → image, text/markdown → report, otherwise blob).",
            },
        },
        "required": []string{"path", "name"},
    },
}
```

**Implementation** (`internal/agent/tools.go`):

Mirror `toolSandboxRegisterObject` (`sandbox_tools.go`) but bypass
the sandbox engine — read the host file directly from
`memory.SessionDir(a.session.ID) + "/work" + path`.

Path validation reuses the same logic the sandbox path already
uses (`safeWorkPath` / equivalent): refuse absolute paths, refuse
`..` traversal, and use `os.Lstat` to refuse symlinks (security
hardening 2 H14 pattern).

**MITL default**: `analysisToolMITLDefault["register-object"] = false`.
Reasoning: the user can already drag and drop files into the
chat (which lands them in objstore unconditionally), and
`sandbox-register-object` is also commonly run with MITL
disabled. Per-tool toggle is available if a user wants it ON.

**Category in `ListTools`**: `"write"` (mutates objstore state)
so the Settings UI shows it grouped with other write-side tools.

### 3.4 generate-image.sh rewrite

```sh
#!/bin/bash
# @tool: generate-image
# @description: Generate an image from a text prompt using Vertex AI Gemini. Writes to $SHELL_AGENT_WORK_DIR; follow up with register-object to make the image appear in chat.
# @param: prompt string "Image generation prompt describing what to create"
# @param: filename string "Output filename (e.g. sunset.png)"
# @category: execute
# @timeout: 120
#
# REQUIRES: gem-image (https://github.com/nlink-jp/gem-image)
# REQUIRES: Vertex AI credentials (gcloud auth application-default login)

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('prompt',''))" 2>/dev/null)
FILENAME=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('filename','generated.png'))" 2>/dev/null)

if [ -z "$PROMPT" ]; then
  echo '{"error": "prompt is required"}'
  exit 1
fi

if [ -z "$SHELL_AGENT_WORK_DIR" ]; then
  echo '{"error": "SHELL_AGENT_WORK_DIR not set — this tool requires shell-agent-v2 ≥ v0.1.25"}'
  exit 1
fi

export PATH="$HOME/bin:$HOME/go/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"

OUTPUT_PATH="$SHELL_AGENT_WORK_DIR/$FILENAME"

if ! gem-image -p "$PROMPT" -o "$OUTPUT_PATH" --force 2>/dev/null; then
  echo "{\"error\": \"Image generation failed\"}"
  exit 1
fi

# Success path: status + filename only. No `next_step`, no
# absolute path. See §6 risks "Imperative next_step" and
# "Absolute paths in tool output".
python3 -c "import json; print(json.dumps({'status':'success','filename':'$FILENAME'}))"
```

The follow-up convention ("then call register-object with
path=<filename>") lives in the tool's `@description` (re-injected
to the LLM every round) — not in per-call output. See §6 for why.

### 3.5 Documentation

- `docs/en/object-storage.md` — add a sub-section "Shell tool
  artefacts" describing the work-dir bridge.
- `README.md` — under "Shell script Tool Calling" mention the
  work-dir env var and `register-object`.
- `AGENTS.md` — gotcha entry.

## 4. Touched files

| File | Change |
|---|---|
| `internal/toolcall/toolcall.go` | `ExecOption` / `WithWorkDir`; `Execute` accepts variadic options |
| `internal/toolcall/toolcall_test.go` | Test that env var is set when `WithWorkDir` is passed; not set when absent |
| `internal/agent/agent.go` | Pass `toolcall.WithWorkDir(...)` from the shell-tool dispatcher branch; create work dir on session load |
| `internal/agent/tools.go` | New `register-object` definition + `toolRegisterObject` impl; entry in `analysisToolMITLDefault` |
| `internal/agent/agent.go` | `register-object` entry in `ListTools()` (Settings UI) + new branch in dispatcher |
| `internal/bundled/tools/examples/generate-image.sh` | Rewrite per §3.4 |
| `docs/en/work-dir-shell-bridge.md` / `docs/ja/work-dir-shell-bridge.ja.md` | this design doc |
| `docs/en/object-storage.md` (+ JA mirror) | sub-section pointing here |
| `CHANGELOG.md` | `[Unreleased]` Added entry |
| `AGENTS.md` | Gotcha + ListTools mention |
| `README.md` / `README.ja.md` | Shell-tool section |

## 5. Backward compatibility

| Surface | Pre-change | Post-change | Compat |
|---|---|---|---|
| Existing shell tools that ignore env vars | unchanged | `SHELL_AGENT_WORK_DIR` exposed but ignored | ✅ |
| `toolcall.Execute(ctx, tool, args)` callers | 3 args | 3 args + variadic options | ✅ identical for old callers |
| `sandbox-register-object` | works | works (no change) | ✅ |
| Old `generate-image.sh` users | wrote to `/tmp/shell-agent-images/`, no chat surface | new behaviour: writes to work dir, hint to register | ⚠ output JSON changes (status only → status + next_step). Considered safe because (a) it never worked correctly anyway, and (b) the script is in `examples/`, an opt-in copy, not auto-installed. Users who pulled the old version into their `tools/` dir keep their copy; they upgrade by re-pulling from `examples/`. |
| `tools.go` `analysisTools` def list | 11 tools | 12 tools (`+register-object`) | ✅ additive |
| `MITLOverrides` JSON keys | unchanged | unchanged | ✅ |

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| LLM forgets to call `register-object` after `generate-image` succeeds | The follow-up convention is documented in `generate-image`'s `@description`, which is re-injected to the LLM every round (no per-call hint needed — see anti-pattern below). |
| Shell tool writes a file to work dir but with a path that escapes the dir (`../../etc/passwd`) | `register-object` validation rejects absolute paths, `..` traversal, and symlink leafs (mirroring `sandbox-register-object`'s `safeWorkPath`) |
| Two tools (`register-object` and `sandbox-register-object`) confuse the LLM about which to choose | Both descriptions cross-reference each other and explain the equivalence; in practice the LLM picks the matching prefix based on which side wrote the file (shell tool → register-object; sandbox tool → sandbox-register-object). Either tool would actually work since they share the same physical directory. |
| Work dir not yet created when first shell tool runs (e.g. session that has never opened sandbox or hit register-object) | Agent's `LoadSession` creates the dir up front |

### 6.1 Anti-pattern: imperative `next_step` in tool output

The first cut of `generate-image.sh` returned an explicit
`next_step` instruction in its JSON output:

```json
{"status":"success","filename":"sunset.png",
 "next_step":"Call register-object with path=\"sunset.png\" name=\"...\" to surface in chat"}
```

This **narrows the LLM's planning horizon to the immediate next
action and can derail multi-step user plans.** Observed during
v0.1.25 verification: the user asked for a 4-step plan (analyze →
prompt → generate-3-images → write-report). The agent executed
generate→register pairs cleanly for all three images, but treated
each `next_step` as the round's primary objective; once the third
register completed and there was no further `next_step` in the
tool output, the agent loop ended **without writing the report**.
The user had to prompt again to recover the originally-stated goal.

**Rule**: shell-tool output should carry **state** (what
happened, where the artefact lives), never **instructions**
(what to do next). The follow-up convention belongs in the
tool's `@description`, which is re-injected to the LLM every
round and competes with the LLM's plan on equal footing rather
than overriding it.

### 6.2 Anti-pattern: absolute paths in tool output

The same first cut also considered emitting
`"work_dir_path": "/Users/magi/Library/.../work/sunset.png"`
(absolute host path) so the LLM could reference the artefact
directly. Rejected because:

- The agent's other tools that legitimately accept absolute
  paths (notably `load-data` — MITL-gated by design so the user
  consciously approves each ingest from arbitrary host paths)
  become attractive targets if the LLM has a curated list of
  full host paths in its context. A prompt-injected LLM could
  set up a load-data MITL prompt where the user, glancing past,
  approves a path that turned out to be `~/.ssh/id_rsa` rather
  than the expected artefact.
- Absolute host paths are also exfiltration material in their
  own right (LLM context → log → screenshare → ...).

**Rule**: shell-tool output should expose **relative
filenames** only. Tools that need absolute paths (`load-data`)
must keep them MITL-gated.

### 6.3 Other bundled tools updated

`write-note.sh` was originally `/tmp/${FILENAME}` — global
filesystem pollution, no per-session scoping, files
inaccessible to sandbox tools or the Data panel. Rewritten to
write into `$SHELL_AGENT_WORK_DIR` like `generate-image.sh`,
following the same output contract (filename-only, no
imperative instructions). The MITL gate stays — a write tool
is still write-category — but the blast radius is now scoped
to the session work dir instead of leaking into the host's
`/tmp`.

The other bundled tools (`weather`, `get-location`,
`list-files`, `file-info`, `preview-file`) only emit data to
stdout and don't write artefacts, so they need no work-dir
treatment.

## 7. Out of scope

- Stdout marker protocol for auto-registration (e.g.
  `OBJSTORE-REGISTER: sunset.png image`). Bigger design; defer.
- Filesystem watcher for `/work`. Same.
- A `register-object` UI button in the Data panel. The Data
  panel can already export and delete objects; promoting a
  `/work` file to an object via UI is a separate UX question
  (sub-section "Promote to object" perhaps), defer.
- A `SHELL_AGENT_SESSION_ID` env var. Add when needed.
- Cleaning up stale `/work` files on session delete. Already
  handled by `DeleteSessionDir`.
