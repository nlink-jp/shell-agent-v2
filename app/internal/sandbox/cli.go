package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/sandbox/imagebuild"
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

	// buildMu serialises BuildImage calls so a second click in the
	// Settings UI doesn't kick off a parallel `podman build`.
	buildMu sync.Mutex
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

	args := buildRunArgs(e.cfg, name, e.WorkDir(sessionID), useSELinuxRelabel(bin))
	if err := runCommand(ctx, bin, args...); err != nil {
		return fmt.Errorf("sandbox: start container: %w", err)
	}
	return nil
}

// ensureImage pulls e.cfg.Image when it's not already present
// locally. Idempotent and a no-op when the image exists.
func (e *cliEngine) ensureImage(ctx context.Context) error {
	bin, _ := e.Detect()
	ready, _ := e.imageReadyOn(ctx, bin, e.cfg.Image)
	if ready {
		return nil
	}
	if pullErr := runCommand(ctx, bin, "pull", e.cfg.Image); pullErr != nil {
		return fmt.Errorf("sandbox: pull image %s: %w", e.cfg.Image, pullErr)
	}
	return nil
}

// ImageReady reports whether tag exists on the local engine.
// `podman image exists` exits 0 when present and non-zero (with
// no stderr) when missing — we treat those two as the bool
// outcome and only return an error on real engine failures
// (binary missing, daemon down).
func (e *cliEngine) ImageReady(ctx context.Context, tag string) (bool, error) {
	bin, ok := e.Detect()
	if !ok {
		return false, ErrEngineNotAvailable
	}
	return e.imageReadyOn(ctx, bin, tag)
}

func (e *cliEngine) imageReadyOn(ctx context.Context, bin, tag string) (bool, error) {
	_, err := runCommandOutput(ctx, bin, "image", "exists", tag)
	if err == nil {
		return true, nil
	}
	// `image exists` returns non-zero with no stderr when the tag
	// is simply absent. Real failures (engine not running, unknown
	// flag) surface as exec errors with stderr; we don't try to
	// distinguish here — the caller treats (false, nil) and
	// (false, err) the same way (image not usable). Surface only
	// when the binary itself failed.
	if errors.Is(err, exec.ErrNotFound) {
		return false, err
	}
	return false, nil
}

// BuildImage writes the given Dockerfile body to a temp dir
// and runs `<engine> build -t <tag> .` on it, with the tag
// derived from imagebuild.TagFor(dockerfile). Stdout/stderr
// stream to onLine line by line.
//
// Concurrent calls are serialised via buildMu — a second
// click in the Settings UI blocks until the first completes.
func (e *cliEngine) BuildImage(ctx context.Context, dockerfile string, onLine func(string)) (string, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()

	bin, ok := e.Detect()
	if !ok {
		return "", ErrEngineNotAvailable
	}

	tag := imagebuild.TagFor(dockerfile)

	workDir, err := os.MkdirTemp("", "shell-agent-v2-build-*")
	if err != nil {
		return "", fmt.Errorf("sandbox build: temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	if err := os.WriteFile(filepath.Join(workDir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		return "", fmt.Errorf("sandbox build: write dockerfile: %w", err)
	}

	// `--label app=shell-agent-v2-sandbox=1` lets ListImages
	// enumerate just our builds without touching foreign tags.
	cmd := exec.CommandContext(ctx, bin, "build",
		"-t", tag,
		"--label", imagebuild.TagPrefix+"=1",
		workDir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("sandbox build: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("sandbox build: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("sandbox build: start: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(stdout, onLine, &wg)
	go streamLines(stderr, onLine, &wg)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("sandbox build: %w", err)
	}
	return tag, nil
}

// ListImages enumerates locally-built sandbox images. We
// identify ours by the tag prefix imagebuild.TagPrefix —
// the content-addressed sha[:12] suffix makes false
// positives effectively impossible. Returns newest-first.
//
// Earlier revisions tried `--filter label=…` but
// buildkit and classic builders attach labels
// inconsistently across podman / docker versions; a tag-
// prefix match is engine-agnostic.
func (e *cliEngine) ListImages(ctx context.Context) ([]ImageInfo, error) {
	bin, ok := e.Detect()
	if !ok {
		return nil, ErrEngineNotAvailable
	}
	out, err := runCommandOutput(ctx, bin, "images",
		"--format", "{{.Repository}}:{{.Tag}}|{{.CreatedAt}}|{{.Size}}",
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list images: %w", err)
	}
	var infos []ImageInfo
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 1 {
			continue
		}
		// Accept both "shell-agent-v2-sandbox:<sha>" and
		// "localhost/shell-agent-v2-sandbox:<sha>" — podman
		// prepends "localhost/" to locally-built images.
		// Normalise Tag to the bare form so it round-trips
		// against what BuildImage produced and what we
		// store in cfg.Sandbox.Image.
		repoTag := parts[0]
		bare := strings.TrimPrefix(repoTag, "localhost/")
		if !strings.HasPrefix(bare, imagebuild.TagPrefix+":") {
			continue
		}
		info := ImageInfo{Tag: bare}
		if len(parts) >= 2 {
			// Both podman and docker accept this format; podman
			// emits RFC3339-ish, docker emits "2 days ago" by
			// default. Best-effort parse.
			if t, err := parseEngineTime(parts[1]); err == nil {
				info.Created = t
			}
		}
		if len(parts) >= 3 {
			info.SizeBytes = parseEngineSize(parts[2])
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Created.After(infos[j].Created)
	})
	return infos, nil
}

// parseEngineTime is a best-effort parser for the various
// CreatedAt formats podman/docker emit. Failure returns a zero
// time which sorts oldest.
func parseEngineTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time format: %q", s)
}

// parseEngineSize reads a size string like "523MB" / "1.2GB"
// into a byte count. Best-effort; returns 0 on parse error.
func parseEngineSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1024*1024*1024, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1024*1024, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "kB"), strings.HasSuffix(s, "KB"):
		mult, s = 1024, s[:len(s)-2]
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Float-ish parse via strconv; ignore decimals beyond what
	// fits in int64 without bringing in math/big.
	dot := strings.IndexByte(s, '.')
	if dot >= 0 {
		s = s[:dot]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

// RemoveImage deletes the image with the given tag. Missing
// tags are a no-op so the UI can call Delete idempotently.
//
// podman labels locally-built images as
// "localhost/<repo>:<tag>" but accepts either form for `image
// rm` only when there's no ambiguity. We try both forms with
// -f so an in-use image is force-removed (the UI Delete
// confirms the user's intent already).
func (e *cliEngine) RemoveImage(ctx context.Context, tag string) error {
	bin, ok := e.Detect()
	if !ok {
		return ErrEngineNotAvailable
	}
	// Safety: only remove tags under our prefix. Caller might
	// pass anything; we refuse to touch foreign images.
	bare := strings.TrimPrefix(tag, "localhost/")
	if !strings.HasPrefix(bare, imagebuild.TagPrefix+":") {
		return fmt.Errorf("sandbox: refuse to remove non-sandbox tag %q", tag)
	}

	// Probe both forms; idempotent no-op when neither exists.
	bareReady, _ := e.imageReadyOn(ctx, bin, bare)
	localhostForm := "localhost/" + bare
	localhostReady, _ := e.imageReadyOn(ctx, bin, localhostForm)
	if !bareReady && !localhostReady {
		return nil
	}

	// Try the form podman actually has. Use -f because the UI
	// Delete already confirmed and we don't want a "in-use by
	// container" failure to confuse the user — the container
	// wrapping is also force-removable.
	target := bare
	if !bareReady {
		target = localhostForm
	}
	out, err := runCommandOutput(ctx, bin, "image", "rm", "-f", target)
	if err != nil {
		return fmt.Errorf("sandbox: remove image %s: %w (stdout=%q)", target, err, out)
	}
	return nil
}

// streamLines reads r line by line and forwards each line to
// onLine. nil onLine drops lines on the floor; this lets
// callers ignore progress without juggling a stub.
func streamLines(r io.Reader, onLine func(string), wg *sync.WaitGroup) {
	defer wg.Done()
	if r == nil {
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if onLine != nil {
			onLine(sc.Text())
		}
	}
}


// useSELinuxRelabel reports whether the bind mount should be
// suffixed with `:Z` (request SELinux relabel of the host
// directory). Only correct on podman + Linux: podman on Linux
// uses SELinux when present, while docker-desktop on macOS
// rejects the option as invalid and Linux+docker without SELinux
// can clobber labels on shared parents. Probed once per
// EnsureContainer call from the resolved binary basename.
func useSELinuxRelabel(binary string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return strings.EqualFold(filepath.Base(binary), "podman")
}

// buildRunArgs builds the `podman run` / `docker run` argv (without
// the binary prefix). Exposed for unit testing.
func buildRunArgs(cfg Config, name, workDir string, selinuxRelabel bool) []string {
	mountSpec := workDir + ":/work:rw"
	if selinuxRelabel {
		mountSpec = workDir + ":/work:Z"
	}
	args := []string{
		"run", "-d",
		"--name", name,
		"--label", containerLabel,
		"--workdir", "/work",
		"--volume", mountSpec,
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
	stdout := newLimitedBuffer(e.cfg.MaxOutputBytes)
	stderr := newLimitedBuffer(e.cfg.MaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

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
		for line := range strings.SplitSeq(strings.TrimSpace(r.Stdout), "\n") {
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
