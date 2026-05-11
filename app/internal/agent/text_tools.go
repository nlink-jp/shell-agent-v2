// text_tools.go — v0.5 markdown / text attachment tools:
// analyze-text, grep-text, get-text. Operate on objstore objects
// of type TypeMarkdown (user-attached) or TypeReport (agent-
// generated via create-report). See docs/en/markdown-attachments.md.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/nlink-jp/shell-agent-v2/internal/analysis"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// resolveTextObject fetches and validates a text-bearing object
// for the analyze-text / grep-text / get-text tools.
//
// idArg may be either the bare hex ID or the "object:<id>" form
// the LLM sees in chat-message anchors. Returns an explicit error
// if the object doesn't exist or isn't markdown/report; the
// non-text-type message is shaped so the LLM can pivot (e.g.
// "this is an image; use vision instead").
func (a *Agent) resolveTextObject(idArg string) (*objstore.ObjectMeta, []byte, error) {
	if a.objects == nil {
		return nil, nil, fmt.Errorf("object store not initialised")
	}
	id := strings.TrimPrefix(idArg, "object:")
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil, fmt.Errorf("object id is required")
	}
	meta, ok := a.objects.Get(id)
	if !ok {
		return nil, nil, fmt.Errorf("object %s not found", id)
	}
	if meta.Type != objstore.TypeMarkdown && meta.Type != objstore.TypeReport {
		return nil, nil, fmt.Errorf("text tools require a markdown or report object; %s is type %q (use list-objects to see what's available)", id, meta.Type)
	}
	rdr, err := a.objects.ReadData(id)
	if err != nil {
		return nil, nil, fmt.Errorf("read object %s: %w", id, err)
	}
	defer rdr.Close()
	content, err := io.ReadAll(rdr)
	if err != nil {
		return nil, nil, fmt.Errorf("read object %s: %w", id, err)
	}
	return meta, content, nil
}

// parseLineRange parses an optional "M-N" or "M" range argument.
// Returns 1-based inclusive [start, end] clamped to [1,
// totalLines]. Empty input means "whole document" → returns
// (1, totalLines).
//
// Accepted forms:
//   - ""        → entire document
//   - "M"       → just line M
//   - "M-N"     → lines M through N inclusive
//   - "M-"      → lines M through end
//   - "-N"      → lines 1 through N
//
// Out-of-range bounds are clamped; an explicitly inverted range
// (start > end after clamping) returns an error.
func parseLineRange(s string, totalLines int) (start, end int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1, totalLines, nil
	}
	if totalLines == 0 {
		return 0, 0, fmt.Errorf("document is empty")
	}
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		left := strings.TrimSpace(s[:idx])
		right := strings.TrimSpace(s[idx+1:])
		switch {
		case left == "" && right == "":
			return 1, totalLines, nil
		case left == "":
			n, perr := strconv.Atoi(right)
			if perr != nil {
				return 0, 0, fmt.Errorf("invalid line range %q: %w", s, perr)
			}
			start, end = 1, n
		case right == "":
			n, perr := strconv.Atoi(left)
			if perr != nil {
				return 0, 0, fmt.Errorf("invalid line range %q: %w", s, perr)
			}
			start, end = n, totalLines
		default:
			l, perr := strconv.Atoi(left)
			if perr != nil {
				return 0, 0, fmt.Errorf("invalid line range %q: %w", s, perr)
			}
			r, perr := strconv.Atoi(right)
			if perr != nil {
				return 0, 0, fmt.Errorf("invalid line range %q: %w", s, perr)
			}
			start, end = l, r
		}
	} else {
		n, perr := strconv.Atoi(s)
		if perr != nil {
			return 0, 0, fmt.Errorf("invalid line range %q: %w", s, perr)
		}
		start, end = n, n
	}
	if start < 1 {
		start = 1
	}
	if end > totalLines {
		end = totalLines
	}
	if start > end {
		return 0, 0, fmt.Errorf("line range %q is empty after clamp (start=%d, end=%d, total=%d)", s, start, end, totalLines)
	}
	return start, end, nil
}

// sliceByLines returns content[start..end] inclusive, where
// `start` / `end` are 1-based line numbers. Empty input or
// out-of-range bounds yield "".
func sliceByLines(content string, start, end int) string {
	lines := strings.Split(content, "\n")
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n")
}

// countLines returns the count of '\n'-separated lines in
// content (matching the semantics used by Store auto-fill:
// non-empty content has at least 1 line).
func countLines(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

// --- analyze-text ---------------------------------------------------

func (a *Agent) toolAnalyzeText(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Object      string `json:"object"`
		Perspective string `json:"perspective"`
		Lines       string `json:"lines"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Perspective == "" {
		return "", fmt.Errorf("perspective is required")
	}

	meta, content, err := a.resolveTextObject(args.Object)
	if err != nil {
		return "", err
	}
	totalLines := countLines(string(content))
	startLine, endLine, err := parseLineRange(args.Lines, totalLines)
	if err != nil {
		return "", err
	}
	body := sliceByLines(string(content), startLine, endLine)
	if body == "" {
		return "(empty range)", nil
	}

	chunks, err := analysis.ChunkText(body, analysis.DefaultChunkerConfig())
	if err != nil {
		return "", err
	}
	if len(chunks) == 0 {
		return "(no analysable content)", nil
	}

	if a.findings == nil {
		return "", fmt.Errorf("no session loaded")
	}

	adapter := &backendLLMAdapter{backend: a.backend}
	// MaxRecordsPerWindow=1 because each chunk is already ~target
	// token budget — we don't want to bundle multiple chunks into
	// one LLM call. OverlapRatio in the summarizer would create
	// double-overlap (the chunker already overlaps content), so
	// we drop it to zero here.
	cfg := analysis.SummarizerConfig{
		MaxRecordsPerWindow: 1,
		OverlapRatio:        0,
		MaxFindings:         analysis.DefaultSummarizerConfig().MaxFindings,
	}
	schema := fmt.Sprintf("Document content (sliding window of text chunks from a %s object named %q, %d total lines)", meta.Type, meta.OrigName, totalLines)
	summarizer := analysis.NewSummarizer(adapter, schema, cfg)
	if a.session != nil {
		summarizer.LanguageHint = detectUserLanguageHint(a.session.Records)
	}

	// Per-window progress mirrors analyze-data's v0.4.1 pattern
	// so the chat-pane bubble updates in place.
	a.mu.Lock()
	parentToolCallID := a.activeToolCallID
	a.mu.Unlock()
	result, err := summarizer.Analyze(ctx, args.Perspective, chunks, func(idx, total int) {
		a.emitActivity(ActivityEvent{
			Type:       "tool_progress",
			Detail:     fmt.Sprintf("analyze-text — window %d/%d", idx+1, total),
			ToolCallID: parentToolCallID,
		})
	})
	if parentToolCallID != "" {
		a.emitActivity(ActivityEvent{
			Type:       "tool_progress",
			Detail:     "analyze-text",
			ToolCallID: parentToolCallID,
		})
	}
	if err != nil {
		return "", fmt.Errorf("analyze-text: %w", err)
	}

	// Auto-promote findings tagged with the document's object ID
	// + name so Findings panel filters can scope to a document.
	for _, f := range result.Findings {
		sev := strings.ToLower(f.Severity)
		fcontent := f.Description
		if f.Evidence != "" {
			fcontent += "\nEvidence: " + f.Evidence
		}
		tags := []string{sev, "object:" + meta.ID}
		if meta.OrigName != "" {
			tags = append(tags, meta.OrigName)
		}
		a.findings.Add(fcontent, tags, findings.SourceAnalyzeText, true)
	}
	_ = a.findings.Save()
	if len(result.Findings) > 0 {
		a.notifyFindingsUpdated()
	}

	report := analysis.GenerateReport(args.Perspective, result)
	if len(report) > 10000 {
		report = report[:10000] + "\n\n... (truncated)"
	}
	return report, nil
}

// --- grep-text ------------------------------------------------------

func (a *Agent) toolGrepText(argsJSON string) (string, error) {
	var args struct {
		Object       string `json:"object"`
		Pattern      string `json:"pattern"`
		Lines        string `json:"lines"`
		MaxMatches   int    `json:"max_matches"`
		ContextLines int    `json:"context_lines"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if args.MaxMatches <= 0 {
		args.MaxMatches = 200
	}
	if args.ContextLines < 0 {
		args.ContextLines = 0
	}
	if args.ContextLines == 0 {
		args.ContextLines = 2
	}

	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", args.Pattern, err)
	}

	_, content, err := a.resolveTextObject(args.Object)
	if err != nil {
		return "", err
	}
	totalLines := countLines(string(content))
	startLine, endLine, err := parseLineRange(args.Lines, totalLines)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(content), "\n")
	if endLine > len(lines) {
		endLine = len(lines)
	}

	type hit struct {
		LineNum int
		Text    string
	}
	var matches []hit
	for i := startLine - 1; i < endLine; i++ {
		if re.MatchString(lines[i]) {
			matches = append(matches, hit{LineNum: i + 1, Text: lines[i]})
			if len(matches) > args.MaxMatches {
				return "", fmt.Errorf("too many matches (>%d). Narrow the pattern, or restrict via the lines argument", args.MaxMatches)
			}
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("(no matches for /%s/ in lines %d-%d)", args.Pattern, startLine, endLine), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d match(es) for /%s/:\n\n", len(matches), args.Pattern))
	for i, m := range matches {
		if i > 0 {
			sb.WriteString("--\n")
		}
		// Print context lines BEFORE.
		startCtx := max(1, m.LineNum-args.ContextLines)
		endCtx := min(len(lines), m.LineNum+args.ContextLines)
		for ln := startCtx; ln <= endCtx; ln++ {
			marker := ":"
			if ln == m.LineNum {
				marker = ">"
			}
			sb.WriteString(fmt.Sprintf("%d%s %s\n", ln, marker, lines[ln-1]))
		}
	}
	out := sb.String()
	if len(out) > 10000 {
		out = out[:10000] + "\n\n... (truncated; raise context_lines selectivity or narrow the pattern)"
	}
	return out, nil
}

// --- get-text -------------------------------------------------------

func (a *Agent) toolGetText(argsJSON string) (string, error) {
	var args struct {
		Object string `json:"object"`
		Lines  string `json:"lines"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Lines == "" {
		return "", fmt.Errorf("lines is required (e.g. \"100-200\")")
	}

	_, content, err := a.resolveTextObject(args.Object)
	if err != nil {
		return "", err
	}
	totalLines := countLines(string(content))
	startLine, endLine, err := parseLineRange(args.Lines, totalLines)
	if err != nil {
		return "", err
	}
	if endLine-startLine+1 > 1000 {
		return "", fmt.Errorf("line range too large: %d lines requested (max 1000 per call); use analyze-text for whole-document summarisation or call get-text in chunks", endLine-startLine+1)
	}

	lines := strings.Split(string(content), "\n")
	if endLine > len(lines) {
		endLine = len(lines)
	}
	var sb strings.Builder
	for ln := startLine; ln <= endLine; ln++ {
		sb.WriteString(fmt.Sprintf("%d: %s\n", ln, lines[ln-1]))
	}
	out := sb.String()
	if len(out) > 10000 {
		out = out[:10000] + "\n\n... (truncated; request a narrower range)"
	}
	return out, nil
}

// textToolDefs returns the three new text-attachment tools so
// they can be appended to the always-visible analysis tool list.
func textToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
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
		},
	}
}
