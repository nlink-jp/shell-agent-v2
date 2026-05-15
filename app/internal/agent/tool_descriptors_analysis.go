// tool_descriptors_analysis.go — analysis-tool descriptors
// (the v0.6 single source of truth for the 14 tools that
// were previously split across analysisTools() builder,
// analysisToolMITLDefault map, analysisToolMITLCategory()
// switch, executeAnalysisTool() switch, executeTool() outer
// case-label, and ListTools() analysis section).
//
// Phase 2a of the refactor: this file defines the
// descriptors. No view function consumes them yet — the
// existing parallel lists are still the active source of
// truth. Phase 2d-2i incrementally migrate each view to
// derive from the descriptors instead, then delete the
// parallel lists.
//
// The Description and Parameters values are intentionally
// duplicated from the live analysisTools() builder for the
// duration of the migration (one commit's lifetime). After
// 2d the live builder reads from this descriptor list and
// the duplication disappears.
//
// See docs/en/adr/0007-tool-registry-refactor.md.

package agent

import (
	"context"
)

// analysisDescriptors returns the 14 analysis-engine tools
// as ToolDescriptor values: 6 always-visible
// (load-data + reset-analysis + create-report + the 3 v0.5
// text tools) and 8 data-gated (the rest of the data-
// dependent tools that the legacy hideUntilDataLoaded mode
// hides until first load-data succeeds).
//
// The 4 builtin tools (resolve-date, list-objects,
// get-object, register-object) live in builtinDescriptors()
// — they're listed under analysisTools() today for
// historical reasons but are dispatched directly by
// executeTool() without going through the analysis engine.
func (a *Agent) analysisDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
		// --- Always-visible (6) ---
		{
			Name:        "load-data",
			Description: "Load a data file (CSV, JSON, JSONL) from the HOST filesystem into the analysis database. Creates or replaces the table. Only use this for absolute host paths the user supplied, or files explicitly attached to the conversation. For files inside the sandbox /work directory (anything you produced via sandbox-run-python, sandbox-write-file, sandbox-export-sql etc.), call sandbox-load-into-analysis instead — load-data cannot reach into the container. Once loaded, the table is queryable via `query-sql`, `describe-data`, `list-tables`, `query-preview`, `suggest-analysis`, `quick-summary`, and `analyze-data`; use `promote-finding` to save insights, `create-report` to assemble a report.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{
						"type":        "string",
						"description": "Absolute path on the host. NOT a /work/ path (use sandbox-load-into-analysis for those).",
					},
					"table_name": map[string]any{
						"type":        "string",
						"description": "Name for the table (alphanumeric and underscores only)",
					},
				},
				"required": []string{"file_path", "table_name"},
			},
			Category:             "read", // matches existing ListTools entry
			Source:               "analysis",
			MITLDefault:          true,
			MITLCategoryOverride: "execute", // analysisToolMITLCategory("load-data") returns "execute"
			HideUntilDataLoaded:  false,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolLoadData(args)
			}),
		},
		{
			Name:        "reset-analysis",
			Description: "Drop all tables and clear analysis data for the current session.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Category:             "write", // destructive
			Source:               "analysis",
			MITLDefault:          true,
			MITLCategoryOverride: "execute", // analysisToolMITLCategory("reset-analysis") returns "execute"
			HideUntilDataLoaded:  false,
			Handle: wrapErrHandler(func(_ context.Context, _ string) (string, error) {
				return a.toolResetAnalysis()
			}),
		},
		{
			Name:        "create-report",
			Description: "Create a structured markdown report. Use this when the user asks for a report, summary document, or formatted output. Write GitHub-flavored Markdown only — do NOT emit raw HTML tags (`<br>`, `<table>`, `<details>`, `<sub>`, etc.); the renderer escapes them and they appear as plain text. Use markdown tables, lists, fenced code blocks, and headings instead. Reference images with `![alt](object:ID)`.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "Report title",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Full report body in GitHub-flavored Markdown. No raw HTML tags.",
					},
				},
				"required": []string{"title", "content"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: false,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolCreateReport(args)
			}),
		},
		{
			Name:        "analyze-text",
			Description: "Run sliding-window analysis over a markdown / text attachment (TypeMarkdown — user-attached) or a previously generated report (TypeReport — your own prior output via create-report). Same pipeline as analyze-data, but the chunks are text segments rather than DuckDB rows. Findings are auto-promoted into the session findings store tagged with the object ID. Heavy: many LLM calls for large documents. Use grep-text for keyword search, get-text for verbatim line reads.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{
						"type":        "string",
						"description": "Object ID, either bare (e.g. \"abc123\") or \"object:abc123\". Must be a TypeMarkdown or TypeReport object — use list-objects to discover.",
					},
					"perspective": map[string]any{
						"type":        "string",
						"description": "Analysis perspective, in the user's language (e.g. \"List anomalies in this audit log\", \"監査ログの不正アクセス痕跡を抽出\").",
					},
					"lines": map[string]any{
						"type":        "string",
						"description": "Optional line range to restrict the analysis to (e.g. \"1-5000\", \"10000-\", \"-500\"). Defaults to the whole document.",
					},
				},
				"required": []string{"object", "perspective"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: false, // text tools are objstore-driven, not DuckDB-driven (design §7.4)
			Handle: wrapErrHandler(func(ctx context.Context, args string) (string, error) {
				return a.toolAnalyzeText(ctx, args)
			}),
		},
		{
			Name:        "grep-text",
			Description: "Regex search across a markdown or report object. Returns matching lines with line numbers and configurable context lines (-A / -B equivalent). If the match count exceeds max_matches, returns an error suggesting you narrow the pattern or restrict via the lines argument — much like ripgrep would. Use this when you need to FIND something specific in a document; use analyze-text for narrative summarisation.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{
						"type":        "string",
						"description": "Object ID, bare or \"object:<id>\".",
					},
					"pattern": map[string]any{
						"type":        "string",
						"description": "RE2 regular expression. Examples: \"error|fatal\", \"^## \", \"\\\\bdeadline\\\\b\".",
					},
					"lines": map[string]any{
						"type":        "string",
						"description": "Optional line range to restrict the search (same syntax as analyze-text).",
					},
					"max_matches": map[string]any{
						"type":        "integer",
						"description": "Cap on returned matches before erroring. Default 200.",
					},
					"context_lines": map[string]any{
						"type":        "integer",
						"description": "Lines of context printed around each match (both before and after). Default 2.",
					},
				},
				"required": []string{"object", "pattern"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: false,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolGrepText(args)
			}),
		},
		{
			Name:        "get-text",
			Description: "Read a specific line range from a markdown or report object verbatim, with line numbers prefixed for unambiguous citation. Hard cap of 1000 lines per call — for longer ranges use analyze-text (summarised) or call get-text in chunks.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{
						"type":        "string",
						"description": "Object ID, bare or \"object:<id>\".",
					},
					"lines": map[string]any{
						"type":        "string",
						"description": "Line range (1-based, inclusive). Examples: \"42\" (single line), \"100-200\", \"500-\" (from line 500 to end), \"-50\" (first 50 lines).",
					},
				},
				"required": []string{"object", "lines"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: false,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolGetText(args)
			}),
		},
		// --- Data-gated (8) ---
		{
			Name:        "describe-data",
			Description: "Show table metadata: columns, row count, and description. Optionally set a description.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"table_name": map[string]any{
						"type":        "string",
						"description": "Table name to describe",
					},
					"set_description": map[string]any{
						"type":        "string",
						"description": "Optional: set a new description for the table",
					},
				},
				"required": []string{"table_name"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: true,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolDescribeData(args)
			}),
		},
		{
			Name:        "query-sql",
			Description: "Execute a read-only SQL SELECT query you write yourself against the analysis database. Returns the raw rows. Use when you already know the SQL — fastest of the query tools, no extra LLM round-trip. If you don't know the SQL yet, use query-preview (natural language → SQL) instead. If you want a narrative interpretation of the results, use quick-summary instead. INSERT/UPDATE/DELETE/DROP/DDL are rejected.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql": map[string]any{
						"type":        "string",
						"description": "SQL SELECT query to execute",
					},
				},
				"required": []string{"sql"},
			},
			Category:             "read",
			Source:               "analysis",
			MITLDefault:          true,
			MITLCategoryOverride: "sql_preview",
			HideUntilDataLoaded:  true,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolQuerySQL(args)
			}),
		},
		{
			Name:        "list-tables",
			Description: "List all tables in the analysis database with their metadata.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: true,
			Handle: wrapErrHandler(func(_ context.Context, _ string) (string, error) {
				return a.toolListTables()
			}),
		},
		{
			Name:        "query-preview",
			Description: "Ask a question about the data in natural language; the agent uses the LLM to generate a SELECT query against the loaded schema, runs it, and returns BOTH the generated SQL and the raw result rows. Use this for exploratory asks where you don't yet know the right SQL — e.g. 'show monthly sales totals' or 'which products sell best'. Costs one extra LLM round-trip vs query-sql; if you already know the SQL, prefer query-sql. If you want a narrative summary instead of raw rows, use quick-summary.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{
						"type":        "string",
						"description": "Natural language question about the data",
					},
				},
				"required": []string{"question"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: true,
			Handle: wrapErrHandler(func(ctx context.Context, args string) (string, error) {
				return a.toolQueryPreview(ctx, args)
			}),
		},
		{
			Name:        "suggest-analysis",
			Description: "Brainstorm 3-5 analysis angles you could pursue against the loaded data — for each: a title, what to look for, and a sample SQL query. Returns markdown text. Does NOT execute any SQL on its own; pick one of the suggestions and run it via query-sql or query-preview. Use this at the start of an exploration when neither you nor the user knows yet what's interesting in the data.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: true,
			Handle: wrapErrHandler(func(ctx context.Context, _ string) (string, error) {
				return a.toolSuggestAnalysis(ctx)
			}),
		},
		{
			Name:        "quick-summary",
			Description: "Run a SELECT (you provide the SQL) and get back BOTH the row count + a one-shot natural-language summary of patterns, outliers, and insights generated by the LLM. Use this when the user wants a narrative interpretation rather than raw rows in one step. If you only need rows, use query-sql; if you don't know the SQL yet, use query-preview. For a deeper, multi-window analysis with accumulated findings, use analyze-data instead.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql": map[string]any{
						"type":        "string",
						"description": "SQL SELECT query to execute and summarize",
					},
				},
				"required": []string{"sql"},
			},
			Category:            "read",
			Source:              "analysis",
			MITLDefault:         false,
			HideUntilDataLoaded: true,
			Handle: wrapErrHandler(func(ctx context.Context, args string) (string, error) {
				return a.toolQuickSummary(ctx, args)
			}),
		},
		{
			Name:        "promote-finding",
			Description: "Promote an analysis insight to the per-session findings store. Use this when you discover a significant result worth remembering. Write `content` in the same language the user is using in the current conversation (e.g. 日本語 if the user is speaking Japanese) — these findings are surfaced directly in the user's chat-pane panel. Avoid promoting near-duplicates of insights already surfaced (the store dedups, but cosmetic re-wording wastes a tool round).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The insight or finding to save",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional tags for categorization",
					},
				},
				"required": []string{"content"},
			},
			Category:            "write",
			Source:              "analysis",
			MITLDefault:         true,
			HideUntilDataLoaded: true,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolPromoteFinding(args)
			}),
		},
		{
			Name:        "save-query",
			Description: "Run a SELECT query and save its result as a new derived table for further analysis. Use this when you want to analyze-data over a filtered subset (e.g. WHERE status='failed' AND ts >= '2026-05-01') — save-query the filter, then pass the new table name to analyze-data. The derived table appears in list-tables alongside loaded tables. Errors on name collision to avoid accidentally overwriting a loaded table; if collision happens, pick a fresh name with a suffix like _v2, _filtered, or _derived.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql": map[string]any{
						"type":        "string",
						"description": "SELECT statement defining the rows to save",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Name for the new table (alphanumeric and underscores only, starts with a letter or underscore)",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "Optional purpose description, shown in describe-data later",
					},
				},
				"required": []string{"sql", "name"},
			},
			Category:             "write",
			Source:               "analysis",
			MITLDefault:          true,
			MITLCategoryOverride: "sql_preview",
			HideUntilDataLoaded:  true,
			Handle: wrapErrHandler(func(_ context.Context, args string) (string, error) {
				return a.toolSaveQuery(args)
			}),
		},
		{
			Name:        "analyze-data",
			Description: "Run deep sliding-window analysis on a loaded table — the agent processes data in chunks, asking the LLM to surface findings on each window, accumulating them, and building a comprehensive summary grouped by severity. Returns a markdown report. Heaviest of the analysis tools (multiple LLM calls). Use this when the user wants a thorough audit-style review (e.g. 'find anomalies', 'summarise this dataset's risks'); for a one-shot SQL + summary, use quick-summary; for brainstorming angles only, use suggest-analysis. For filtered analysis, use `save-query` first to materialise a SELECT result as a derived table, then pass that table's name here.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "Analysis perspective and what to look for (e.g. 'Find anomalies in response times')",
					},
					"table": map[string]any{
						"type":        "string",
						"description": "Table to analyze (default: first loaded table)",
					},
				},
				"required": []string{"prompt"},
			},
			Category:             "read",
			Source:               "analysis",
			MITLDefault:          true,
			MITLCategoryOverride: "analysis_plan",
			HideUntilDataLoaded:  true,
			Handle: wrapErrHandler(func(ctx context.Context, args string) (string, error) {
				return a.toolAnalyzeData(ctx, args)
			}),
		},
	}
}
