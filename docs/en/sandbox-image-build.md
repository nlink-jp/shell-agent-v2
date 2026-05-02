# Sandbox Image Build — Design Document

> Date: 2026-05-01
> Status: Shipped in v0.1.18 — Revision 3 (Dockerfile editor
> in Settings, content-addressed image tags, built-images
> library with Active / Use / Delete actions, dedicated
> Sandbox Settings tab). Sandbox tools register only when
> **all three** of: (a) an active image is selected,
> (b) the active image is present on the local engine,
> (c) `Sandbox.Enabled` is true.
>
> Revision history:
>   - r1: separate "Image tag" config field plus an
>     opaque "Build recommended image" button. Fragile —
>     implicit coupling between user-settable Image and
>     always-recommended Build target; arbitrary
>     `Image=alpine:3.20` would still build the embedded
>     recipe; build-time package installs assume `apt`.
>   - r2: expose the Dockerfile text directly; Build uses
>     exactly what's in the textarea; tag is
>     content-addressed.
>   - r3 (current): keep r2's Dockerfile-text approach,
>     **add a built-images library** (list with Active /
>     Use / Delete actions) so users can keep multiple
>     recipes coexisting and switch between them without
>     rebuilding, and **move the whole sandbox UI into
>     its own Settings tab** since the section grew past
>     the "single dropdown" budget.

## 1. Problem

The default sandbox image (`python:3.12-slim`) ships with
no CJK fonts. matplotlib charts with Japanese labels
render as `□□`. User workaround today: switch to a
heavier image, run `apt-get install fonts-noto-cjk`
manually, or accept mojibake. None of these are
discoverable.

A second related issue: the sandbox can be silently
"enabled but not configured" — `Sandbox.Enabled = true`
with a default image, no fonts, no analysis libs
pre-installed. The user expects rich Python output and
gets a bare interpreter.

We want the recommended path to be one click: **Build
image** → **Enable sandbox** → tools register. No
manual Dockerfile editing, no remembering apt-get
recipes.

## 2. Goals / Non-goals

### Goals

1. Ship a Dockerfile inside the binary that produces a
   sandbox image with CJK fonts and the common analysis
   stack (pandas, numpy, matplotlib, scipy, scikit-learn).
2. Provide a Settings UI button that builds this image
   on the user's local podman/docker. Build progress
   streams to a log overlay so the user can see what's
   happening (apt-get + pip install take minutes).
3. Sandbox tools (the eight `sandbox-*` ToolDefs) are
   only registered when **the configured image exists
   on the local engine** AND `Sandbox.Enabled` is true.
   A user who toggles Enabled without building gets a
   clear prompt; the tools stay hidden.
4. The configured `Sandbox.Image` default points at the
   bundled image's canonical tag, so the happy path is
   "build → enable → it just works."
5. Power users who set `Sandbox.Image` to anything else
   (`python:3.12-slim`, a custom registry image, etc.)
   keep working as today — the readiness check is
   "does this tag exist on the engine," not "did *we*
   build it."

### Non-goals

- **No auto-build at startup.** Build is explicit, user-
  triggered. Avoids surprise multi-minute waits and
  silent network use.
- **No image push to a registry.** Local-only.
- **No multi-arch matrix.** The build runs on whatever
  the local engine supports (typically arm64 on Apple
  Silicon, amd64 on Linux).
- **No live-edit of the embedded Dockerfile from the
  UI.** Power users can clone the repo and edit; our
  embedded copy is the supported path.
- **No incremental layer reuse logic beyond podman/docker
  defaults.** Layer caching gives this for free.
- **No image GC.** Tags live forever on the user's
  engine until they remove them manually.

## 3. Detailed design

### 3.1 Embedded recommended Dockerfile

New package `internal/sandbox/imagebuild`:

```go
package imagebuild

import (
    "crypto/sha256"
    "encoding/hex"
)

// TagPrefix is the namespace under which all sandbox
// images live. ListImages filters by this prefix; user
// builds get tags of the form "<TagPrefix>:<sha[:12]>".
const TagPrefix = "shell-agent-v2-sandbox"

// RecommendedDockerfile is the default Dockerfile body
// shown in the Settings textarea on first open and
// restored by the "Reset to recommended" button. Self-
// contained: matplotlibrc is created inline so we don't
// need a multi-file build context.
const RecommendedDockerfile = `FROM python:3.12-slim

# CJK fonts — matplotlib renders Japanese / Chinese /
# Korean labels as □□ without these.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        fonts-noto-cjk \
        fonts-noto-cjk-extra \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Common analysis libs.
RUN pip install --no-cache-dir \
        pandas \
        numpy \
        matplotlib \
        scipy \
        scikit-learn

# matplotlib rcParams: put Noto Sans CJK JP into the font
# fallback chain so charts with Japanese labels render
# correctly even when the script doesn't set rcParams.
RUN mkdir -p /etc/matplotlib && \
    printf 'font.family: sans-serif\nfont.sans-serif: DejaVu Sans, Noto Sans CJK JP, Arial, Liberation Sans\naxes.unicode_minus: False\n' > /etc/matplotlib/matplotlibrc
ENV MATPLOTLIBRC=/etc/matplotlib/matplotlibrc

WORKDIR /work
`

// TagFor returns the content-addressed image tag for a
// given Dockerfile body. Edits to the Dockerfile change
// the tag, so a previous build of a different recipe stays
// on the engine under its own tag.
func TagFor(dockerfile string) string {
    sum := sha256.Sum256([]byte(dockerfile))
    return TagPrefix + ":" + hex.EncodeToString(sum[:6])
}
```

The `bundle/` directory and `embed.FS` are gone — the
Dockerfile is a single string. The image build label
that ListImages filters on is also `TagPrefix`.

### 3.2 `Engine.BuildImage`

Engine method takes the Dockerfile **body** instead of a
hard-coded bundle reference, and returns the tag it
produced (so the caller doesn't have to re-compute it):

```go
// BuildImage writes dockerfile to a temp dir, runs
// `<engine> build -t <tag> .`, streams stdout/stderr
// to onLine. Returns the computed tag (TagFor(dockerfile))
// and any build error. Concurrent calls are serialised
// inside the engine.
BuildImage(ctx context.Context, dockerfile string, onLine func(string)) (tag string, err error)
```

Implementation writes `dockerfile` to
`<tempdir>/Dockerfile` and runs
`<bin> build -t <TagFor(...)> --label <TagPrefix>=1 .`.
The label lets ListImages enumerate ours.

### 3.3 `Engine.ImageReady` / `ListImages` / `RemoveImage`

```go
// ImageReady reports whether `tag` exists locally.
ImageReady(ctx context.Context, tag string) (bool, error)

// ListImages enumerates locally-built sandbox images
// (those with the TagPrefix label). Sorted newest-first.
ListImages(ctx context.Context) ([]ImageInfo, error)

// RemoveImage deletes the image with the given tag.
// No-op (not error) when the tag doesn't exist.
RemoveImage(ctx context.Context, tag string) error
```

`ImageInfo`:

```go
type ImageInfo struct {
    Tag       string    // shell-agent-v2-sandbox:<sha>
    Created   time.Time
    SizeBytes int64
}
```

`ListImages` runs
`<bin> images --filter label=<TagPrefix>=1 --format '{{.Repository}}:{{.Tag}}|{{.CreatedAt}}|{{.Size}}'`
and parses; `RemoveImage` runs `<bin> image rm <tag>` and
ignores "no such image" errors.

### 3.4 Agent / sandbox enablement gating

`agent.maybeStartSandbox` now needs an "active" image —
the one the user picked from the library. The active image
lives in `cfg.Sandbox.Image` (already there). Gate logic:

```go
if !cfg.Sandbox.Enabled {
    return
}
if cfg.Sandbox.Image == "" {
    logger.Info("sandbox: no active image selected; pick one in Settings")
    return
}
eng, err := sandbox.NewCLI(...)
if err != nil { ... }

ready, _ := eng.ImageReady(ctx, cfg.Sandbox.Image)
if !ready {
    logger.Info("sandbox: active image %q is not present locally", cfg.Sandbox.Image)
    return
}
a.sandbox = eng
```

After a successful Build the bindings call
`agent.RestartSandbox` so the gate re-evaluates
immediately.

### 3.5 Bindings

```go
// BuildSandboxImage builds an image from the Dockerfile
// the user has in their config (or imagebuild.RecommendedDockerfile
// when empty). Returns immediately; progress streams via
// Wails events. The completion event carries the
// computed tag.
//
//   "sandbox:build:line"  payload {"line": <stdout|stderr>}
//   "sandbox:build:done"  payload {"tag": <tag>, "error": <"" on success>}
//
// Concurrent calls return ErrBuildInProgress. On success,
// the new tag is set as cfg.Sandbox.Image, the config is
// saved, and the agent's sandbox is restarted so the
// readiness gate re-evaluates.
func (b *Bindings) BuildSandboxImage() error

// ListSandboxImages returns the locally-built sandbox
// images, newest first.
func (b *Bindings) ListSandboxImages() []SandboxImageInfo

// SetActiveSandboxImage sets cfg.Sandbox.Image to tag
// (which must be one of ListSandboxImages's results),
// saves config, and triggers RestartSandbox.
func (b *Bindings) SetActiveSandboxImage(tag string) error

// RemoveSandboxImage deletes the image with the given
// tag from the engine. If it was the active image,
// cfg.Sandbox.Image is cleared and the sandbox tools
// unregister on the next RestartSandbox.
func (b *Bindings) RemoveSandboxImage(tag string) error

// SandboxImageStatus snapshot for the dialog.
type SandboxImageStatus struct {
    ActiveTag         string                `json:"active_tag"`
    ActiveReady       bool                  `json:"active_ready"`
    Building          bool                  `json:"building"`
    RecommendedDockerfile string            `json:"recommended_dockerfile"`
    CurrentDockerfile     string            `json:"current_dockerfile"` // cfg or recommended
    Images            []SandboxImageInfo    `json:"images"`
}
```

### 3.6 Settings UI — separate "Sandbox" tab

The Settings dialog grows a 4th tab. The current Sandbox
subsection moves out of "General" into a dedicated
**Sandbox** tab.

```
[ General | Tools | MCP | Sandbox ]

Sandbox tab
───────────────────────────────────────────────────────
Built images
  ● shell-agent-v2-sandbox:a1b2c3d4e5f6   [Active]   [Delete]
  ○ shell-agent-v2-sandbox:7890abcdef12   [Use]     [Delete]
  ○ shell-agent-v2-sandbox:fedcba987654   [Use]     [Delete]
  (none yet — build one below)

[x] Enable container sandbox
    (greyed out until an active image exists & is ready)

Dockerfile
┌──────────────────────────────────────────────────────┐
│ FROM python:3.12-slim                                  │
│ RUN apt-get update && \                               │
│     apt-get install -y --no-install-recommends ...    │
│ ...                                                   │
└──────────────────────────────────────────────────────┘
[Reset to recommended]   [Build]    Status: ⏳ Building…

Engine: [auto ▾]
[ ] Allow network egress (default off)
CPU limit:        [2     ]
Memory limit:     [1g    ]
Per-call timeout: [60    ]
```

Behaviour:

- The textarea initial value comes from
  `cfg.Sandbox.Dockerfile` (empty → recommended).
- "Reset to recommended" overwrites the textarea with
  `imagebuild.RecommendedDockerfile`.
- "Build" sends the textarea's *current* value to
  `BuildSandboxImage` (which also persists it to
  `cfg.Sandbox.Dockerfile`).
- Build-log overlay (modal) streams progress; same
  pattern as r2.
- After build success, the new tag appears in the list
  and is auto-selected as Active.
- "Use" radio sets active; "Delete" removes after a
  one-step confirm.
- "Enable container sandbox" disabled when active is
  empty or not ready.

### 3.7 Default config

`cfg.Sandbox.Image` default is **empty** on fresh
installs. The user picks an image after their first
Build. Existing configs (with a tag set) keep the value;
ImageReady probes it; if the engine has it, sandbox
tools register, no migration needed.

`cfg.Sandbox.Dockerfile string` is added (empty = use
recommended). Persisted so the textarea retains user
edits across sessions.

### 3.8 Versioning

The image tag is content-addressed
(`sha256[:12]` of the Dockerfile body). Edits to the
recommended Dockerfile in a future release produce a new
hash; the user's existing-active tag survives until they
explicitly Delete it. No `BundleVersion` constant — the
hash itself is the version.

## 4. Touched files

| File | Change |
|---|---|
| `internal/sandbox/imagebuild/bundle.go` | RecommendedDockerfile const, TagPrefix, TagFor() — replaces the embed.FS approach from r1/r2 |
| `internal/sandbox/imagebuild/bundle/` | **deleted** — single string replaces the multi-file build context |
| `internal/sandbox/engine.go` | Engine gains BuildImage(ctx, dockerfile, onLine) (tag, error) + ListImages + RemoveImage; ImageReady stays |
| `internal/sandbox/cli.go` | implementations + buildMu |
| `internal/agent/agent.go` | maybeStartSandbox gates on cfg.Sandbox.Image being non-empty AND ImageReady |
| `internal/config/config.go` | new field `Sandbox.Dockerfile string`; default `Sandbox.Image` becomes "" (no auto-build, user picks after first Build) |
| `bindings.go` | BuildSandboxImage / ListSandboxImages / SetActiveSandboxImage / RemoveSandboxImage / GetSandboxImageStatus |
| `frontend/src/types.ts` | SandboxImageStatus, SandboxImageInfo |
| `frontend/src/dialogs/SettingsDialog.tsx` | new Sandbox tab (4th); built-images list + Dockerfile textarea + Reset/Build + log overlay; old General-tab Sandbox section removed |

## 5. Test plan

### Unit
- `imagebuild.Bundle` walks all files; non-empty.
- `cliEngine.ImageReady` returns true/false based on
  `podman image exists` exit code (mocked via fake exec
  pipe — current sandbox tests already do this for run
  args).
- `cliEngine.BuildImage` writes the bundle to the temp
  dir (assert files exist) and invokes `podman build`
  with the right argv (mock the exec, verify args).
- `Bindings.BuildSandboxImage` rejects concurrent calls
  with `ErrBuildInProgress`.

### Integration (require podman/docker on PATH)
- `TestIntegration_BuildAndUseImage`: build the canonical
  image, then `EnsureContainer` against it, then
  `Exec("python", "import matplotlib;...")`. Confirm a
  Japanese label round-trips through matplotlib without
  fallback warnings (search for the warning string in
  stderr; absent = pass).
- Re-running the test reuses the existing image (build
  is fast on second run).

### Manual
- Click Build with no image: progress streams, image
  appears, status flips to Ready, Enable becomes
  clickable.
- Click Build twice quickly: second click sees "build in
  progress", first build completes normally.
- Edit Image to a tag that doesn't exist: status flips
  to Not Built, Enable greys out, Build button still
  builds the canonical tag (clear that the button only
  targets one tag).
- Rebuild after `BundleVersion` bump in the next release:
  fresh status shows the new tag as Not Built; old tag
  still on the engine.

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Build takes 3-10 minutes on first install; user thinks app is hung | Stream stdout to a visible log overlay; show progress lines (apt step, pip step). Cancel button calls `ctx.Cancel()`. |
| Network down during build | Build fails with stderr surfaced; user sees and can retry. Fail does NOT change Status. |
| Existing users with custom `Sandbox.Image` get an unexpected default-bump on upgrade | Default-bump only affects fresh installs (config field is set on first save). Existing configs untouched. |
| Disk fills up with tags from version bumps | Documented in CHANGELOG when BundleVersion bumps. User can `podman image rm shell-agent-v2-sandbox:<old>` manually. We don't auto-delete. |
| User clicks Build with podman machine not running on macOS | Engine surfaces a clear error from the build process; the existing `Detect()` reports availability. Hint "Start podman machine first" in error path. |
| Bundle FS evolves but BundleVersion not bumped | Same image tag with different content. Mitigation: doc-comment on `BundleVersion` const explicitly says "bump on any bundle/* change". CI can compute a hash and assert const matches in a `//go:generate` step (later). |

## 7. Phasing

The r1 Phase-1 commit (`da5307f`) shipped an `embed.FS`
bundle and `CanonicalTag` constant that this revision
deletes. We treat the pivot as a single compensating
commit (no need to revert da5307f separately — git history
shows the evolution):

1. **refactor(sandbox): pivot image build to user-editable Dockerfile + image library.** Drops the bundle FS in favour of `RecommendedDockerfile` const + `TagFor()` + `TagPrefix`. Adds `ListImages` / `RemoveImage`. Adds `cfg.Sandbox.Dockerfile`. Removes `cfg.Sandbox.Image` default.
2. **feat(ui): dedicated Sandbox tab with library + Dockerfile editor.** Adds the 4th Settings tab; built-images list with Active radio + Delete; Dockerfile textarea + Reset + Build; log overlay. Old Sandbox section in General tab removed.

v0.1.19 release after Phase 2.

## 8. Out of scope

- Any auto-build behaviour at launch.
- Pulling the canonical image from a registry instead of
  building locally.
- A "remove image" button (use `podman image rm` for now).
- A configurable Dockerfile path (the embedded one is the
  supported recipe; advanced users edit the repo or set
  `Sandbox.Image` to their own pre-built tag).
- Linux / Windows-specific build tweaks beyond what
  podman/docker does for free.
