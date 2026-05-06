package logger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// resetGlobalForTest reinitialises the package-level global so each
// test gets a fresh logger backed by its own tempdir. The Init
// sync.Once normally prevents re-initialisation, so we reach
// directly into the package state — only acceptable from tests in
// the same package.
func resetGlobalForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	once = sync.Once{}
	global = nil
	if err := Init(dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return filepath.Join(dir, "app.log")
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(data)
}

// TestDefaultLevelInfoDropsDebug confirms the v0.3.0 privacy
// default: a fresh Init lands at LevelInfo and Debug calls do
// NOT reach the file.
func TestDefaultLevelInfoDropsDebug(t *testing.T) {
	logPath := resetGlobalForTest(t)
	if got := CurrentLevel(); got != LevelInfo {
		t.Fatalf("default level = %v, want LevelInfo", got)
	}

	Debug("user said: %s", "secret-prompt-content")
	Info("event without content")

	out := readLog(t, logPath)
	if strings.Contains(out, "secret-prompt-content") {
		t.Errorf("Debug call leaked into log at default Info level: %q", out)
	}
	if !strings.Contains(out, "event without content") {
		t.Errorf("Info call missing from log: %q", out)
	}
}

// TestSetLevelDebugRestoresVerbose confirms the operator escape
// hatch: switching to LevelDebug surfaces Debug calls again.
func TestSetLevelDebugRestoresVerbose(t *testing.T) {
	logPath := resetGlobalForTest(t)
	SetLevel(LevelDebug)
	Debug("debug now visible: %s", "diagnose-token")

	out := readLog(t, logPath)
	if !strings.Contains(out, "diagnose-token") {
		t.Errorf("Debug should be visible after SetLevel(LevelDebug): %q", out)
	}
}

// TestSetLevelErrorSilencesInfoToo pins that LevelError gates out
// Info as well. Useful for production-mode runs where only failures
// matter.
func TestSetLevelErrorSilencesInfoToo(t *testing.T) {
	logPath := resetGlobalForTest(t)
	SetLevel(LevelError)
	Info("informational only")
	Debug("debug only")
	Error("real error")

	out := readLog(t, logPath)
	if strings.Contains(out, "informational only") {
		t.Errorf("Info should be silenced at LevelError: %q", out)
	}
	if strings.Contains(out, "debug only") {
		t.Errorf("Debug should be silenced at LevelError: %q", out)
	}
	if !strings.Contains(out, "real error") {
		t.Errorf("Error must always log: %q", out)
	}
}

// TestErrorAlwaysLogs pins the contract that errors bypass the
// level filter — privacy is not a concern here because Error
// messages are conventionally fmt.Errorf wraps, not raw user
// content.
func TestErrorAlwaysLogs(t *testing.T) {
	logPath := resetGlobalForTest(t)
	for _, lvl := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		SetLevel(lvl)
		Error("err at level %v", lvl)
	}
	out := readLog(t, logPath)
	for _, lvl := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		needle := "err at level " + lvlString(lvl)
		if !strings.Contains(out, needle) {
			t.Errorf("Error missing for level %v: %q", lvl, out)
		}
	}
}

func lvlString(l Level) string {
	// Mirrors the integer formatting used by fmt.Sprintf("%v", l).
	switch l {
	case LevelDebug:
		return "0"
	case LevelInfo:
		return "1"
	case LevelWarn:
		return "2"
	case LevelError:
		return "3"
	}
	return "?"
}
