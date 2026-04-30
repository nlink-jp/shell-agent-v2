package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// containerLabel is attached to every container we create so StopAll
// can scope its reap by label and never touch foreign containers.
const containerLabel = "app=shell-agent-v2"

// containerNamePrefix prefixes the per-session container name —
// `shell-agent-v2-<sessionID>`.
const containerNamePrefix = "shell-agent-v2-"

// cliEngine is the production Engine that shells out to podman or docker.
type cliEngine struct {
	cfg Config

	// resolved binary, "" until Detect succeeds.
	mu     sync.Mutex
	binary string
}

// NewCLI constructs an Engine. Returns ErrEngineNotAvailable when
// the requested engine cannot be located.
func NewCLI(cfg Config) (Engine, error) {
	cfg.applyDefaults()
	e := &cliEngine{cfg: cfg}
	if _, ok := e.Detect(); !ok {
		return nil, ErrEngineNotAvailable
	}
	return e, nil
}

// --- Detection -----------------------------------------------------

func (e *cliEngine) Detect() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.binary != "" {
		return e.binary, true
	}
	bin, ok := resolveEngine(e.cfg.Engine)
	if !ok {
		return "", false
	}
	e.binary = bin
	return bin, true
}

// resolveEngine picks a binary based on the requested engine name.
// "auto" prefers podman, falling back to docker. Explicit names
// require that exact binary be on PATH.
func resolveEngine(req string) (string, bool) {
	switch req {
	case "podman":
		if _, err := exec.LookPath("podman"); err == nil {
			return "podman", true
		}
		return "", false
	case "docker":
		if _, err := exec.LookPath("docker"); err == nil {
			return "docker", true
		}
		return "", false
	default: // "auto" or unknown
		if _, err := exec.LookPath("podman"); err == nil {
			return "podman", true
		}
		if _, err := exec.LookPath("docker"); err == nil {
			return "docker", true
		}
		return "", false
	}
}

// --- Container lifecycle -------------------------------------------

func (e *cliEngine) EnsureContainer(ctx context.Context, sessionID string) error {
	bin, ok := e.Detect()
	if !ok {
		return ErrEngineNotAvailable
	}
	if err := os.MkdirAll(e.WorkDir(sessionID), 0700); err != nil {
		return fmt.Errorf("sandbox: create work dir: %w", err)
	}
	name := containerName(sessionID)

	// Already running?
	if running, err := e.containerRunning(ctx, name); err != nil {
		return err
	} else if running {
		return nil
	}

	// Container exists but stopped? Remove first so we get a fresh
	// state with the latest config.
	if exists, err := e.containerExists(ctx, name); err != nil {
		return err
	} else if exists {
		_ = runCommand(ctx, bin, "rm", "-f", name)
	}

	if err := e.ensureImage(ctx); err != nil {
		return err
	}

	args := buildRunArgs(e.cfg, name, e.WorkDir(sessionID))
	if err := runCommand(ctx, bin, args...); err != nil {
		return fmt.Errorf("sandbox: start container: %w", err)
	}
	return nil
}

// ensureImage pulls e.cfg.Image when it's not already present
// locally. Idempotent and a no-op when the image exists.
func (e *cliEngine) ensureImage(ctx context.Context) error {
	bin, _ := e.Detect()
	out, err := runCommandOutput(ctx, bin, "image", "exists", e.cfg.Image)
	_ = out
	if err == nil {
		return nil
	}
	// `image exists` returns non-zero (no stderr) when missing — fall
	// through to pull. Distinguish other errors by re-running with
	// stderr surfaced.
	if pullErr := runCommand(ctx, bin, "pull", e.cfg.Image); pullErr != nil {
		return fmt.Errorf("sandbox: pull image %s: %w", e.cfg.Image, pullErr)
	}
	return nil
}

// buildRunArgs builds the `podman run` / `docker run` argv (without
// the binary prefix). Exposed for unit testing.
func buildRunArgs(cfg Config, name, workDir string) []string {
	args := []string{
		"run", "-d",
		"--name", name,
		"--label", containerLabel,
		"--workdir", "/work",
		"--volume", workDir + ":/work:Z",
		"--user", strconv.Itoa(os.Getuid()),
	}
	if !cfg.Network {
		args = append(args, "--network", "none")
	}
	if cfg.CPULimit != "" {
		args = append(args, "--cpus", cfg.CPULimit)
	}
	if cfg.MemoryLimit != "" {
		args = append(args, "--memory", cfg.MemoryLimit)
	}
	args = append(args, cfg.Image, "sleep", "infinity")
	return args
}

func (e *cliEngine) Stop(ctx context.Context, sessionID string) error {
	bin, ok := e.Detect()
	if !ok {
		return nil // nothing to stop
	}
	_ = runCommand(ctx, bin, "rm", "-f", containerName(sessionID))
	return nil
}

func (e *cliEngine) StopAll(ctx context.Context) error {
	bin, ok := e.Detect()
	if !ok {
		return nil
	}
	out, err := runCommandOutput(ctx, bin, "ps", "-a", "-q", "--filter", "label="+containerLabel)
	if err != nil {
		return fmt.Errorf("sandbox: list containers: %w", err)
	}
	ids := parseLabelFilter(out)
	for _, id := range ids {
		_ = runCommand(ctx, bin, "rm", "-f", id)
	}
	return nil
}

// containerRunning checks if `<name>` is currently running.
func (e *cliEngine) containerRunning(ctx context.Context, name string) (bool, error) {
	bin, _ := e.Detect()
	out, err := runCommandOutput(ctx, bin, "ps", "-q", "--filter", "name=^"+name+"$")
	if err != nil {
		return false, fmt.Errorf("sandbox: ps: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// containerExists checks if `<name>` exists in any state.
func (e *cliEngine) containerExists(ctx context.Context, name string) (bool, error) {
	bin, _ := e.Detect()
	out, err := runCommandOutput(ctx, bin, "ps", "-a", "-q", "--filter", "name=^"+name+"$")
	if err != nil {
		return false, fmt.Errorf("sandbox: ps -a: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// --- Exec ----------------------------------------------------------

func (e *cliEngine) Exec(ctx context.Context, sessionID string, args ExecArgs) (*ExecResult, error) {
	bin, ok := e.Detect()
	if !ok {
		return nil, ErrEngineNotAvailable
	}
	name := containerName(sessionID)
	running, err := e.containerRunning(ctx, name)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, ErrContainerNotRunning
	}

	timeout := args.Timeout
	if timeout <= 0 {
		timeout = time.Duration(e.cfg.TimeoutSeconds) * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execArgs := buildExecArgs(name, args.Language)
	cmd := exec.CommandContext(execCtx, bin, execArgs...)
	cmd.Stdin = strings.NewReader(args.Code)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	res := &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		if res.ExitCode == 0 {
			res.ExitCode = 124
		}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else if !res.TimedOut {
			return nil, fmt.Errorf("sandbox exec: %w", err)
		}
	}
	return res, nil
}

// buildExecArgs builds the `<engine> exec -i <name> <interpreter> -`
// argv. Code is fed via stdin so we don't have to escape quotes.
// "shell" → `sh -s`, "python" → `python3 -`.
func buildExecArgs(name, language string) []string {
	switch language {
	case "python":
		return []string{"exec", "-i", "--workdir", "/work", name, "python3", "-"}
	default:
		return []string{"exec", "-i", "--workdir", "/work", name, "sh", "-s"}
	}
}

// --- Info ----------------------------------------------------------

func (e *cliEngine) Info(ctx context.Context, sessionID string) (*Info, error) {
	if err := e.EnsureContainer(ctx, sessionID); err != nil {
		return nil, err
	}
	bin, _ := e.Detect()

	out := &Info{
		Engine:      bin,
		Image:       e.cfg.Image,
		Network:     e.cfg.Network,
		CPULimit:    e.cfg.CPULimit,
		MemoryLimit: e.cfg.MemoryLimit,
		TimeoutSec:  e.cfg.TimeoutSeconds,
	}

	if v, err := runCommandOutput(ctx, bin, "--version"); err == nil {
		out.EngineVersion = strings.TrimSpace(v)
	}

	// Python version + pip list inside the container.
	if r, err := e.Exec(ctx, sessionID, ExecArgs{Language: "shell", Code: "python3 -V 2>&1"}); err == nil && r.ExitCode == 0 {
		out.PythonVersion = strings.TrimSpace(r.Stdout)
	}
	if r, err := e.Exec(ctx, sessionID, ExecArgs{Language: "shell", Code: "pip list --format=freeze 2>/dev/null | head -200"}); err == nil && r.ExitCode == 0 {
		for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
			if line != "" {
				out.PipPackages = append(out.PipPackages, line)
			}
		}
	}

	out.WorkFiles = ListWorkFiles(e.WorkDir(sessionID), 50)
	return out, nil
}

// ListWorkFiles walks workDir and returns up to limit entries sorted
// newest-first by mtime. Exported so the Wails bindings can list a
// session's /work directory regardless of whether the engine is
// currently running — the directory layout is owned by the engine
// but reading it is just file I/O.
//
// Pass limit ≤ 0 for "no limit"; the caller is then responsible
// for any truncation.
func ListWorkFiles(workDir string, limit int) []FileInfo {
	var files []FileInfo
	_ = filepath.WalkDir(workDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(workDir, p)
		files = append(files, FileInfo{Path: rel, Size: info.Size(), MTime: info.ModTime()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].MTime.After(files[j].MTime) })
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files
}

// --- WorkDir -------------------------------------------------------

func (e *cliEngine) WorkDir(sessionID string) string {
	return filepath.Join(e.cfg.SessionsDir, sessionID, "work")
}

// --- helpers -------------------------------------------------------

func containerName(sessionID string) string {
	return containerNamePrefix + sanitizeName(sessionID)
}

// sanitizeName replaces characters that container engines reject in
// names with underscores. Container names accept [a-zA-Z0-9_.-].
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// runCommand runs a command and discards stdout/stderr unless it
// fails — the returned error includes captured stderr.
func runCommand(ctx context.Context, bin string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return err
		}
		return fmt.Errorf("%s: %s", bin, msg)
	}
	return nil
}

// runCommandOutput captures stdout.
func runCommandOutput(ctx context.Context, bin string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%s: %s", bin, msg)
	}
	return stdout.String(), nil
}

// parseLabelFilter parses the line-separated container ID output of
// `<engine> ps --filter`. Whitespace-only lines are dropped.
func parseLabelFilter(out string) []string {
	var ids []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		id := strings.TrimSpace(scanner.Text())
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
