package sandbox

import (
	"fmt"
	"strings"
)

// FormatExecResult renders an ExecResult into the LLM-facing string
// described in the design doc:
//
//	<stdout>
//
//	[stderr]
//	<stderr>
//
//	[exit: N]
//
// Sections with empty content are omitted (no [stderr] block when
// Stderr is empty). The exit footer is always present. When TimedOut
// is true a "(timed out)" suffix is appended to the footer.
func FormatExecResult(r *ExecResult) string {
	if r == nil {
		return "[exit: -1]"
	}
	var b strings.Builder
	if r.Stdout != "" {
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr]\n")
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	if r.TimedOut {
		fmt.Fprintf(&b, "[exit: %d (timed out)]", r.ExitCode)
	} else {
		fmt.Fprintf(&b, "[exit: %d]", r.ExitCode)
	}
	return b.String()
}

// FormatInfo renders an Info into a multi-line LLM-facing summary.
// Long lists (PipPackages, WorkFiles) are kept as-is; the caller is
// expected to apply the per-backend tool-result truncation.
func FormatInfo(i *Info) string {
	if i == nil {
		return "(sandbox not running)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "engine:    %s", i.Engine)
	if i.EngineVersion != "" {
		fmt.Fprintf(&b, " %s", strings.TrimPrefix(i.EngineVersion, i.Engine+" "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "image:     %s\n", i.Image)
	if i.PythonVersion != "" {
		fmt.Fprintf(&b, "python:    %s\n", strings.TrimPrefix(i.PythonVersion, "Python "))
	}
	if i.Network {
		b.WriteString("network:   on\n")
	} else {
		b.WriteString("network:   off\n")
	}
	fmt.Fprintf(&b, "limits:    cpus=%s memory=%s timeout=%ds\n", i.CPULimit, i.MemoryLimit, i.TimeoutSec)

	if len(i.PipPackages) > 0 {
		b.WriteString("\npackages (pip):\n")
		for _, p := range i.PipPackages {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	b.WriteString("\nwork directory (/work):\n")
	if len(i.WorkFiles) == 0 {
		b.WriteString("  (empty)\n")
	} else {
		for _, f := range i.WorkFiles {
			fmt.Fprintf(&b, "  %-30s %8s  %s\n", f.Path, humanSize(f.Size), f.MTime.Format("2006-01-02 15:04"))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// humanSize renders a byte count as B/KB/MB.
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
