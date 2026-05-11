package analysis

import "testing"

// setMaxAnalyzeRowsForTesting temporarily lowers MaxAnalyzeRows
// so cap-overflow tests don't need to materialise a million rows.
// Restored on t.Cleanup. Test-only seam — do not call from
// production code.
func setMaxAnalyzeRowsForTesting(t *testing.T, n int) {
	t.Helper()
	prev := MaxAnalyzeRows
	MaxAnalyzeRows = n
	t.Cleanup(func() { MaxAnalyzeRows = prev })
}
