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
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
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
// analysisToolMITLDefault was the v0.6 single source of
// truth for analysis-tool MITL defaults. v0.6 moves the
// per-tool MITLDefault flag into ToolDescriptor so the
// IsToolMITLRequired lookup and the Settings UI default
// derive from the same value — drift impossible by
// construction. Map deleted; see analysisDescriptors() /
// builtinDescriptors() for the per-tool values.

// analysisToolMITLCategory was the v0.5 free-function
// switch that mapped tool name → frontend MITL dialog
// category (sql_preview / analysis_plan / execute / write).
// v0.6 moves the override into ToolDescriptor (MITLCategory
// Override + Category fallback) and the dispatcher calls
// a.toolMITLCategory(name) which reads from there. Function
// deleted.

// analysisTools was the v0.5 hand-coded LLM tool-def
// builder. v0.6 derives the same output from
// a.toolDescriptors via descriptorToolDefs() — see
// tool_descriptor.go and tool_descriptors_analysis.go.

// executeAnalysisTool was the v0.5 inner dispatcher for the
// 14-case switch of analysis tool names. Phase 2e replaced
// the switch with descriptor lookup, then Phase 2f replaced
// the outer call site with dispatchDescriptor — leaving this
// function unused. Deleted.

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

// toolSaveQuery materialises a SELECT result as a new derived
// base table via Engine.CreateFromQuery. The derived table is
// then queryable / describable / analyzable through every
// existing analysis tool because the engine treats it
// identically to a load-data loaded table. See
// docs/en/adr/0013-saved-query-tables.md.
func (a *Agent) toolSaveQuery(argsJSON string) (string, error) {
	var args struct {
		SQL         string `json:"sql"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.SQL == "" {
		return "", fmt.Errorf("sql is required")
	}
	if args.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	if a.analysis == nil {
		return "", fmt.Errorf("no analysis engine")
	}

	meta, err := a.analysis.CreateFromQuery(args.Name, args.SQL, args.Description)
	if err != nil {
		return "", err
	}

	out := formatTableMeta(meta)
	// Surface a warning when the derived table exceeds the
	// analyze-data backstop so the LLM knows to narrow the
	// filter before chaining analyze-data. The CREATE itself
	// succeeded — query-sql / describe-data on a >1M-row table
	// still work — so this is advisory, not an error.
	if meta.RowCount > int64(analysis.MaxAnalyzeRows) {
		out += fmt.Sprintf("\n\nNote: %d rows exceeds the analyze-data cap (%d). "+
			"Narrow the filter further before running analyze-data on %q.",
			meta.RowCount, analysis.MaxAnalyzeRows, meta.Name)
	}
	return out, nil
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
	if f == nil {
		// dedup hit — tell the LLM so it doesn't keep retrying
		// with cosmetically different wording. analyze-data's
		// auto-promote often races this when the user asks for
		// promotion right after a sliding-window run.
		return fmt.Sprintf("Finding already recorded (matches an existing observation): %q. No new entry created.", args.Content), nil
	}
	if err := a.findings.Save(); err != nil {
		return "", fmt.Errorf("save finding: %w", err)
	}
	a.notifyFindingsUpdated()

	return fmt.Sprintf("Finding promoted: %s (%s)", f.Content, f.CreatedLabel), nil
}

// toolRememberFact backs the remember-fact builtin (ADR-0019).
// LLM-driven memory: when the assistant judges a user fact worth
// persisting, it calls this tool. The fact is routed by category
// using the same allowlist as auto-extraction (preference/decision
// → GlobalMemory, fact/context → SessionMemory) and goes through
// the same IsSelfReferential filter that protects against THINK-
// leakage facts. Source = ToolCall so the audit trail distinguishes
// it from auto-extracted records.
//
// Returned strings are LLM-visible — they double as feedback that
// the call succeeded (LLM can stop trying to remember the same
// thing) or failed for a structural reason (LLM can rephrase).
func (a *Agent) toolRememberFact(argsJSON string) (string, error) {
	var args struct {
		Fact     string `json:"fact"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	fact := strings.TrimSpace(args.Fact)
	if fact == "" {
		return "", fmt.Errorf("fact is required and must be non-empty")
	}
	if !memory.ValidExtractionCategories[args.Category] {
		return "", fmt.Errorf("category must be one of preference, decision, fact, context (got %q)", args.Category)
	}
	if memory.IsSelfReferential(fact) {
		// Reject with feedback so the LLM can rephrase if the fact
		// was a legitimate user statement that happened to mention
		// the assistant. Same defence as extractMemories.
		return "", fmt.Errorf("rejected: fact appears to describe the assistant or its internals, not the user")
	}
	if a.session == nil {
		return "", fmt.Errorf("no session loaded")
	}

	isGlobal := args.Category == "preference" || args.Category == "decision"
	if isGlobal && a.session.Private {
		// Mirrors extractMemories' privacy gate: private sessions
		// do not promote facts to the cross-session store.
		return "", fmt.Errorf("rejected: this session is marked private; preference/decision facts cannot be saved cross-session")
	}
	if isGlobal {
		added := a.globalMemory.Add(memory.GlobalMemoryEntry{
			Fact:        fact,
			Category:    args.Category,
			Source:      memory.GlobalSourceToolCall,
			CreatedTurn: userTurnCount(a.session.Records),
		})
		if !added {
			return fmt.Sprintf("Fact already recorded (deduplicated): %q. No new entry created.", fact), nil
		}
		_ = a.globalMemory.Save()
		a.mu.Lock()
		h := a.handlers.GlobalMemory
		a.mu.Unlock()
		if h != nil {
			h()
		}
		return fmt.Sprintf("Saved to global memory (%s): %q", args.Category, fact), nil
	}
	if a.sessionMemory == nil {
		return "", fmt.Errorf("session memory store unavailable")
	}
	added := a.sessionMemory.Add(memory.SessionMemoryEntry{
		Fact:        fact,
		Category:    args.Category,
		Source:      memory.SessionSourceToolCall,
		CreatedTurn: userTurnCount(a.session.Records),
	})
	if !added {
		return fmt.Sprintf("Fact already recorded (deduplicated): %q. No new entry created.", fact), nil
	}
	_ = a.sessionMemory.Save()
	a.notifySessionMemoryUpdated()
	return fmt.Sprintf("Saved to session memory (%s): %q", args.Category, fact), nil
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
// Design: docs/en/history/agent-data-flow.md Section 6
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

	// Buffer the report so the agent loop can append it to
	// session.Records AFTER AddToolResult — this guarantees the
	// persisted order is "assistant tool_call → tool result →
	// report", which is also the order the chat pane should
	// render. Writing the report to records here would put it
	// BEFORE the tool result (which the loop appends moments
	// later), and a subsequent LoadSession would replay them in
	// the reversed order.
	if a.session != nil {
		a.mu.Lock()
		a.pendingReport = &pendingReport{
			title:    args.Title,
			content:  reportContent,
			objectID: reportObjectID,
		}
		a.mu.Unlock()
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
		sb.WriteString(fmt.Sprintf("- ID: %s | Type: %s | Name: %s | Size: %d bytes",
			o.ID, o.Type, name, o.Size))
		// v0.5: surface Lines/Tokens for text-bearing types so the
		// LLM can plan get-text ranges and analyze-text invocations
		// without copying the file into the sandbox just to wc -l.
		// We gate on Lines > 0 (rather than Type) so legacy reports
		// from pre-v0.5 (Lines unset) simply omit the columns; the
		// Load() backfill fills them on first launch with v0.5.
		if o.Lines > 0 {
			sb.WriteString(fmt.Sprintf(" | Lines: %d | Tokens: %d", o.Lines, o.Tokens))
		}
		sb.WriteString(fmt.Sprintf(" | Created: %s\n", o.CreatedAt.Format("2006-01-02 15:04:05")))
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
// docs/en/history/work-dir-shell-bridge.md.
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

	// Fetch all rows via the analyze-specific path. The interactive
	// QuerySQL cap (MaxQueryRows=10k) is correct for chat-output
	// tools but defeats the sliding window's whole purpose here —
	// the rows never enter the chat directly, they get chunked
	// into per-window LLM calls. analyze-data uses the much higher
	// MaxAnalyzeRows backstop instead. See docs/en/adr/0005-analyze-data-row-cap.md.
	query := fmt.Sprintf("SELECT * FROM \"%s\"", tableName)
	results, err := a.analysis.QuerySQLForAnalyze(query)
	if err != nil {
		return "", fmt.Errorf("query table: %w", err)
	}
	rows := analysis.RowsToJSON(results)

	// Build LLM adapter
	adapter := &backendLLMAdapter{backend: a.backend}

	// Run analysis. Inject a language hint derived from the user's
	// recent conversation so the summarizer doesn't silently emit
	// English findings when the assistant LLM translated the
	// perspective string to English upstream — see fix(findings)
	// commit a6a8e55 for the symptom.
	cfg := analysis.DefaultSummarizerConfig()
	summarizer := analysis.NewSummarizer(adapter, a.analysis.Schema(), cfg)
	if a.session != nil {
		summarizer.LanguageHint = detectUserLanguageHint(a.session.Records)
	}
	// Per-window progress is published as tool_progress events
	// targeting the parent "analyze-data" bubble so the chat pane
	// shows a single bubble whose text updates in place rather
	// than a fresh "running" bubble per window (which previously
	// stayed stuck because no matching tool_end ever fired). See
	// docs/en/adr/0002-tool-progress-events.md and issue #5.
	a.mu.Lock()
	parentToolCallID := a.activeToolCallID
	a.mu.Unlock()
	result, err := summarizer.Analyze(ctx, args.Prompt, rows, func(idx, total int) {
		a.emitActivity(ActivityEvent{
			Type:       "tool_progress",
			Detail:     fmt.Sprintf("analyze-data — window %d/%d", idx+1, total),
			ToolCallID: parentToolCallID,
		})
	})
	// Revert the bubble text to the parent tool name so the
	// completed bubble reads "analyze-data" (matching the visual
	// convention of every other tool) rather than freezing on the
	// last window's progress text. Emitted regardless of err so
	// the bubble that's about to be marked error/success looks
	// the same as a single-window run. The frontend's tool_end
	// matches by tool_call_id (App.tsx) so this revert is
	// purely cosmetic.
	if parentToolCallID != "" {
		a.emitActivity(ActivityEvent{
			Type:       "tool_progress",
			Detail:     "analyze-data",
			ToolCallID: parentToolCallID,
		})
	}
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
