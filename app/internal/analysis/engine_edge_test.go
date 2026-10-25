package analysis

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuerySQLOnEmptyDB(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "empty.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	e.Open()
	defer e.Close()

	results, err := e.QuerySQL("SELECT 1 AS n")
	if err != nil {
		t.Fatalf("error on empty DB: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
}

func TestLoadCSVOverwrite(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "overwrite.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()

	csv1 := filepath.Join(t.TempDir(), "v1.csv")
	os.WriteFile(csv1, []byte("a\n1\n2\n"), 0644)
	e.LoadCSV("data", csv1)

	csv2 := filepath.Join(t.TempDir(), "v2.csv")
	os.WriteFile(csv2, []byte("a\n10\n20\n30\n"), 0644)
	e.LoadCSV("data", csv2)

	tables := e.Tables()
	if len(tables) != 1 {
		t.Fatalf("tables = %d, want 1", len(tables))
	}
	if tables[0].RowCount != 3 {
		t.Errorf("row count = %d, want 3 (overwritten)", tables[0].RowCount)
	}
}

func TestResetOnEmptyDB(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "reset.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	e.Open()
	defer e.Close()

	// Reset on empty should not error
	if err := e.Reset(); err != nil {
		t.Errorf("reset on empty: %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "close.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	e.Open()

	if err := e.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestSetDescriptionOnClosedDB(t *testing.T) {
	e := &Engine{
		sessionID: "test",
		dbPath:    filepath.Join(t.TempDir(), "closed.duckdb"),
		tables:    make(map[string]*TableMeta),
	}

	err := e.SetTableDescription("test", "desc")
	if err == nil {
		t.Error("expected error on closed DB")
	}
}

// TestReadOnlyGate drives agent-written SQL through every read path and
// create-from-query, and checks both the verdict and that the database
// is unchanged afterwards — a guard that only says "rejected" while a
// leading statement already ran would pass a verdict-only test.
func TestReadOnlyGate(t *testing.T) {
	e := &Engine{
		sessionID: "gate",
		dbPath:    filepath.Join(t.TempDir(), "gate.duckdb"),
		tables:    make(map[string]*TableMeta),
	}
	defer e.Close()
	if err := e.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := e.db.Exec("CREATE TABLE t AS SELECT 1 AS x"); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshTableMeta("t"); err != nil {
		t.Fatal(err)
	}
	unchanged := func(t *testing.T, q string) {
		t.Helper()
		var n int
		if err := e.db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 1 {
			t.Fatalf("after %q: table t changed (rows=%d, err=%v)", q, n, err)
		}
		var tables int
		e.db.QueryRow("SELECT count(*) FROM duckdb_tables()").Scan(&tables)
		var dbs int
		e.db.QueryRow("SELECT count(*) FROM duckdb_databases() WHERE NOT internal").Scan(&dbs)
		if tables != 1 || dbs != 1 {
			t.Fatalf("after %q: tables=%d attached databases=%d, want 1 and 1", q, tables, dbs)
		}
	}

	reads := []string{
		"SELECT * FROM t",
		"  select count(*) from t",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
		"FROM t",
		"DESCRIBE t",
		"SUMMARIZE t",
		"SELECT x FROM t;",
		"PIVOT t ON x IN (1)",
	}
	for _, q := range reads {
		if _, err := e.QuerySQL(q); err != nil {
			t.Errorf("QuerySQL(%q) = %v, want allowed", q, err)
		}
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "out.csv")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	writes := []string{
		"SELECT 1; DROP TABLE t",
		"SELECT 1; INSERT INTO t VALUES (2)",
		"DROP TABLE t; SELECT 1",
		"INSERT INTO t VALUES (2); SELECT * FROM t",
		// Type SELECT, refused by the read-only transaction:
		"WITH d AS (DELETE FROM t RETURNING *) SELECT count(*) FROM d",
		"WITH i AS (INSERT INTO t VALUES (2) RETURNING x) SELECT x FROM i",
		"WITH u AS (UPDATE t SET x = 9 RETURNING *) SELECT * FROM u",
		// Type SELECT, refused by the locked configuration:
		"SELECT * FROM enable_profiling(format='json', save_location='" + victim + "')",
		"SELECT * FROM enable_logging(storage='file', storage_path='" + logDir + "')",
		"SELECT * FROM sqlite_attach('" + filepath.Join(dir, "z.sqlite") + "')",
		"WITH c AS (SELECT 2) INSERT INTO t SELECT * FROM c",
		"INSERT INTO t VALUES (1)",
		"  UPDATE t SET x = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"CREATE TABLE u (id INT)",
		"ALTER TABLE t ADD COLUMN y INT",
		"COPY t TO '" + out + "'",
		"ATTACH ':memory:' AS m",
		"SET threads = 1",
		"LOAD 'ext'",
		"INSTALL 'ext'",
		"CALL pragma_version()",
		"EXPLAIN ANALYZE DELETE FROM t",
		"SELECT * FROM query('DROP TABLE t')",
		"",
	}
	for _, q := range writes {
		if _, err := e.QuerySQL(q); err == nil {
			t.Errorf("QuerySQL(%q) = nil, want rejected", q)
		}
		unchanged(t, q)
		var buf bytes.Buffer
		if _, _, err := e.QuerySQLToCSV(q, &buf); err == nil {
			t.Errorf("QuerySQLToCSV(%q) = nil, want rejected", q)
		}
		unchanged(t, q)
		if _, err := e.CreateFromQuery("derived", q, ""); err == nil {
			t.Errorf("CreateFromQuery(%q) = nil, want rejected", q)
		}
		unchanged(t, q)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("COPY wrote %s", out)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim file overwritten: %.40q", b)
	}
	if _, err := os.Stat(logDir); err == nil {
		t.Errorf("enable_logging created %s", logDir)
	}
	// A pooled connection must not be left read-only for the next writer.
	if _, err := e.CreateFromQuery("after_reads", "SELECT 1 AS a", ""); err != nil {
		t.Errorf("write after rejected reads: %v", err)
	}
	// The refusal names what to change rather than echoing the driver's
	// "try using PrepareContext"; a bind error stays DuckDB's own.
	if _, err := e.QuerySQL("SELECT 1; DROP TABLE t"); !errors.Is(err, errNotSelect) {
		t.Errorf("multi-statement error = %v, want errNotSelect", err)
	}
	if _, err := e.QuerySQL("SELECT no_such FROM t"); err == nil || errors.Is(err, errNotSelect) || !strings.HasPrefix(err.Error(), "query: ") {
		t.Errorf("bind error = %v, want DuckDB's error prefixed with \"query: \"", err)
	}

	for i, body := range []string{
		"SELECT x * 2 AS y FROM t",
		"SELECT x FROM t;",
		"SELECT x FROM t -- trailing note",
		"WITH c AS (SELECT x FROM t) SELECT * FROM c ORDER BY x",
	} {
		name := "derived_" + string(rune('a'+i))
		if _, err := e.CreateFromQuery(name, body, ""); err != nil {
			t.Errorf("CreateFromQuery(%q) = %v, want allowed", body, err)
		}
	}
}
