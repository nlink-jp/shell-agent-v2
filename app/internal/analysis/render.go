// Package analysis — type-dispatched scalar rendering.
//
// renderScalar converts one value scanned through database/sql into
// a Go value safe for downstream JSON marshaling and human display.
// Dispatch is keyed on the upstream DuckDB type name returned by
// sql.ColumnType.DatabaseTypeName, never on a runtime Go-type
// sniff: a 16-byte UUID binary is frequently valid UTF-8 by
// coincidence, so any value-shape heuristic eventually misclassifies.
//
// See docs/en/adr/0010-duckdb-result-rendering.md for the design
// rationale, the discovery sweep that established the bug class, and
// the rejected alternatives.
package analysis

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/marcboeker/go-duckdb"
)

// renderScalar dispatches on the DuckDB type name reported by the
// driver. Types not listed here pass through unchanged — that is
// intentional: every new dispatch entry is a deliberate decision,
// and the discovery sweep test (engine_typesweep_test.go) prints
// visible drift when an unhandled type produces an unexpected Go
// value.
func renderScalar(v any, dbTypeName string) any {
	if v == nil {
		return nil
	}
	switch {
	case dbTypeName == "UUID":
		// go-duckdb v1.x returns UUID as the 16-byte binary form.
		// Format as canonical lowercase 8-4-4-4-12.
		if b, ok := v.([]byte); ok && len(b) == 16 {
			return formatUUID(b)
		}
		// Already a string (future driver versions): leave alone.
	case dbTypeName == "BLOB":
		// Keep as []byte. encoding/json marshals []byte to base64
		// (the only standard JSON binary encoding); CSV path
		// converts to base64 explicitly via csvFormat.
		return v
	case dbTypeName == "INTERVAL":
		if iv, ok := v.(duckdb.Interval); ok {
			return formatInterval(iv)
		}
	case dbTypeName == "TIME":
		if t, ok := v.(time.Time); ok {
			return t.Format("15:04:05.999999")
		}
	case strings.HasPrefix(dbTypeName, "DECIMAL"):
		if d, ok := v.(duckdb.Decimal); ok {
			return formatDecimal(d)
		}
	case strings.HasPrefix(dbTypeName, "MAP"):
		if m, ok := v.(duckdb.Map); ok {
			return mapToStringKeyed(m)
		}
	}
	// VARCHAR (and any text-like type that arrives as []byte):
	// preserve the existing convenience cast so json.Marshal emits
	// readable text instead of base64. Restricted to types we
	// haven't already handled above so a UUID or BLOB never lands
	// in this branch.
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// formatUUID renders a 16-byte UUID in canonical lowercase form.
// The byte layout matches RFC 4122 big-endian fields, which is
// what DuckDB uses on the wire.
func formatUUID(b []byte) string {
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// formatDecimal renders a duckdb.Decimal as a canonical decimal
// string. The point is inserted at position len-Scale; if Scale
// >= digit count the result is left-padded with zeros (e.g.
// Width=18,Scale=2,Value=1 → "0.01"). Sign is preserved.
func formatDecimal(d duckdb.Decimal) string {
	if d.Value == nil {
		return "0"
	}
	neg := d.Value.Sign() < 0
	abs := new(strings.Builder)
	if neg {
		abs.WriteString(d.Value.String()[1:]) // strip leading '-'
	} else {
		abs.WriteString(d.Value.String())
	}
	digits := abs.String()
	scale := int(d.Scale)
	if scale == 0 {
		if neg {
			return "-" + digits
		}
		return digits
	}
	var out string
	if len(digits) > scale {
		out = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	} else {
		out = "0." + strings.Repeat("0", scale-len(digits)) + digits
	}
	if neg {
		return "-" + out
	}
	return out
}

// formatInterval renders a duckdb.Interval in ISO-8601 duration
// form, preserving all three components even when zero. DuckDB
// does not normalise across months / days / micros (P1M30D is a
// distinct value from P2M0D), so the format must keep them
// separate. Years are rolled out from months when divisible
// because that is the natural way DuckDB INTERVAL constructors
// produce them ("INTERVAL 1 YEAR" yields 12 months).
func formatInterval(iv duckdb.Interval) string {
	years := iv.Months / 12
	months := iv.Months - years*12
	micros := iv.Micros
	hours := micros / 3_600_000_000
	micros -= hours * 3_600_000_000
	minutes := micros / 60_000_000
	micros -= minutes * 60_000_000
	seconds := micros / 1_000_000
	frac := micros - seconds*1_000_000
	var b strings.Builder
	b.WriteString("P")
	fmt.Fprintf(&b, "%dY%dM%dD", years, months, iv.Days)
	b.WriteString("T")
	fmt.Fprintf(&b, "%dH%dM", hours, minutes)
	if frac == 0 {
		fmt.Fprintf(&b, "%dS", seconds)
	} else {
		// Trim trailing zeros from the microsecond fraction to
		// keep common values readable.
		fracStr := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
		fmt.Fprintf(&b, "%d.%sS", seconds, fracStr)
	}
	return b.String()
}

// mapToStringKeyed converts duckdb.Map (map[any]any) to a
// map[string]any that encoding/json can marshal. Keys are rendered
// via fmt %v; values are passed through (nested DuckDB types in
// values are not re-dispatched in Phase 1 — see ADR-0010 §2
// non-goals).
func mapToStringKeyed(m duckdb.Map) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[fmt.Sprintf("%v", k)] = v
	}
	return out
}

// columnTypeNames extracts DatabaseTypeName for each column,
// suitable as the dispatch key for renderScalar. Returns nil
// (which is safe — renderScalar treats an empty type-name as
// "no dispatch entry, fall through") if the driver reports an
// error rather than failing the read; per-row rendering is a
// best-effort path and shouldn't abort an otherwise-successful
// SELECT.
func columnTypeNames(rows *sql.Rows) []string {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil
	}
	names := make([]string, len(cts))
	for i, ct := range cts {
		names[i] = ct.DatabaseTypeName()
	}
	return names
}

// dispatchName returns the type name at index i, or "" when the
// caller passed a nil names slice (e.g. ColumnTypes() failed).
// renderScalar treats "" as fall-through.
func dispatchName(names []string, i int) string {
	if i < len(names) {
		return names[i]
	}
	return ""
}
