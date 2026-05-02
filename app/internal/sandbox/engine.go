// Package sandbox runs LLM-supplied shell or Python code inside a
// per-session container managed by `podman` or `docker`. Each session
// gets its own long-running container with `/work` mounted from a
// session-scoped host directory; files written there persist across
// calls within the session and are isolated between sessions.
//
// Phase 1: this package is dormant — it has no callers in the agent
// loop. The agent integration (Settings flag, six sandbox-* tools,
// MITL routing) lands in Phase 2.
//
// Design: docs/en/sandbox-execution.md
package sandbox

import (
	"context"
	"errors"
	"time"
)

// ErrEngineNotAvailable is returned by NewCLI when neither podman nor
// docker is on PATH (and the config didn't pin a specific binary).
var ErrEngineNotAvailable = errors.New("sandbox: no container engine available (podman or docker)")

// ErrContainerNotRunning is returned by Exec when the session has no
// active container (e.g. Stop was already called).
var ErrContainerNotRunning = errors.New("sandbox: session container is not running")

// Engine is the per-session sandbox abstraction. One implementation
// (cliEngine) wraps the user's `podman` or `docker` CLI; the
// interface keeps it swappable for testing and for hypothetical
// future engines (e.g. a remote runner).
type Engine interface {
	// Detect returns the resolved engine binary name ("podman" or
	// "docker") and whether it is usable on this host.
	Detect() (binary string, ok bool)

	// EnsureContainer creates and starts the per-session container if
	// it isn't already running. Idempotent. The host-side work
	// directory at WorkDir(sessionID) is created as a side effect.
	EnsureContainer(ctx context.Context, sessionID string) error

	// Exec runs the given code inside the session's container.
	// Returns combined output, exit code, timeout flag, and any
	// startup error. The container must already be running (Exec
	// does NOT auto-start; callers do EnsureContainer first).
	Exec(ctx context.Context, sessionID string, args ExecArgs) (*ExecResult, error)

	// Stop tears down the session's container and forgets any cached
	// state for that session. Safe to call when the container is not
	// running.
	Stop(ctx context.Context, sessionID string) error

	// StopAll reaps every container labelled as belonging to this
	// app — used at shutdown to clean up across all sessions, and
	// at startup to sweep up containers from a previous launch that
	// crashed.
	StopAll(ctx context.Context) error

	// WorkDir returns the host-side absolute path of the session's
	// /work mount. The directory is created on EnsureContainer.
	WorkDir(sessionID string) string

	// Info returns introspection data about the session's runtime
	// (engine version, image, python version, installed pip
	// packages, network policy, resource limits, /work listing).
	// Safe to call before Exec — will EnsureContainer internally.
	Info(ctx context.Context, sessionID string) (*Info, error)

	// ImageReady reports whether `tag` exists on the local engine.
	// The agent uses this to decide whether to register the
	// sandbox-* tools — sandbox is only enabled when the
	// configured Sandbox.Image is actually present.
	ImageReady(ctx context.Context, tag string) (bool, error)

	// BuildImage writes `dockerfile` to a temp dir and runs
	// `<engine> build -t <tag> .`. The tag is computed from
	// the Dockerfile content (imagebuild.TagFor) and returned
	// to the caller so the UI / config can record it. Engine
	// stdout/stderr stream line-by-line to onLine (nil-safe).
	// Concurrent calls are serialised inside the engine.
	BuildImage(ctx context.Context, dockerfile string, onLine func(string)) (tag string, err error)

	// ListImages returns the locally-built sandbox images
	// (those carrying the imagebuild.TagPrefix label),
	// newest-first.
	ListImages(ctx context.Context) ([]ImageInfo, error)

	// RemoveImage deletes the image with the given tag. A
	// missing tag is a no-op (returns nil) so the UI can
	// click Delete idempotently.
	RemoveImage(ctx context.Context, tag string) error
}

// ImageInfo describes one locally-built sandbox image.
type ImageInfo struct {
	Tag       string
	Created   time.Time
	SizeBytes int64
}

// ExecArgs is the input to Exec.
type ExecArgs struct {
	// Language selects the interpreter inside the container.
	// Supported values: "shell" (runs via /bin/sh -c) and "python"
	// (runs via python3 -c).
	Language string

	// Code is the source to execute. For shell, a complete command
	// line; for python, complete Python source.
	Code string

	// Timeout caps the execution. Zero means use the engine's
	// default. On timeout the result has TimedOut=true and a
	// non-zero ExitCode.
	Timeout time.Duration
}

// ExecResult is the outcome of Exec.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// Info is the runtime introspection payload returned by Info().
type Info struct {
	Engine        string // "podman" or "docker"
	EngineVersion string
	Image         string
	PythonVersion string
	Network       bool
	CPULimit      string
	MemoryLimit   string
	TimeoutSec    int

	// PipPackages is the freeze-format output of `pip list`,
	// already capped to a reasonable line count.
	PipPackages []string

	// WorkFiles enumerates /work entries (recursive, capped at 50,
	// sorted by mtime descending).
	WorkFiles []FileInfo
}

// FileInfo is one /work entry.
type FileInfo struct {
	Path  string // relative to /work
	Size  int64
	MTime time.Time
}

// Config is the per-package config that NewCLI consumes. It mirrors
// the public config.SandboxConfig that Phase 2 will introduce; we
// keep an internal copy here so the package has no dependency on
// internal/config (Phase 2 maps fields explicitly).
type Config struct {
	Engine         string // "auto" | "podman" | "docker"
	Image          string
	Network        bool
	CPULimit       string
	MemoryLimit    string
	TimeoutSeconds int

	// MaxOutputBytes caps each of stdout / stderr captured per Exec
	// call. Beyond the cap, further bytes are dropped and a marker is
	// appended. 0 → applyDefaults sets it to 8 MiB. The cap exists to
	// stop a single LLM-issued command from OOMing the app via
	// `cat /dev/zero` or similar (security-hardening-2.md C3).
	MaxOutputBytes int

	// SessionsDir is the host-side root under which per-session work
	// directories live. The full path for a session is:
	//   <SessionsDir>/<sessionID>/work
	SessionsDir string
}

// DefaultMaxOutputBytes is the default cap for sandbox stdout/stderr
// when SandboxConfig.MaxOutputBytes is left at zero.
const DefaultMaxOutputBytes = 8 * 1024 * 1024

// applyDefaults fills empty fields with the documented defaults.
func (c *Config) applyDefaults() {
	if c.Engine == "" {
		c.Engine = "auto"
	}
	if c.Image == "" {
		c.Image = "python:3.12-slim"
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 60
	}
	if c.CPULimit == "" {
		c.CPULimit = "2"
	}
	if c.MemoryLimit == "" {
		c.MemoryLimit = "1g"
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
}
