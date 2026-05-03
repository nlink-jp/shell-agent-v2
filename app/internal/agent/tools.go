package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
	"github.com/nlink-jp/shell-agent-v2/internal/sandbox"
)

// analysisToolMITLDefault is the default MITL gate per analysis tool.
// Consulted by Agent.IsToolMITLRequired when no MITLOverrides entry is
// set. Centralises the policy so the Settings → Tools toggle and the
// dispatcher both see the same source of truth
// (security-hardening-2.md H1+H2).
//
// Categories:
//   - true (MITL on by default)  — host-filesystem ingest, destructive
//     operations, cross-session state mutation, query/analyze surfaces
//     that already had a UI confirmation dialog
//   - false                       — pure metadata reads or local-only
//     artefact creation
var analysisToolMITLDefault = map[string]bool{
	"load-data":        true,  // host-file ingest into the analysis DB
	"reset-analysis":   true,  // drops every table in the session
	"promote-finding":  true,  // mutates the global findings store
	"query-sql":        true,  // SQL preview dialog already in place
	"analyze-data":     true,  // analysis-plan dialog already in place
	"create-report":    false, // local artefact in objstore
	"describe-data":    false, // metadata read
	"list-tables":      false, // metadata read
	"query-preview":    false, // NL → SQL only, doesn't execute
	"suggest-analysis": false, // LLM-side suggestion, no state change
	"quick-summary":    false, // SELECT + summarise, no mutation
	"register-object":  false, // moves a /work file into objstore — same trust level as a drag-and-drop
}

// analysisToolMITLCategory returns the human-readable category passed
// to requestMITL for analysis tools. The frontend special-cases
// "sql_preview" and "analysis_plan" to render a SQL preview dialog
// and an analysis-plan dialog respectively; everything else falls
// back to the standard execute / write confirmation.
func analysisToolMITLCategory(name string) string {
	switch name {
	case "query-sql":
		return "sql_preview"
	case "analyze-data":
		return "analysis_plan"
	case "load-data", "reset-analysis":
		return "execute"
	default:
		return "write"
	}
}

// analysisTools returns tool definitions for data analysis.
//
// All 11 tools are exposed every round so the LLM can plan a
// load-then-analyse workflow up front (see
// docs/en/agent-tool-visibility.md). The dispatcher's
// executeAnalysisTool already handles the empty-session case for
// each tool: when something like query-sql is called against a
// fresh session, the underlying engine returns an explicit
// "no tables loaded" error the model can react to.
//
// The legacy hasData-based filter is preserved behind
// hideUntilDataLoaded for users on weaker local backends; passing
// true restores the pre-v0.1.21 behaviour where the
// data-dependent half of the set only appears after a successful
// load-data.
func analysisTools(hasData, hideUntilDataLoaded bool) []llm.ToolDef {
	tools := []llm.ToolDef{
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
		},
		{
			Name:        "reset-analysis",
			Description: "Drop all tables and clear analysis data for the current session.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
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
		},
		{
			Name:        "list-objects",
			Description: "List all objects (images, files, reports) in the current session. Returns ID, type, filename, and creation time.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type_filter": map[string]any{
						"type":        "string",
						"enum":        []string{"image", "blob", "report", "all"},
						"description": "Filter by object type (default: all)",
					},
				},
			},
		},
		{
			Name:        "get-object",
			Description: "Retrieve an object by ID. For images, returns a marker that will be resolved to the actual image. For text/data, returns the content directly.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Object ID (hex string, currently 32 chars; legacy 12-char IDs continue to work)",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "register-object",
			Description: "Register a file already present in the session work directory ($SHELL_AGENT_WORK_DIR — same physical path that the sandbox sees as /work) into the central object store, returning an object:<ID> reference the chat can render. Use this to surface artefacts produced by shell tools (e.g. generate-image): write to $SHELL_AGENT_WORK_DIR from the shell tool, then call this with the same filename. For artefacts produced by sandbox-run-python / sandbox-run-shell, prefer sandbox-register-object (both end up reading from the same physical directory). Design: docs/en/work-dir-shell-bridge.md.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path inside the work directory. Relative paths only; '..' traversal is rejected. e.g. 'sunset.png'",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Human-readable name for the object (shown in the Data panel). Falls back to the basename if omitted.",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"image", "blob", "report"},
						"description": "Object type. If omitted, inferred from the file's MIME (image/* → image, text/markdown → report, otherwise blob).",
					},
				},
				"required": []string{"path"},
			},
		},
	}

	// Legacy mode (opt-in via cfg.Tools.HideAnalysisToolsUntilDataLoaded):
	// only the load-data half until a successful load. The new default
	// falls through and exposes the full set every round so the LLM can
	// plan multi-step workflows up front.
	if hideUntilDataLoaded && !hasData {
		return tools
	}

	// Full tool set
	dataTools := []llm.ToolDef{
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
		},
		{
			Name:        "list-tables",
			Description: "List all tables in the analysis database with their metadata.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
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
		},
		{
			Name:        "suggest-analysis",
			Description: "Brainstorm 3-5 analysis angles you could pursue against the loaded data — for each: a title, what to look for, and a sample SQL query. Returns markdown text. Does NOT execute any SQL on its own; pick one of the suggestions and run it via query-sql or query-preview. Use this at the start of an exploration when neither you nor the user knows yet what's interesting in the data.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
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
		},
		{
			Name:        "promote-finding",
			Description: "Promote an analysis insight to the global findings store so it can be referenced across sessions. Use this when you discover a significant result worth remembering.",
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
		},
		{
			Name:        "analyze-data",
			Description: "Run deep sliding-window analysis on a loaded table — the agent processes data in chunks, asking the LLM to surface findings on each window, accumulating them, and building a comprehensive summary grouped by severity. Returns a markdown report. Heaviest of the analysis tools (multiple LLM calls). Use this when the user wants a thorough audit-style review (e.g. 'find anomalies', 'summarise this dataset's risks'); for a one-shot SQL + summary, use quick-summary; for brainstorming angles only, use suggest-analysis.",
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
		},
	}

	return append(tools, dataTools...)
}

// executeAnalysisTool handles analysis tool calls.
func (a *Agent) executeAnalysisTool(ctx context.Context, name string, argsJSON string) (string, error) {
	switch name {
	case "load-data":
		return a.toolLoadData(argsJSON)
	case "describe-data":
		return a.toolDescribeData(argsJSON)
	case "query-sql":
		// MITL is now gated by the dispatcher via IsToolMITLRequired
		// so the Settings → Tools toggle takes effect. See
		// security-hardening-2.md H1+H2 / agent.go:1231.
		return a.toolQuerySQL(argsJSON)
	case "list-tables":
		return a.toolListTables()
	case "query-preview":
		return a.toolQueryPreview(ctx, argsJSON)
	case "suggest-analysis":
		return a.toolSuggestAnalysis(ctx)
	case "quick-summary":
		return a.toolQuickSummary(ctx, argsJSON)
	case "reset-analysis":
		return a.toolResetAnalysis()
	case "create-report":
		return a.toolCreateReport(argsJSON)
	case "promote-finding":
		return a.toolPromoteFinding(argsJSON)
	case "analyze-data":
		// MITL is now gated by the dispatcher via IsToolMITLRequired
		// (security-hardening-2.md H1+H2 / agent.go:1231).
		return a.toolAnalyzeData(ctx, argsJSON)
	default:
		return "", fmt.Errorf("unknown analysis tool: %s", name)
	}
}

func (a *Agent) toolLoadData(argsJSON string) (string, error) {
	var args struct {
		FilePath  string `json:"file_path"`
		TableName string `json:"table_name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.FilePath == "" || args.TableName == "" {
		return "", fmt.Errorf("file_path and table_name are required")
	}

	if err := a.analysis.LoadFile(args.TableName, args.FilePath); err != nil {
		return "", err
	}

	tables := a.analysis.Tables()
	for _, t := range tables {
		if t.Name == args.TableName {
			return fmt.Sprintf("Loaded table %q: %d rows, columns: %v",
				t.Name, t.RowCount, t.Columns), nil
		}
	}
	return fmt.Sprintf("Loaded table %q", args.TableName), nil
}

func (a *Agent) toolDescribeData(argsJSON string) (string, error) {
	var args struct {
		TableName      string `json:"table_name"`
		SetDescription string `json:"set_description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if args.SetDescription != "" {
		if err := a.analysis.SetTableDescription(args.TableName, args.SetDescription); err != nil {
			return "", err
		}
	}

	tables := a.analysis.Tables()
	for _, t := range tables {
		if t.Name == args.TableName {
			return formatTableMeta(t), nil
		}
	}
	return "", fmt.Errorf("table %q not found", args.TableName)
}

func (a *Agent) toolQuerySQL(argsJSON string) (string, error) {
	var args struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	results, err := a.analysis.QuerySQL(args.SQL)
	if err != nil {
		return "", err
	}

	if len(results) == 0 {
		return "(no results)", nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return string(data), nil
}

func (a *Agent) toolListTables() (string, error) {
	tables := a.analysis.Tables()
	if len(tables) == 0 {
		return "No tables loaded.", nil
	}

	var sb fmt.Stringer = &tableListBuilder{tables: tables}
	return sb.String(), nil
}

type tableListBuilder struct {
	tables []*analysis.TableMeta
}

func (b *tableListBuilder) String() string {
	var result string
	for _, t := range b.tables {
		result += formatTableMeta(t) + "\n"
	}
	return result
}

func (a *Agent) toolResetAnalysis() (string, error) {
	if err := a.analysis.Reset(); err != nil {
		return "", err
	}
	return "All tables dropped. Analysis data cleared.", nil
}

func (a *Agent) toolPromoteFinding(argsJSON string) (string, error) {
	var args struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	sessionID := ""
	sessionTitle := ""
	if a.session != nil {
		sessionID = a.session.ID
		sessionTitle = a.session.Title
	}

	if a.findings == nil {
		return "", fmt.Errorf("no session loaded")
	}
	_ = sessionID
	_ = sessionTitle
	f := a.findings.Add(args.Content, args.Tags, findings.SourceLLMPromoted, true)
	if err := a.findings.Save(); err != nil {
		return "", fmt.Errorf("save finding: %w", err)
	}
	a.notifyFindingsUpdated()

	return fmt.Sprintf("Finding promoted: %s (%s)", f.Content, f.CreatedLabel), nil
}

func (a *Agent) toolQueryPreview(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	schema := a.analysis.Schema()
	if schema == "" {
		return "", fmt.Errorf("no tables loaded")
	}

	// Generate SQL via LLM
	messages := []llm.Message{
		{Role: "system", Content: "Generate a SQL query for DuckDB to answer the user's question. " +
			"Only generate SELECT statements. Never generate INSERT, UPDATE, DELETE, DROP, or any DDL statements. " +
			"Respond with ONLY the SQL query. No explanation, no markdown fences.\n\n" +
			"Database schema:\n" + schema},
		{Role: "user", Content: args.Question},
	}

	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		return "", fmt.Errorf("SQL generation: %w", err)
	}

	sql := strings.TrimSpace(resp.Content)
	sql = strings.TrimPrefix(sql, "```sql")
	sql = strings.TrimPrefix(sql, "```")
	sql = strings.TrimSuffix(sql, "```")
	sql = strings.TrimSpace(sql)

	// Execute the generated SQL
	results, err := a.analysis.QuerySQL(sql)
	if err != nil {
		return "", fmt.Errorf("query execution: %w (SQL: %s)", err, sql)
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return fmt.Sprintf("Generated SQL:\n```sql\n%s\n```\n\nResults (%d rows):\n%s", sql, len(results), string(data)), nil
}

func (a *Agent) toolSuggestAnalysis(ctx context.Context) (string, error) {
	schema := a.analysis.Schema()
	if schema == "" {
		return "", fmt.Errorf("no tables loaded")
	}

	messages := []llm.Message{
		{Role: "system", Content: "You are a data analyst. Given the database schema, suggest 3-5 analysis perspectives. " +
			"For each, provide: a title, what to look for, and a sample SQL query. " +
			"Use the same language as the table/column names suggest. Format as markdown."},
		{Role: "user", Content: "Database schema:\n" + schema},
	}

	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		return "", fmt.Errorf("suggest analysis: %w", err)
	}

	return resp.Content, nil
}

func (a *Agent) toolQuickSummary(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	results, err := a.analysis.QuerySQL(args.SQL)
	if err != nil {
		return "", err
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	resultText := string(data)
	if len(resultText) > 30000 {
		resultText = resultText[:30000] + "\n... (truncated)"
	}

	// Summarize via LLM
	messages := []llm.Message{
		{Role: "system", Content: "Summarize the following SQL query results in natural language. " +
			"Highlight key patterns, outliers, and insights. Be concise. " +
			"Use the same language as the data suggests."},
		{Role: "user", Content: fmt.Sprintf("SQL:\n```sql\n%s\n```\n\nResults (%d rows):\n%s", args.SQL, len(results), resultText)},
	}

	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		return fmt.Sprintf("Results (%d rows):\n%s\n\n(Summary generation failed: %v)", len(results), resultText, err), nil
	}

	return fmt.Sprintf("```sql\n%s\n```\n\nResults: %d rows\n\n%s", args.SQL, len(results), resp.Content), nil
}

// toolCreateReport creates a report and stores it in session records.
// Design: docs/en/agent-data-flow.md Section 6
func (a *Agent) toolCreateReport(argsJSON string) (string, error) {
	var args struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	reportContent := fmt.Sprintf("# %s\n\n%s", args.Title, args.Content)

	// Save report to objstore as TypeReport
	var reportObjectID string
	if a.objects != nil {
		sessionID := ""
		if a.session != nil {
			sessionID = a.session.ID
		}
		meta, err := a.objects.Store(
			strings.NewReader(reportContent),
			objstore.TypeReport, "text/markdown", args.Title+".md", sessionID,
		)
		if err == nil {
			reportObjectID = meta.ID
		}
	}

	// Store report in session records with ObjectID reference
	if a.session != nil {
		a.session.AddReportMessage(args.Title, reportContent)
		if reportObjectID != "" {
			last := &a.session.Records[len(a.session.Records)-1]
			last.ObjectIDs = []string{reportObjectID}
		}
	}

	// Notify frontend for immediate display
	a.mu.Lock()
	h := a.reportHandler
	a.mu.Unlock()
	if h != nil {
		h(args.Title, reportContent)
	}

	// Do NOT include the report's object ID in the tool result.
	// The report is already shown to the user via reportHandler;
	// leaking the ID prompts the LLM to write `[link](object:ID)`
	// into its chat reply, which then renders as a redundant link
	// pointing at content the user is already looking at.
	_ = reportObjectID
	result := fmt.Sprintf("SUCCESS: Report '%s' has been created and displayed to the user. Do not explain or describe the report contents. Reply only with a brief confirmation.", args.Title)
	return result, nil
}

// toolListObjects lists objects in the current session.
func (a *Agent) toolListObjects(argsJSON string) string {
	if a.objects == nil {
		return "No object store available."
	}

	var args struct {
		TypeFilter string `json:"type_filter"`
	}
	json.Unmarshal([]byte(argsJSON), &args)

	var objs []*objstore.ObjectMeta
	sessionID := ""
	if a.session != nil {
		sessionID = a.session.ID
	}

	if sessionID != "" {
		objs = a.objects.ListBySession(sessionID)
	} else {
		objs = a.objects.All()
	}

	// Filter by type if specified
	if args.TypeFilter != "" && args.TypeFilter != "all" {
		var filtered []*objstore.ObjectMeta
		for _, o := range objs {
			if string(o.Type) == args.TypeFilter {
				filtered = append(filtered, o)
			}
		}
		objs = filtered
	}

	if len(objs) == 0 {
		return "No objects found."
	}

	var sb strings.Builder
	for _, o := range objs {
		name := o.OrigName
		if name == "" {
			name = o.ID
		}
		sb.WriteString(fmt.Sprintf("- ID: %s | Type: %s | Name: %s | Size: %d bytes | Created: %s\n",
			o.ID, o.Type, name, o.Size, o.CreatedAt.Format("2006-01-02 15:04:05")))
	}
	return sb.String()
}

// toolGetObject retrieves an object by ID.
func (a *Agent) toolGetObject(argsJSON string) string {
	if a.objects == nil {
		return "Error: no object store available"
	}

	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	meta, ok := a.objects.Get(args.ID)
	if !ok {
		return fmt.Sprintf("Error: object %s not found", args.ID)
	}

	// Images: return recall marker (resolved by message builder)
	if meta.Type == objstore.TypeImage {
		return fmt.Sprintf("__IMAGE_RECALL_BLOB__%s__", args.ID)
	}

	// Text/data: return content directly (truncated)
	data, err := a.objects.ReadData(args.ID)
	if err != nil {
		return fmt.Sprintf("Error reading object: %v", err)
	}
	defer data.Close()

	buf := make([]byte, 30000)
	n, _ := data.Read(buf)
	content := string(buf[:n])
	if n >= 30000 {
		content += "\n... (truncated)"
	}
	return content
}

// toolRegisterObject reads a file from the session work directory
// (the same physical path the sandbox bind-mounts at /work) and
// registers it into objstore, returning the new object's ID. Used
// by shell tools that wrote artefacts via $SHELL_AGENT_WORK_DIR
// when the sandbox isn't running. Mirrors the effect of
// sandbox-register-object — both read from the same host directory
// and call the same objstore.Store.
//
// Path validation reuses the sandbox-side `safeWorkPath` helper so
// `..` traversal, absolute paths, and symlink leaves are all
// refused (security-hardening-2 §3.2.1 / H14 pattern). Design:
// docs/en/work-dir-shell-bridge.md.
func (a *Agent) toolRegisterObject(argsJSON string) (string, ActivityEventStatus) {
	var args struct {
		Path string `json:"path"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "Error: invalid arguments: " + err.Error(), ActivityStatusError
	}
	if args.Path == "" {
		return "Error: 'path' is required", ActivityStatusError
	}
	if a.objects == nil {
		return "Error: object store not available", ActivityStatusError
	}
	if a.session == nil {
		return "Error: no active session", ActivityStatusError
	}

	workDir := a.sessionWorkDir()
	src, err := safeWorkPath(workDir, args.Path)
	if err != nil {
		return "Error: " + err.Error(), ActivityStatusError
	}
	f, err := os.Open(src)
	if err != nil {
		return "Error: open source: " + err.Error(), ActivityStatusError
	}
	defer f.Close()

	mime := sandbox.MimeFromPath(args.Path)
	objType := args.Type
	if objType == "" {
		objType = sandbox.ObjectTypeForMIME(mime)
	}
	name := args.Name
	if name == "" {
		name = filepath.Base(args.Path)
	}
	meta, err := a.objects.Store(f, objstore.ObjectType(objType), mime, name, a.session.ID)
	if err != nil {
		return "Error: store: " + err.Error(), ActivityStatusError
	}
	return fmt.Sprintf("registered as object %s (%s, %s)", meta.ID, objType, humanSize(meta.Size)), ActivityStatusSuccess
}

// toolAnalyzeData runs sliding window analysis on a loaded table.
func (a *Agent) toolAnalyzeData(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Prompt string `json:"prompt"`
		Table  string `json:"table"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if a.analysis == nil {
		return "", fmt.Errorf("no analysis engine")
	}

	// Determine target table
	tableName := args.Table
	if tableName == "" {
		tables := a.analysis.Tables()
		if len(tables) == 0 {
			return "No tables loaded. Use load-data first.", nil
		}
		tableName = tables[0].Name
	}

	// Fetch all rows
	query := fmt.Sprintf("SELECT * FROM \"%s\"", tableName)
	results, err := a.analysis.QuerySQL(query)
	if err != nil {
		return "", fmt.Errorf("query table: %w", err)
	}
	rows := analysis.RowsToJSON(results)

	// Build LLM adapter
	adapter := &backendLLMAdapter{backend: a.backend}

	// Run analysis
	cfg := analysis.DefaultSummarizerConfig()
	summarizer := analysis.NewSummarizer(adapter, a.analysis.Schema(), cfg)
	result, err := summarizer.Analyze(ctx, args.Prompt, rows, func(idx, total int) {
		a.emitActivity(ActivityEvent{
			Type:   "tool_start",
			Detail: fmt.Sprintf("analyze-data (window %d/%d)", idx+1, total),
		})
	})
	if err != nil {
		return "", fmt.Errorf("analysis: %w", err)
	}

	// Auto-promote analyze-data findings to the per-session findings store
	if a.findings == nil {
		return "", fmt.Errorf("no session loaded")
	}
	for _, f := range result.Findings {
		sev := strings.ToLower(f.Severity)
		content := f.Description
		if f.Evidence != "" {
			content += "\nEvidence: " + f.Evidence
		}
		a.findings.Add(content, []string{sev, tableName}, findings.SourceAnalyzeData, true)
	}
	_ = a.findings.Save()
	if len(result.Findings) > 0 {
		a.notifyFindingsUpdated()
	}

	// Generate report
	report := analysis.GenerateReport(args.Prompt, result)

	// Truncate if too long for tool result
	if len(report) > 10000 {
		report = report[:10000] + "\n\n... (truncated)"
	}

	return report, nil
}

// backendLLMAdapter adapts llm.Backend to analysis.LLMClient.
type backendLLMAdapter struct {
	backend llm.Backend
}

func (a *backendLLMAdapter) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: userPrompt},
	}
	resp, err := a.backend.Chat(ctx, messages, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func formatTableMeta(t *analysis.TableMeta) string {
	desc := t.Description
	if desc == "" {
		desc = "(no description)"
	}
	return fmt.Sprintf("Table: %s\n  Description: %s\n  Rows: %d\n  Columns: %v",
		t.Name, desc, t.RowCount, t.Columns)
}
