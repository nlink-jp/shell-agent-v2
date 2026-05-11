# Sandbox UID mapping (v0.4.3)

## 1. Symptom

A user on a corporate-managed macOS account reported the
following error when the sandbox tried to start a container:

```
Error: ensure container: sandbox: start container:
podman: Error: crun: setresuid to `202594884`: Invalid argument:
OCI runtime error
```

The sandbox image built without error; the failure was strictly
at container start time.

## 2. Root cause

`buildRunArgs` in `internal/sandbox/cli.go` was passing the host
UID directly to `--user`:

```go
"--user", strconv.Itoa(os.Getuid()),
```

On a vanilla Linux/macOS machine this works because the user's
UID (e.g., 501) sits inside the rootless subuid range (typically
65,536 IDs starting somewhere below `2^31`). On a corporate
macOS account whose UID is mapped from Active Directory or LDAP,
`os.Getuid()` returns a **very large** number (e.g., 202,594,884)
that is *outside* any mappable range in the rootless Podman user
namespace. `crun` then fails its `setresuid()` syscall with
`EINVAL` because the requested UID is not present in
`/proc/self/uid_map`.

The user reported the bug specifically against `podman`. Docker
uses a different rootless model (and on macOS Docker Desktop is
typically rootful inside its own VM with a file-sharing layer),
so the symptom does not reproduce there.

## 3. Fix

For the `podman` engine, replace the bare `--user` flag with an
explicit user-namespace remap that keeps the host UID *somewhere
small* inside the container:

```go
args = append(args,
    "--userns", "keep-id:uid=1000,gid=1000",
    "--user", "1000:1000",
)
```

What this does:

- `--userns=keep-id:uid=1000,gid=1000` — instead of mapping the
  host UID one-to-one inside the container's user namespace
  (which is what default `keep-id` would do, and which fails for
  the same reason as before), this remaps the **host UID to UID
  1000 inside the container**. The reverse mapping is set up
  symmetrically, so files written to the bind-mounted `/work`
  directory still appear on the host as the host user — exactly
  the property the original `--user $(id -u)` was trying to
  preserve.
- `--user 1000:1000` — actually run the container process as
  UID/GID 1000. Without this the image's `USER` directive
  decides; for `python:3.12-slim` that's root, which would
  defeat the defence-in-depth posture from the v0.2.0 sandbox
  design (history/sandbox-execution.md §9-6).

The docker engine path is unchanged: `--user $(id -u)` keeps
working on rootful Docker Desktop and rootless Linux Docker,
where the rootless mapping (when it exists) is wired
differently.

Engine selection happens at run time via the existing
`usePodmanUserns(binary)` helper, parallel to `useSELinuxRelabel`
— a one-liner basename match on `podman`.

## 4. Compatibility

- **Podman version**: `--userns=keep-id:uid=N,gid=N` was added
  in **Podman 4.3** (November 2022). All currently maintained
  Podman releases support it. Podman ≤ 4.2 will reject the flag
  at run time; the agent surfaces the `podman` error verbatim
  via the existing `sandbox: start container:` wrap.
- **File ownership in `/work`**: unchanged in observable
  behaviour. Files created inside the container as UID 1000
  appear on the host as the host user, because `keep-id`
  installs the inverse mapping.
- **Security posture**: unchanged. Container processes still
  run as a non-root user (UID 1000 inside the namespace) rather
  than as root — the v0.2.0 sandbox security note continues to
  hold.

## 5. Tests

`internal/sandbox/cli_test.go` adds:

- `TestBuildRunArgs_PodmanRemapsHostUID` — asserts that the
  podman path emits both `--userns=keep-id:uid=1000,gid=1000`
  and `--user 1000:1000`, **and never** passes the host UID
  through. Guards against an accidental regression to the
  `strconv.Itoa(os.Getuid())` form.
- `TestBuildRunArgs_NonPodmanPassesHostUID` — asserts that
  the docker path keeps the historical `--user $(id -u)`
  behaviour and never emits `--userns`. The two paths are
  exclusive.
- `TestUsePodmanUserns` — basename-match table for the engine
  detection helper (case-insensitive, full path tolerant).

Updated existing tests for the new `buildRunArgs` arity.

## 6. Out of scope

- Selecting a UID other than 1000 (no use case, would only
  add a config knob nobody asked for).
- Detecting the host UID and switching strategies dynamically
  (the keep-id path works for *every* host UID, including
  small ones — there is no value in branching).
- Documenting the broader Podman rootless model (history note
  is the place for that, not this fix note).
