package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nlink-jp/nlk/guard"
	"github.com/nlink-jp/nlk/jsonfix"
)

// LLMClient is the interface for LLM calls used by the summarizer.
type LLMClient interface {
	Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// SummarizerConfig controls the sliding window analysis behavior.
type SummarizerConfig struct {
	MaxRecordsPerWindow int     // rows per window (default 100)
	OverlapRatio        float64 // overlap between windows (default 0.1)
	MaxFindings         int     // max accumulated findings (default 50)
}

// DefaultSummarizerConfig returns the default configuration.
func DefaultSummarizerConfig() SummarizerConfig {
	return SummarizerConfig{
		MaxRecordsPerWindow: 100,
		OverlapRatio:        0.1,
		MaxFindings:         50,
	}
}

// Finding represents a single analysis finding.
type Finding struct {
	Description string `json:"description"`
	Severity    string `json:"severity"` // info, low, medium, high, critical
	Evidence    string `json:"evidence"`
}

// AnalyzeResult holds the output of a sliding window analysis.
type AnalyzeResult struct {
	Summary  string        `json:"summary"`
	Findings []Finding     `json:"findings"`
	Windows  int           `json:"windows"`
	Duration time.Duration `json:"duration"`
}

// ProgressCallback is called for each window during analysis.
type ProgressCallback func(windowIndex, totalWindows int)

// windowResponse is the expected JSON structure from the LLM per window.
type windowResponse struct {
	Summary     string    `json:"summary"`
	NewFindings []Finding `json:"new_findings"`
}

// Summarizer performs sliding window analysis on data rows.
type Summarizer struct {
	llm    LLMClient
	schema string
	cfg    SummarizerConfig
}

// NewSummarizer creates a new summarizer.
func NewSummarizer(llm LLMClient, schema string, cfg SummarizerConfig) *Summarizer {
	return &Summarizer{llm: llm, schema: schema, cfg: cfg}
}

// Analyze runs sliding window analysis on data rows with the given perspective.
func (s *Summarizer) Analyze(ctx context.Context, perspective string, rows []string, progress ProgressCallback) (*AnalyzeResult, error) {
	start := time.Now()

	if len(rows) == 0 {
		return &AnalyzeResult{Summary: "No data to analyze."}, nil
	}

	step := s.cfg.MaxRecordsPerWindow - int(float64(s.cfg.MaxRecordsPerWindow)*s.cfg.OverlapRatio)
	if step < 1 {
		step = 1
	}
	totalWindows := (len(rows) + step - 1) / step

	tag := guard.NewTag()
	var summary string
	var findings []Finding
	windowIndex := 0

	for offset := 0; offset < len(rows); offset += step {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		end := offset + s.cfg.MaxRecordsPerWindow
		if end > len(rows) {
			end = len(rows)
		}
		windowRows := rows[offset:end]

		if progress != nil {
			progress(windowIndex, totalWindows)
		}

		sysPrompt := s.buildSystemPrompt(perspective)
		userPrompt := s.buildUserPrompt(tag, summary, findings, windowRows, windowIndex)

		resp, err := s.llm.Chat(ctx, sysPrompt, userPrompt)
		if err != nil {
			return nil, fmt.Errorf("window %d: %w", windowIndex, err)
		}

		wr := s.parseWindowResponse(resp)
		summary = wr.Summary
		findings = append(findings, wr.NewFindings...)
		findings = evictFindings(findings, s.cfg.MaxFindings)

		windowIndex++
	}

	return &AnalyzeResult{
		Summary:  summary,
		Findings: findings,
		Windows:  windowIndex,
		Duration: time.Since(start),
	}, nil
}

// GenerateReport creates a markdown report from analysis results.
func GenerateReport(perspective string, result *AnalyzeResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Analysis Report\n\n"))
	sb.WriteString(fmt.Sprintf("> Perspective: %s\n", perspective))
	sb.WriteString(fmt.Sprintf("> Windows: %d | Duration: %s\n\n", result.Windows, result.Duration.Round(time.Second)))

	sb.WriteString("## Summary\n\n")
	sb.WriteString(result.Summary)
	sb.WriteString("\n\n")

	if len(result.Findings) == 0 {
		sb.WriteString("## Findings\n\nNo significant findings.\n")
		return sb.String()
	}

	sb.WriteString("## Findings\n\n")

	// Group by severity
	order := []string{"critical", "high", "medium", "low", "info"}
	grouped := map[string][]Finding{}
	for _, f := range result.Findings {
		sev := normalizeSeverity(f.Severity)
		grouped[sev] = append(grouped[sev], f)
	}

	for _, sev := range order {
		fs := grouped[sev]
		if len(fs) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("### %s (%d)\n\n", strings.Title(sev), len(fs)))
		for _, f := range fs {
			sb.WriteString(fmt.Sprintf("- **%s**\n", f.Description))
			if f.Evidence != "" {
				sb.WriteString(fmt.Sprintf("  - Evidence: %s\n", f.Evidence))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// --- prompts ---

func (s *Summarizer) buildSystemPrompt(perspective string) string {
	return fmt.Sprintf(`You are a data analyst. Analyze data records from a specific perspective.

## Analysis Perspective
%s

## Data Schema
%s

## Output Format
Respond with ONLY valid JSON:
{
  "summary": "Updated running summary incorporating new observations from this window",
  "new_findings": [
    {
      "description": "What was found",
      "severity": "info|low|medium|high|critical",
      "evidence": "Specific data that supports this finding"
    }
  ]
}

Rules:
- Update the summary to incorporate observations from the new data window
- Only report NEW findings not already covered in previous findings
- Use severity levels appropriately: critical for urgent issues, info for general observations
- Include specific evidence from the data
- Write the summary and finding descriptions in the same language as the analysis perspective`, perspective, s.schema)
}

func (s *Summarizer) buildUserPrompt(tag guard.Tag, summary string, findings []Finding, rows []string, windowIndex int) string {
	var sb strings.Builder

	if summary != "" {
		sb.WriteString("### Previous Summary\n")
		sb.WriteString(summary)
		sb.WriteString("\n\n")
	}

	if len(findings) > 0 {
		sb.WriteString("### Current Findings\n")
		for _, f := range findings {
			sb.WriteString(fmt.Sprintf("- [%s] %s\n", f.Severity, f.Description))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("### New Data (Window %d)\n", windowIndex+1))

	dataChunk := strings.Join(rows, "\n")
	if wrapped, err := tag.Wrap(dataChunk); err == nil {
		sb.WriteString(wrapped)
	} else {
		sb.WriteString(dataChunk)
	}

	return sb.String()
}

// --- parsing ---

func (s *Summarizer) parseWindowResponse(resp string) windowResponse {
	var wr windowResponse
	resp = strings.TrimSpace(resp)

	// jsonfix handles markdown fences, surrounding prose, single
	// quotes, trailing commas, and unbalanced braces — all the
	// things a hand-rolled "try direct parse → strip ```json
	// fence → walk to first balanced }" cascade tries to deal
	// with, but with more cases covered. RFP §3 explicitly lists
	// nlk/jsonfix as a reuse target; until v0.1.11 we'd shipped
	// a degraded copy.
	if fixed, err := jsonfix.Extract(resp); err == nil {
		if jerr := json.Unmarshal([]byte(fixed), &wr); jerr == nil {
			return wr
		}
	}
	// Fallback: use the raw text as summary so a malformed
	// response still produces something usable downstream.
	return windowResponse{Summary: resp}
}

// --- helpers ---

func evictFindings(findings []Finding, max int) []Finding {
	if len(findings) <= max {
		return findings
	}

	// Keep high-priority findings, evict low-priority ones (FIFO)
	var high, low []Finding
	for _, f := range findings {
		sev := normalizeSeverity(f.Severity)
		switch sev {
		case "critical", "high", "medium":
			high = append(high, f)
		default:
			low = append(low, f)
		}
	}

	remaining := max - len(high)
	if remaining < 0 {
		// Even high-priority exceeds limit, keep newest
		return high[len(high)-max:]
	}
	if remaining < len(low) {
		low = low[len(low)-remaining:]
	}
	return append(high, low...)
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return "critical"
	case "high":
		return "high"
	case "medium":
		return "medium"
	case "low":
		return "low"
	default:
		return "info"
	}
}

// RowsToJSON converts query results to JSON row strings for the summarizer.
func RowsToJSON(results []map[string]any) []string {
	rows := make([]string, len(results))
	for i, row := range results {
		data, _ := json.Marshal(row)
		rows[i] = string(data)
	}
	return rows
}
