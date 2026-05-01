# Sandbox Image Build — Design Document

> Date: 2026-05-01
> Status: Draft — pending implementation
> Scope: Embed a recommended sandbox image (Python + CJK
> fonts + analysis stack) and a build mechanism so users
> can build it from the Settings dialog. Sandbox tools
> become available only when **both** the image is built
> and `Sandbox.Enabled` is true.

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

### 3.1 Embedded Dockerfile bundle

New package `internal/sandbox/imagebuild`:

```
internal/sandbox/imagebuild/
├── bundle.go         // go:embed all:bundle/*  + version const
└── bundle/
    ├── Dockerfile
    └── matplotlibrc
```

`bundle.go`:

```go
package imagebuild

import "embed"

//go:embed all:bundle
var Bundle embed.FS

// BundleVersion is bumped whenever any file under bundle/
// changes in a way that should invalidate previously-built
// images. The image tag is "shell-agent-v2-sandbox:<BundleVersion>"
// so a new BundleVersion forces a fresh build the next time
// the user clicks Build.
const BundleVersion = "0.1"

// CanonicalTag is the image tag that "Build" produces and
// that ImageReady() expects to find on the engine.
const CanonicalTag = "shell-agent-v2-sandbox:" + BundleVersion
```

`bundle/Dockerfile`:

```dockerfile
FROM python:3.12-slim

# CJK fonts — matplotlib renders Japanese / Chinese /
# Korean labels as □□ without these. fonts-noto-cjk
# ships full coverage; -extra adds variants.
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

# matplotlib config: ship a default rcParams that puts
# Noto Sans CJK JP in the font fallback chain so charts
# with Japanese labels render correctly even when the
# user / model doesn't explicitly set rcParams.
COPY matplotlibrc /etc/matplotlib/matplotlibrc
ENV MATPLOTLIBRC=/etc/matplotlib/matplotlibrc

WORKDIR /work
```

`bundle/matplotlibrc`:

```
font.family: sans-serif
font.sans-serif: DejaVu Sans, Noto Sans CJK JP, Arial, Liberation Sans
axes.unicode_minus: False
```

### 3.2 `Engine.BuildImage`

Add to the `Engine` interface (`internal/sandbox/engine.go`):

```go
// BuildImage builds the image described by the embedded
// imagebuild bundle and tags it as `tag`. Stdout/stderr
// from the engine are streamed line by line to onLine
// (nil-safe).
//
// The build context is a temp dir where the bundle's
// files are written; cleaned up on return.
//
// Concurrent calls are serialised inside the engine: the
// second call blocks until the first finishes.
BuildImage(ctx context.Context, tag string, onLine func(string)) error
```

`cliEngine.BuildImage` implementation:

```go
func (e *cliEngine) BuildImage(ctx context.Context, tag string, onLine func(string)) error {
    e.buildMu.Lock()
    defer e.buildMu.Unlock()

    bin, ok := e.Detect()
    if !ok {
        return ErrEngineNotAvailable
    }

    // Materialise the embedded bundle into a temp dir.
    workDir, err := os.MkdirTemp("", "shell-agent-v2-build-*")
    if err != nil {
        return fmt.Errorf("temp dir: %w", err)
    }
    defer os.RemoveAll(workDir)

    if err := writeBundle(workDir); err != nil {
        return fmt.Errorf("write bundle: %w", err)
    }

    // podman/docker build accepts -f and a context dir.
    cmd := exec.CommandContext(ctx, bin, "build", "-t", tag, workDir)
    stdout, _ := cmd.StdoutPipe()
    stderr, _ := cmd.StderrPipe()

    if err := cmd.Start(); err != nil {
        return fmt.Errorf("start: %w", err)
    }

    // Two scanners so stdout / stderr both reach onLine
    // in arrival order.
    var wg sync.WaitGroup
    wg.Add(2)
    go streamLines(stdout, onLine, &wg)
    go streamLines(stderr, onLine, &wg)
    wg.Wait()

    if err := cmd.Wait(); err != nil {
        return fmt.Errorf("build: %w", err)
    }
    return nil
}
```

`writeBundle` walks `imagebuild.Bundle` (`embed.FS`) and
copies entries under the workDir, preserving the filename.

### 3.3 `Engine.ImageReady`

```go
// ImageReady reports whether `tag` exists locally on the
// engine. Used by the agent to decide whether to expose
// the sandbox tools.
ImageReady(ctx context.Context, tag string) (bool, error)
```

Implementation: `podman image exists tag` (already used
internally by `ensureImage`); returns `(true, nil)` on
exit 0, `(false, nil)` on the documented "image missing"
exit, `(false, err)` for actual engine errors.

### 3.4 Agent / sandbox enablement gating

`agent.maybeStartSandbox`:

```go
if !cfg.Sandbox.Enabled {
    return
}
eng, err := sandbox.NewCLI(...)
if err != nil { ... }

// New: image-ready check.
ready, err := eng.ImageReady(ctx, cfg.Sandbox.Image)
if err != nil {
    logger.Info("sandbox: image readiness probe failed: %v — tools will stay hidden", err)
    return
}
if !ready {
    logger.Info("sandbox: image %q not built; click 'Build sandbox image' in Settings — tools will stay hidden", cfg.Sandbox.Image)
    return
}
a.sandbox = eng
// startup sweep, etc.
```

`buildToolDefs` already gates on `a.sandbox != nil`. The
gate now also implicitly requires image readiness.

`SaveSettings` calls `RestartSandbox` when `cfg.Sandbox`
diffs (already in v0.1.18). After a build, the bindings
also need to call `RestartSandbox` so the agent re-checks
readiness. We add `Bindings.RebindSandbox()` (private to
the build flow) that triggers the same path without
requiring a config diff.

### 3.5 Bindings + Wails events

```go
// BuildSandboxImage starts a build of the canonical
// embedded image. Returns immediately; progress is sent
// via Wails events:
//   - "sandbox:build:line"  payload {line string}
//   - "sandbox:build:done"  payload {tag string, error string}
// Only one build at a time per process; concurrent calls
// receive ErrBuildInProgress.
func (b *Bindings) BuildSandboxImage() error
```

Single-build invariant via `b.buildMu sync.Mutex` +
`b.buildInFlight bool`. `defer` clears the flag.

```go
// SandboxImageStatus is a snapshot for the Settings UI.
type SandboxImageStatus struct {
    Tag      string `json:"tag"`        // cfg.Sandbox.Image
    Ready    bool   `json:"ready"`      // engine has the tag
    Building bool   `json:"building"`   // a build is in flight
    Recommended string `json:"recommended"` // imagebuild.CanonicalTag
}

func (b *Bindings) GetSandboxImageStatus() SandboxImageStatus
```

The Settings dialog reads `GetSandboxImageStatus()` once
on open and after each build event.

### 3.6 Settings UI

Inside the existing **Sandbox** subsection (`SettingsDialog.tsx`):

```
Sandbox (experimental)
[ ] Enable container sandbox  ← disabled until image is ready
    Hint: tools register only when the image below is built AND
    this checkbox is on.

Image: [shell-agent-v2-sandbox:0.1                    ▾]
       Status: ✓ Ready  /  ⚠ Not built — click Build below  /  ⏳ Building…
       [ Build recommended image ]   [ View build log ]

[ Engine, Network, CPU, Memory, Timeout — same as today ]
```

Behaviour:

- "Build recommended image" calls
  `Bindings.BuildSandboxImage()`, opens a modal that
  streams `sandbox:build:line` events into a scrollback.
  On `sandbox:build:done`, the modal shows result + close.
- The Image input is editable. When the user types a
  custom tag, status check re-fires; if that tag isn't
  on the engine, status shows "Not built" and Build
  remains disabled (Build only targets the canonical
  embedded tag, not arbitrary user tags).
- The "Enable" checkbox stays disabled when status is
  "Not built". A tooltip explains why.
- A small note: "Building takes a few minutes — apt-get
  + pip install. The build runs on your local
  podman/docker."

### 3.7 Default config

Bump `cfg.Sandbox.Image` default in `config.Default()` from
`python:3.12-slim` to `imagebuild.CanonicalTag` (currently
`shell-agent-v2-sandbox:0.1`). Existing user configs are
preserved by JSON-load (the Image field is set) — only fresh
installs see the new default.

For users on `python:3.12-slim`, the readiness check still
passes if they've pulled it; sandbox tools register; no
forced migration. The Settings UI "Build" button only ever
produces the canonical tag, so users who want the bundled
fonts must set the Image field to the canonical tag.

### 3.8 Versioning & rebuild trigger

`imagebuild.BundleVersion` is bumped whenever the
Dockerfile or matplotlibrc changes. The image tag includes
the version, so after an app update the previous build is
still on the engine but the new canonical tag is "Not
built" until the user clicks Build again. Dialog text:
"A newer image is available — rebuild?".

## 4. Touched files

| File | Change |
|---|---|
| `internal/sandbox/imagebuild/bundle.go` | new — embed.FS + version + canonical tag |
| `internal/sandbox/imagebuild/bundle/Dockerfile` | new |
| `internal/sandbox/imagebuild/bundle/matplotlibrc` | new |
| `internal/sandbox/engine.go` | add `BuildImage`, `ImageReady` to Engine interface |
| `internal/sandbox/cli.go` | implement `BuildImage`, `ImageReady`, `buildMu` |
| `internal/agent/agent.go` | `maybeStartSandbox` gates on `eng.ImageReady` |
| `bindings.go` | `BuildSandboxImage`, `GetSandboxImageStatus`, build-flow lock |
| `internal/config/config.go` | default `Sandbox.Image` → `imagebuild.CanonicalTag` |
| `frontend/src/types.ts` | `SandboxImageStatus` interface |
| `frontend/src/dialogs/SettingsDialog.tsx` | Image status row + Build button + log modal |
| `frontend/src/App.tsx` (or new component) | build-log overlay listening on `sandbox:build:*` |

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

Two commits, in order:

1. **feat(sandbox): embed image build bundle + Engine.BuildImage / ImageReady.** Pure backend; no UI surface yet. Tests assert build-arg shape and concurrent-build lock. The default `Sandbox.Image` stays at `python:3.12-slim` for now.
2. **feat(ui): Settings sandbox image build flow + readiness gating.** Wires `BuildSandboxImage` / `GetSandboxImageStatus` into the dialog, adds the build-log modal, swaps the default `Sandbox.Image` to the canonical bundled tag, and changes `maybeStartSandbox` to gate on `ImageReady`.

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
