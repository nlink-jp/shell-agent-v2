package analysis

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTypeSweep_RegressionGuard is the permanent guard for the
// type-dispatched scalar conversion introduced by ADR-0010. It
// loads one row containing every DuckDB scalar / nested type we
// plausibly support, dumps each of the three result-extraction
// paths (PreviewTable / QuerySQL / QuerySQLToCSV), and asserts
// the canonical form for the six types Phase 1 fixed:
// UUID, BLOB, DECIMAL, INTERVAL, MAP, TIME.
//
// Display-quality issues for LIST / STRUCT / JSON-as-VARCHAR are
// printed (not asserted) so future drift remains visible. New
// DuckDB types or driver upgrades that change the rendering of an
// asserted type fail the test — that is the intended retrofit gate.
func TestTypeSweep_RegressionGuard(t *testing.T) {
	tmpDir := t.TempDir()
	e := &Engine{
		sessionID: "typesweep",
		dbPath:    filepath.Join(tmpDir, "x.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// One row, every type we care about. Casts are explicit so
	// DuckDB's auto-inference doesn't shift things underneath us.
	const ddl = `
		CREATE TABLE type_sweep AS SELECT
			'plain text'                                          AS varchar_col,
			42                                                    AS integer_col,
			CAST(9223372036854775807 AS BIGINT)                   AS bigint_col,
			CAST(18446744073709551615 AS UBIGINT)                 AS ubigint_col,
			CAST(170141183460469231731687303715884105727 AS HUGEINT) AS hugeint_col,
			CAST(123.456 AS DOUBLE)                               AS double_col,
			CAST(123.456 AS DECIMAL(10,3))                        AS decimal_col,
			TRUE                                                  AS boolean_col,
			DATE '2026-05-14'                                     AS date_col,
			TIME '12:34:56'                                       AS time_col,
			TIMESTAMP '2026-05-14 12:34:56'                       AS timestamp_col,
			TIMESTAMPTZ '2026-05-14 12:34:56+09'                  AS timestamptz_col,
			INTERVAL 1 YEAR + INTERVAL 2 MONTH                    AS interval_col,
			CAST('550e8400-e29b-41d4-a716-446655440000' AS UUID)  AS uuid_col,
			CAST('\xDE\xAD\xBE\xEF' AS BLOB)                      AS blob_col,
			[1, 2, 3]                                             AS list_col,
			{'a': 1, 'b': 'x'}                                    AS struct_col,
			MAP {'k1': 'v1', 'k2': 'v2'}                          AS map_col,
			CAST('{"x": 1}' AS JSON)                              AS json_col`
	if _, err := e.db.Exec(ddl); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if err := e.refreshTableMeta("type_sweep"); err != nil {
		t.Fatalf("refreshTableMeta: %v", err)
	}

	// --- Probe 0: rows.ColumnTypes() — what DatabaseTypeName()
	// returns is the dispatch key for the type-dispatched fix. ---
	t.Log("\n=== rows.ColumnTypes() ===")
	probeRows, err := e.db.Query("SELECT * FROM type_sweep")
	if err != nil {
		t.Fatalf("probe query: %v", err)
	}
	probeTypes, err := probeRows.ColumnTypes()
	if err != nil {
		t.Fatalf("ColumnTypes: %v", err)
	}
	for _, ct := range probeTypes {
		t.Logf("  %-18s DatabaseTypeName=%-12s ScanType=%v",
			ct.Name(), ct.DatabaseTypeName(), reflect.TypeOf(scanZero(ct)))
	}
	probeRows.Close()

	// --- Path 1: PreviewTable (Data panel UI). ---
	t.Log("\n=== Path 1: PreviewTable (UI) ===")
	prev, err := e.PreviewTable("type_sweep", 1)
	if err != nil {
		t.Fatalf("PreviewTable: %v", err)
	}
	if len(prev.Rows) != 1 {
		t.Fatalf("PreviewTable rows=%d", len(prev.Rows))
	}
	for i, col := range prev.Columns {
		v := prev.Rows[0][i]
		t.Logf("  %-18s value=%-50q  go-type=%T",
			col, fmt.Sprintf("%v", v), v)
	}
	// Also marshal the whole preview to JSON the way Wails would
	// over the wire — this is what the React UI actually receives.
	prevJSON, _ := json.Marshal(prev.Rows[0])
	t.Logf("  -- PreviewTable row as JSON: %s", string(prevJSON))

	// --- Path 2: QuerySQL → JSON (LLM tool result). ---
	t.Log("\n=== Path 2: QuerySQL → JSON (LLM tool result) ===")
	rows, err := e.QuerySQL("SELECT * FROM type_sweep")
	if err != nil {
		t.Fatalf("QuerySQL: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("QuerySQL rows=%d", len(rows))
	}
	for k, v := range rows[0] {
		jsonBytes, _ := json.Marshal(v)
		t.Logf("  %-18s json=%-60s  go-type=%T",
			k, string(jsonBytes), v)
	}

	// --- Path 3: QuerySQLToCSV (sandbox-export-sql / download). ---
	t.Log("\n=== Path 3: QuerySQLToCSV (CSV export) ===")
	var buf bytes.Buffer
	cols, n, err := e.QuerySQLToCSV("SELECT * FROM type_sweep", &buf)
	if err != nil {
		t.Fatalf("QuerySQLToCSV: %v", err)
	}
	t.Logf("  cols=%v rowCount=%d", cols, n)
	t.Logf("  CSV body:\n%s", buf.String())

	// --- Assertions for the 6 types ADR-0010 Phase 1 fixes. ---
	// Map column name → expected (Preview JSON form, QuerySQL JSON form, CSV cell).
	type want struct {
		previewJSON string
		querySQL    string
		csvCell     string
	}
	wants := map[string]want{
		"uuid_col":     {`"550e8400-e29b-41d4-a716-446655440000"`, `"550e8400-e29b-41d4-a716-446655440000"`, "550e8400-e29b-41d4-a716-446655440000"},
		"blob_col":     {`"3q2+7w=="`, `"3q2+7w=="`, "3q2+7w=="},
		"decimal_col":  {`"123.456"`, `"123.456"`, "123.456"},
		"interval_col": {`"P1Y2M0DT0H0M0S"`, `"P1Y2M0DT0H0M0S"`, "P1Y2M0DT0H0M0S"},
		"map_col":      {`{"k1":"v1","k2":"v2"}`, `{"k1":"v1","k2":"v2"}`, "map[k1:v1 k2:v2]"}, // CSV uses Go fmt for maps; Phase 2.
		"time_col":     {`"12:34:56"`, `"12:34:56"`, "12:34:56"},
	}
	colIdx := map[string]int{}
	for i, c := range prev.Columns {
		colIdx[c] = i
	}
	for col, w := range wants {
		i := colIdx[col]
		// Preview: marshal the single value and compare.
		gotPrev, _ := json.Marshal(prev.Rows[0][i])
		if string(gotPrev) != w.previewJSON {
			t.Errorf("Preview[%s]: got %s, want %s", col, gotPrev, w.previewJSON)
		}
		// QuerySQL: marshal the value and compare.
		gotQ, _ := json.Marshal(rows[0][col])
		if string(gotQ) != w.querySQL {
			t.Errorf("QuerySQL[%s]: got %s, want %s", col, gotQ, w.querySQL)
		}
		// CSV: parse the body, get the row, get the column.
		csvLines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
		if len(csvLines) < 2 {
			t.Fatalf("CSV: expected header + data row")
		}
		header := bytes.Split(csvLines[0], []byte(","))
		dataCells := bytes.Split(csvLines[1], []byte(","))
		var ci int = -1
		for j, h := range header {
			if string(h) == col {
				ci = j
				break
			}
		}
		if ci < 0 {
			t.Fatalf("CSV: column %s not found in header", col)
		}
		if got := string(dataCells[ci]); got != w.csvCell {
			t.Errorf("CSV[%s]: got %q, want %q", col, got, w.csvCell)
		}
	}
}

// TestUUIDLoadedFromJSON_RendersCanonically pins the user-reported
// symptom independently of the broader sweep. A UUID-shaped string
// loaded via read_json_auto must come back through all three
// result paths as the canonical 8-4-4-4-12 lowercase form.
func TestUUIDLoadedFromJSON_RendersCanonically(t *testing.T) {
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "data.json")
	body := `[{"guid":"550e8400-e29b-41d4-a716-446655440000","name":"row1"}]`
	if err := writeFile(jsonPath, body); err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		sessionID: "uuidtest",
		dbPath:    filepath.Join(tmpDir, "x.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()
	if err := e.LoadJSON("guid_table", jsonPath); err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}

	// Confirm DuckDB inferred UUID — the precondition for the bug.
	row := e.db.QueryRow("SELECT data_type FROM information_schema.columns WHERE table_name='guid_table' AND column_name='guid'")
	var dataType string
	if err := row.Scan(&dataType); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if dataType != "UUID" {
		t.Fatalf("guid column inferred as %q, expected UUID — sweep precondition broken", dataType)
	}

	const wantUUID = "550e8400-e29b-41d4-a716-446655440000"

	// Path 1: Preview.
	prev, err := e.PreviewTable("guid_table", 1)
	if err != nil {
		t.Fatalf("PreviewTable: %v", err)
	}
	guidIdx := -1
	for i, c := range prev.Columns {
		if c == "guid" {
			guidIdx = i
		}
	}
	if got, ok := prev.Rows[0][guidIdx].(string); !ok || got != wantUUID {
		t.Errorf("Preview guid = %v (%T), want %q", prev.Rows[0][guidIdx], prev.Rows[0][guidIdx], wantUUID)
	}

	// Path 2: QuerySQL → JSON.
	rows, err := e.QuerySQL("SELECT guid FROM guid_table")
	if err != nil {
		t.Fatalf("QuerySQL: %v", err)
	}
	if got, ok := rows[0]["guid"].(string); !ok || got != wantUUID {
		t.Errorf("QuerySQL guid = %v (%T), want %q", rows[0]["guid"], rows[0]["guid"], wantUUID)
	}

	// Path 3: QuerySQLToCSV.
	var buf bytes.Buffer
	if _, _, err := e.QuerySQLToCSV("SELECT guid FROM guid_table", &buf); err != nil {
		t.Fatalf("QuerySQLToCSV: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(wantUUID)) {
		t.Errorf("CSV body missing canonical UUID %q; got:\n%s", wantUUID, buf.String())
	}
}

// TestPreviewRowMarshalsToJSON_AllSweepTypes pins the silent
// json.Marshal failure where a duckdb.Map column in the row caused
// json.Marshal to return an empty result, breaking the Wails event
// payload that ferries Preview rows to React.
func TestPreviewRowMarshalsToJSON_AllSweepTypes(t *testing.T) {
	tmpDir := t.TempDir()
	e := &Engine{
		sessionID: "marshaltest",
		dbPath:    filepath.Join(tmpDir, "x.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := e.db.Exec(`CREATE TABLE m AS SELECT MAP {'k1': 'v1'} AS m_col, 'plain' AS s_col`); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if err := e.refreshTableMeta("m"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	prev, err := e.PreviewTable("m", 1)
	if err != nil {
		t.Fatalf("PreviewTable: %v", err)
	}
	out, err := json.Marshal(prev.Rows[0])
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(out) == 0 || string(out) == "null" {
		t.Errorf("expected non-empty JSON, got %q", string(out))
	}
	// Sanity-check the MAP serialised properly.
	if !bytes.Contains(out, []byte(`"k1":"v1"`)) {
		t.Errorf("MAP did not serialise as JSON object: %s", string(out))
	}
}

// writeFile is a tiny test helper to keep callers terse.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0644)
}

// scanZero peeks at what go-duckdb would Scan into when the column
// is read into an interface{}.
func scanZero(ct *sql.ColumnType) any {
	if t := ct.ScanType(); t != nil {
		return reflect.New(t).Elem().Interface()
	}
	return nil
}
