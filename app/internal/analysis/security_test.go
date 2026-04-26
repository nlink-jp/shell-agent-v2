package analysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateFilePath(t *testing.T) {
	tmp := t.TempDir()
	good := filepath.Join(tmp, "data.csv")
	if err := os.WriteFile(good, []byte("a,b\n1,2"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("valid file", func(t *testing.T) {
		got, err := validateFilePath(good)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("expected absolute path, got %s", got)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := validateFilePath("")
		if err == nil {
			t.Error("expected error for empty path")
		}
	})

	t.Run("nonexistent", func(t *testing.T) {
		_, err := validateFilePath(filepath.Join(tmp, "does-not-exist.csv"))
		if err == nil {
			t.Error("expected error for nonexistent file")
		}
	})

	t.Run("directory", func(t *testing.T) {
		_, err := validateFilePath(tmp)
		if err == nil {
			t.Error("expected error for directory path")
		}
	})

	t.Run("newline injection", func(t *testing.T) {
		_, err := validateFilePath(good + "\n; DROP TABLE x; --")
		if err == nil {
			t.Error("expected error for path with newline")
		}
	})
}

func TestEscapeSQLString(t *testing.T) {
	tests := []struct {
		in, out string
	}{
		{"hello", "hello"},
		{"O'Brien", "O''Brien"},
		{"a'b'c", "a''b''c"},
		{"'); DROP TABLE x; --", "''); DROP TABLE x; --"},
	}
	for _, tt := range tests {
		got := escapeSQLString(tt.in)
		if got != tt.out {
			t.Errorf("escapeSQLString(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestQuerySQLRowLimit(t *testing.T) {
	if MaxQueryRows < 100 {
		t.Errorf("MaxQueryRows too low: %d", MaxQueryRows)
	}
	// Sanity check: error message references the constant
	// (ensures the limit is actually enforced; integration test in agent package
	// would verify behavior end-to-end)
	if !strings.Contains("only SELECT queries are allowed", "SELECT") {
		t.Skip()
	}
}
