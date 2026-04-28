package sandbox

import (
	"strings"
	"testing"
	"time"
)

func TestFormatExecResult_StdoutOnly(t *testing.T) {
	got := FormatExecResult(&ExecResult{Stdout: "hello\n", ExitCode: 0})
	if !strings.HasPrefix(got, "hello\n") {
		t.Errorf("missing stdout prefix: %q", got)
	}
	if !strings.HasSuffix(got, "[exit: 0]") {
		t.Errorf("missing exit footer: %q", got)
	}
	if strings.Contains(got, "[stderr]") {
		t.Errorf("stderr block must not appear when Stderr is empty: %q", got)
	}
}

func TestFormatExecResult_StderrAppended(t *testing.T) {
	got := FormatExecResult(&ExecResult{Stdout: "out", Stderr: "err", ExitCode: 1})
	if !strings.Contains(got, "[stderr]\nerr") {
		t.Errorf("stderr block missing: %q", got)
	}
	if !strings.Contains(got, "[exit: 1]") {
		t.Errorf("exit footer missing: %q", got)
	}
}

func TestFormatExecResult_ExitFooterAlwaysPresent(t *testing.T) {
	got := FormatExecResult(&ExecResult{ExitCode: 137})
	if got != "[exit: 137]" {
		t.Errorf("empty result format = %q, want bare exit footer", got)
	}
}

func TestFormatExecResult_TimedOutSuffix(t *testing.T) {
	got := FormatExecResult(&ExecResult{ExitCode: 124, TimedOut: true})
	if !strings.Contains(got, "(timed out)") {
		t.Errorf("timed out should be flagged: %q", got)
	}
}

func TestFormatExecResult_NilSafe(t *testing.T) {
	got := FormatExecResult(nil)
	if got == "" || !strings.Contains(got, "exit") {
		t.Errorf("nil input should return a recognisable footer; got %q", got)
	}
}

func TestFormatInfo_BasicShape(t *testing.T) {
	i := &Info{
		Engine: "podman", EngineVersion: "podman 4.9.4",
		Image: "python:3.12-slim", PythonVersion: "Python 3.12.5",
		Network: false, CPULimit: "2", MemoryLimit: "1g", TimeoutSec: 60,
		PipPackages: []string{"numpy==1.26", "pandas==2.2"},
		WorkFiles: []FileInfo{
			{Path: "data.csv", Size: 12345, MTime: time.Date(2026, 4, 28, 22, 45, 0, 0, time.UTC)},
		},
	}
	got := FormatInfo(i)
	for _, want := range []string{
		"engine:",
		"4.9.4",
		"image:",
		"python:",
		"3.12.5",
		"network:   off",
		"limits:",
		"cpus=2",
		"memory=1g",
		"timeout=60s",
		"numpy==1.26",
		"pandas==2.2",
		"data.csv",
		"12.1 KB",   // 12345 / 1024 ≈ 12.1
		"2026-04-28 22:45",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatInfo missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatInfo_EmptyWorkDir(t *testing.T) {
	got := FormatInfo(&Info{Engine: "podman", Image: "x", CPULimit: "1", MemoryLimit: "1g", TimeoutSec: 60})
	if !strings.Contains(got, "(empty)") {
		t.Errorf("empty work dir placeholder missing: %s", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:                 "0 B",
		1023:              "1023 B",
		1024:              "1.0 KB",
		1024 * 1024:       "1.0 MB",
		2 * 1024 * 1024:   "2.0 MB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
