package sandbox

import (
	"strings"
	"testing"
)

func TestLimitedBuffer_BelowCap(t *testing.T) {
	b := newLimitedBuffer(100)
	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if got := b.String(); got != "hello" {
		t.Errorf("string = %q, want %q", got, "hello")
	}
	if b.Truncated() {
		t.Error("Truncated should be false below cap")
	}
}

func TestLimitedBuffer_ExactCap(t *testing.T) {
	b := newLimitedBuffer(5)
	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	// At-cap with no overflow should not be flagged truncated.
	if b.Truncated() {
		t.Error("Truncated should be false when fill is exactly cap")
	}
}

// TestLimitedBuffer_OverflowReportsFullN verifies the writer reports
// the full input length even when it actually buffered less. This
// keeps the kernel pipe / child process from observing a short write
// and retrying — security-hardening-2.md C3.
func TestLimitedBuffer_OverflowReportsFullN(t *testing.T) {
	b := newLimitedBuffer(3)
	in := []byte("abcdef")
	n, err := b.Write(in)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(in) {
		t.Errorf("n = %d, want %d (must report full len even after truncate)", n, len(in))
	}
	if !b.Truncated() {
		t.Error("Truncated should be true after overflow")
	}
	out := b.String()
	if !strings.HasPrefix(out, "abc") {
		t.Errorf("buffered prefix = %q, want 'abc'", out[:3])
	}
	if !strings.Contains(out, "[output truncated at 3 bytes]") {
		t.Errorf("missing truncation marker: %q", out)
	}
}

// TestLimitedBuffer_MultipleWritesSpanCap covers the case where the
// cap is hit mid-Write — the partial fill must include the prefix of
// the second write, not just the first.
func TestLimitedBuffer_MultipleWritesSpanCap(t *testing.T) {
	b := newLimitedBuffer(7)
	if _, err := b.Write([]byte("abcd")); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if b.Truncated() {
		t.Error("not yet at cap")
	}
	if _, err := b.Write([]byte("efghij")); err != nil {
		t.Fatalf("write2: %v", err)
	}
	if !b.Truncated() {
		t.Error("should be truncated after second write")
	}
	out := b.String()
	if !strings.HasPrefix(out, "abcdefg") {
		t.Errorf("prefix should be 'abcdefg', got %q", out[:7])
	}
}

// TestLimitedBuffer_WritesAfterCapStillReportFullN ensures even
// post-cap writes are reported as fully consumed so the child does
// not enter an error loop. We over-fill first so the buffer is
// definitely past the cap, then write more to exercise the
// post-cap path.
func TestLimitedBuffer_WritesAfterCapStillReportFullN(t *testing.T) {
	b := newLimitedBuffer(2)
	b.Write([]byte("aaa")) // 3 > cap 2, triggers truncation
	if !b.Truncated() {
		t.Error("expected truncated after over-fill")
	}
	n, err := b.Write([]byte("more"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 4 {
		t.Errorf("post-cap n = %d, want 4", n)
	}
}

func TestLimitedBuffer_ZeroCapUsesDefault(t *testing.T) {
	b := newLimitedBuffer(0)
	if b.cap != DefaultMaxOutputBytes {
		t.Errorf("cap = %d, want %d", b.cap, DefaultMaxOutputBytes)
	}
}
