package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ResolveDateArgs are the parameters for the resolve-date tool.
type ResolveDateArgs struct {
	Expression    string `json:"expression"`
	ReferenceDate string `json:"reference_date,omitempty"`
}

// ResolveDate resolves a relative date expression to an absolute date.
// Supports: "today", "yesterday", "tomorrow", "last/next <weekday>",
// "N days/weeks/months ago", "N days/weeks/months from now".
func ResolveDate(argsJSON string) (string, error) {
	var args ResolveDateArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	ref := time.Now()
	if args.ReferenceDate != "" {
		parsed, err := time.Parse("2006-01-02", args.ReferenceDate)
		if err != nil {
			return "", fmt.Errorf("invalid reference_date %q: use YYYY-MM-DD format", args.ReferenceDate)
		}
		ref = parsed
	}

	expr := strings.TrimSpace(strings.ToLower(args.Expression))

	result, err := resolveExpression(expr, ref)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s (%s)", result.Format("2006-01-02"), result.Format("Monday")), nil
}

func resolveExpression(expr string, ref time.Time) (time.Time, error) {
	switch expr {
	case "today":
		return ref, nil
	case "yesterday":
		return ref.AddDate(0, 0, -1), nil
	case "tomorrow":
		return ref.AddDate(0, 0, 1), nil
	}

	// "last <weekday>" / "next <weekday>"
	if strings.HasPrefix(expr, "last ") {
		dayName := strings.TrimPrefix(expr, "last ")
		if wd, ok := parseWeekday(dayName); ok {
			return lastWeekday(ref, wd), nil
		}
	}
	if strings.HasPrefix(expr, "next ") {
		dayName := strings.TrimPrefix(expr, "next ")
		if wd, ok := parseWeekday(dayName); ok {
			return nextWeekday(ref, wd), nil
		}
	}

	// "N days/weeks/months ago"
	if strings.HasSuffix(expr, " ago") {
		trimmed := strings.TrimSuffix(expr, " ago")
		return parseRelative(trimmed, ref, -1)
	}

	// "N days/weeks/months from now"
	if strings.HasSuffix(expr, " from now") {
		trimmed := strings.TrimSuffix(expr, " from now")
		return parseRelative(trimmed, ref, 1)
	}

	return time.Time{}, fmt.Errorf("unrecognized expression: %q", expr)
}

func parseRelative(s string, ref time.Time, direction int) (time.Time, error) {
	var n int
	var unit string
	if _, err := fmt.Sscanf(s, "%d %s", &n, &unit); err != nil {
		return time.Time{}, fmt.Errorf("unrecognized expression: %q", s)
	}

	unit = strings.TrimSuffix(unit, "s") // normalize plural
	n *= direction

	switch unit {
	case "day":
		return ref.AddDate(0, 0, n), nil
	case "week":
		return ref.AddDate(0, 0, n*7), nil
	case "month":
		return ref.AddDate(0, n, 0), nil
	case "year":
		return ref.AddDate(n, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown unit: %q", unit)
	}
}

func parseWeekday(name string) (time.Weekday, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	days := map[string]time.Weekday{
		"sunday": time.Sunday, "sun": time.Sunday,
		"monday": time.Monday, "mon": time.Monday,
		"tuesday": time.Tuesday, "tue": time.Tuesday,
		"wednesday": time.Wednesday, "wed": time.Wednesday,
		"thursday": time.Thursday, "thu": time.Thursday,
		"friday": time.Friday, "fri": time.Friday,
		"saturday": time.Saturday, "sat": time.Saturday,
	}
	wd, ok := days[name]
	return wd, ok
}

func lastWeekday(ref time.Time, target time.Weekday) time.Time {
	diff := int(ref.Weekday()) - int(target)
	if diff <= 0 {
		diff += 7
	}
	return ref.AddDate(0, 0, -diff)
}

func nextWeekday(ref time.Time, target time.Weekday) time.Time {
	diff := int(target) - int(ref.Weekday())
	if diff <= 0 {
		diff += 7
	}
	return ref.AddDate(0, 0, diff)
}

// ResolveDateToolDef returns the tool definition for resolve-date.
func ResolveDateToolDef() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expression": map[string]any{
				"type": "string",
				// Language constraint is critical: the resolver
				// does pure-English keyword matching ("last X" /
				// "next X" / "N days ago" / "N weeks from now" /
				// today / yesterday / tomorrow). Passing the
				// expression in the conversation language
				// (e.g. "先週の木曜日", "3 semaines plus tard")
				// returns "unrecognized expression". State the
				// constraint here so the LLM doesn't mirror the
				// user's language into the parameter.
				"description": "Natural language date expression in ENGLISH ONLY. Supported forms: 'today', 'yesterday', 'tomorrow', 'last|next <weekday>' (e.g. 'last Thursday'), 'N days|weeks|months|years ago', 'N days|weeks|months|years from now'. Do NOT pass localized expressions like '先週の木曜日' even when the user is speaking another language — translate the date concept to English first.",
			},
			"reference_date": map[string]any{
				"type":        "string",
				"description": "ISO date to calculate from (default: today). Format: YYYY-MM-DD",
			},
		},
		"required": []string{"expression"},
	}
}
