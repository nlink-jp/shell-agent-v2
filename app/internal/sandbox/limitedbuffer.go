package sandbox

import (
	"bytes"
	"fmt"
)

// limitedBuffer is an io.Writer that buffers up to cap bytes and
// silently discards the rest, recording that truncation happened.
// The discard is intentional: returning an error to the child
// process would change its observed exit behaviour, and we want the
// command to continue to natural termination so the timeout / exit
// code path stays accurate. Excess bytes never reach memory at all
// — only the per-Write slice is briefly held in the kernel pipe
// before being dropped here (security-hardening-2.md C3).
type limitedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func newLimitedBuffer(cap int) *limitedBuffer {
	if cap <= 0 {
		cap = DefaultMaxOutputBytes
	}
	return &limitedBuffer{cap: cap}
}

// Write satisfies io.Writer. Always reports n == len(p) on success
// so the writer (the child process via the kernel pipe) doesn't
// observe a short write and start retrying.
func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if l.buf.Len() >= l.cap {
		l.truncated = true
		return n, nil
	}
	remaining := l.cap - l.buf.Len()
	if len(p) > remaining {
		l.buf.Write(p[:remaining])
		l.truncated = true
		return n, nil
	}
	l.buf.Write(p)
	return n, nil
}

// String returns the buffered contents, with a truncation marker
// appended if any bytes were dropped. Marker bytes count as part of
// the returned string, not toward the cap (the cap was hit before
// the marker was added).
func (l *limitedBuffer) String() string {
	if !l.truncated {
		return l.buf.String()
	}
	return l.buf.String() + fmt.Sprintf("\n... [output truncated at %d bytes]", l.cap)
}

// Truncated reports whether the buffer dropped any input.
func (l *limitedBuffer) Truncated() bool {
	return l.truncated
}
