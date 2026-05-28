package config

import (
	"encoding/json"
	"testing"
)

func TestAnalysisConfig_Resolved(t *testing.T) {
	// Zero resolves to the package defaults.
	var zero AnalysisConfig
	if got := zero.MaxQueryRowsResolved(); got != DefaultMaxQueryRows {
		t.Errorf("zero MaxQueryRowsResolved() = %d, want %d", got, DefaultMaxQueryRows)
	}
	if got := zero.MaxExportRowsResolved(); got != DefaultMaxExportRows {
		t.Errorf("zero MaxExportRowsResolved() = %d, want %d", got, DefaultMaxExportRows)
	}

	// Explicit values pass through.
	set := AnalysisConfig{MaxQueryRows: 25_000, MaxExportRows: 500}
	if got := set.MaxQueryRowsResolved(); got != 25_000 {
		t.Errorf("MaxQueryRowsResolved() = %d, want 25000", got)
	}
	if got := set.MaxExportRowsResolved(); got != 500 {
		t.Errorf("MaxExportRowsResolved() = %d, want 500", got)
	}
}

func TestAnalysisConfig_JSONRoundTrip(t *testing.T) {
	in := Config{Analysis: AnalysisConfig{MaxQueryRows: 12_345, MaxExportRows: 678_900}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Analysis.MaxQueryRows != 12_345 {
		t.Errorf("MaxQueryRows round-trip = %d, want 12345", out.Analysis.MaxQueryRows)
	}
	if out.Analysis.MaxExportRows != 678_900 {
		t.Errorf("MaxExportRows round-trip = %d, want 678900", out.Analysis.MaxExportRows)
	}

	// An absent analysis block resolves to defaults (no migration needed).
	var empty Config
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if got := empty.Analysis.MaxQueryRowsResolved(); got != DefaultMaxQueryRows {
		t.Errorf("absent block MaxQueryRowsResolved() = %d, want %d", got, DefaultMaxQueryRows)
	}
	if got := empty.Analysis.MaxExportRowsResolved(); got != DefaultMaxExportRows {
		t.Errorf("absent block MaxExportRowsResolved() = %d, want %d", got, DefaultMaxExportRows)
	}
}
