package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// analysisTools returns tool definitions for data analysis.
// When no data is loaded, only load-data and reset-analysis are exposed
// to keep the tool count low for local LLMs.
func analysisTools(hasData bool) []llm.ToolDef {
	tools := []llm.ToolDef{
		{
			Name:        "load-data",
			Description: "Load a data file (CSV, JSON, JSONL) from the HOST filesystem into the analysis database. Creates or replaces the table. Only use this for absolute host paths the user supplied, or files explicitly attached to the conversation. For files inside the sandbox /work directory (anything you produced via sandbox-run-python, sandbox-write-file, sandbox-export-sql etc.), call sandbox-load-into-analysis instead — load-data cannot reach into the container.",
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
			Description: "Create a structured markdown report. Use this when the user asks for a report, summary document, or formatted output. You can include images by referencing earlier conversation images with markdown syntax.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "Report title",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Full markdown content of the report",
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
						"description": "Object ID (12-character hex)",
					},
				},
				"required": []string{"id"},
			},
		},
	}

	if !hasData {
		return tools
	}

	// Full tool set when data is loaded
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
			Description: "Execute a read-only SQL query against the analysis database. Only SELECT queries are allowed.",
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
			Description: "Generate and execute a SQL query from a natural language question. The system generates the SQL, validates it, and runs it.",
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
			Description: "Suggest 3-5 analysis perspectives for the loaded data, including what to look for and sample SQL queries.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "quick-summary",
			Description: "Execute a SQL query and generate a natural language summary of the results.",
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
			Description: "Run deep sliding-window analysis on a loaded table. Processes data in windows, accumulating findings and building a comprehensive summary. Returns a markdown report with findings grouped by severity.",
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
		// MITL: show SQL before execution. Carry the rejection
		// sentinel back so the agentLoop colours the tool-event
		// bubble red. The result text remains the user-facing
		// rejection message so the LLM understands why nothing
		// ran.
		if rejection := a.requestMITL("query-sql", argsJSON, "sql_preview"); rejection != "" {
			return rejection, ErrMITLRejected
		}
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
		// MITL: show analysis perspective before execution.
		// Same sentinel-on-rejection pattern as query-sql above.
		if rejection := a.requestMITL("analyze-data", argsJSON, "analysis_plan"); rejection != "" {
			return rejection, ErrMITLRejected
		}
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

	f := a.findings.Add(args.Content, sessionID, sessionTitle, args.Tags)
	if err := a.findings.Save(); err != nil {
		return "", fmt.Errorf("save finding: %w", err)
	}

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

	result := fmt.Sprintf("SUCCESS: Report '%s' has been created and displayed to the user. Do not explain or describe the report contents. Reply only with a brief confirmation.", args.Title)
	if reportObjectID != "" {
		result += fmt.Sprintf(" [Stored as object ID: %s]", reportObjectID)
	}
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

	// Auto-promote significant findings to global findings store
	sessionID := ""
	sessionTitle := ""
	if a.session != nil {
		sessionID = a.session.ID
		sessionTitle = a.session.Title
	}
	for _, f := range result.Findings {
		sev := strings.ToLower(f.Severity)
		content := f.Description
		if f.Evidence != "" {
			content += "\nEvidence: " + f.Evidence
		}
		a.findings.Add(content, sessionID, sessionTitle, []string{sev, tableName})
	}
	_ = a.findings.Save()

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
