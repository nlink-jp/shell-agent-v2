package agent

import (
	"encoding/json"
	"fmt"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
)

// analysisTools returns tool definitions for data analysis.
// When no data is loaded, only load-data and reset-analysis are exposed
// to keep the tool count low for local LLMs.
func analysisTools(hasData bool) []llm.ToolDef {
	tools := []llm.ToolDef{
		{
			Name:        "load-data",
			Description: "Load a CSV file into the analysis database. Creates or replaces the table.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{
						"type":        "string",
						"description": "Path to the CSV file to load",
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
	}

	return append(tools, dataTools...)
}

// executeAnalysisTool handles analysis tool calls.
func (a *Agent) executeAnalysisTool(name string, argsJSON string) (string, error) {
	switch name {
	case "load-data":
		return a.toolLoadData(argsJSON)
	case "describe-data":
		return a.toolDescribeData(argsJSON)
	case "query-sql":
		return a.toolQuerySQL(argsJSON)
	case "list-tables":
		return a.toolListTables()
	case "reset-analysis":
		return a.toolResetAnalysis()
	case "promote-finding":
		return a.toolPromoteFinding(argsJSON)
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

	if err := a.analysis.LoadCSV(args.TableName, args.FilePath); err != nil {
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

func formatTableMeta(t *analysis.TableMeta) string {
	desc := t.Description
	if desc == "" {
		desc = "(no description)"
	}
	return fmt.Sprintf("Table: %s\n  Description: %s\n  Rows: %d\n  Columns: %v",
		t.Name, desc, t.RowCount, t.Columns)
}
