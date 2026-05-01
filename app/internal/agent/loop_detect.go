// loop_detect.go — detect "model stuck retrying the same tool with
// errors" stretches and produce a corrective hint to inject into
// the next LLM message list.
//
// Design: docs/{en,ja}/agent-loop-resilience{,.ja}.md §3. The hint
// is one-shot per stretch, advisory only, and is NOT persisted to
// session.Records — it's a transient nudge, not memory.

package agent

import "fmt"

// recentToolWindow is the size of the ring buffer used to detect
// "same tool, all error" stretches. 3 is the smallest value that
// gives the model two trivially-varied retries to recover on its
// own before we step in.
const recentToolWindow = 3

// toolCallTrace records what we need to detect a stuck loop:
// which tool was called, and whether it returned error.
type toolCallTrace struct {
	Name   string
	Status ActivityEventStatus
}

// pushToolCallTrace appends t to buf and trims to recentToolWindow.
// Returns the (possibly trimmed) slice.
func pushToolCallTrace(buf []toolCallTrace, t toolCallTrace) []toolCallTrace {
	buf = append(buf, t)
	if len(buf) > recentToolWindow {
		buf = buf[len(buf)-recentToolWindow:]
	}
	return buf
}

// detectStuckLoop reports whether buf represents a fired-loop
// condition: full window, all same Name, all error. Returns the
// repeated tool name when ok=true.
func detectStuckLoop(buf []toolCallTrace) (toolName string, ok bool) {
	if len(buf) != recentToolWindow {
		return "", false
	}
	name := buf[0].Name
	for _, t := range buf {
		if t.Name != name || t.Status != ActivityStatusError {
			return "", false
		}
	}
	return name, true
}

// loopHintFor formats the corrective system-note hint for the
// repeated tool. English-only — the LLM is multilingual enough
// that this works regardless of the user's UI locale.
func loopHintFor(toolName string) string {
	return fmt.Sprintf(
		"System note: you have called `%s` three times in a row "+
			"and each call returned an error. Stop retrying with minor "+
			"variations. Try a substantively different approach — for "+
			"example, write the input to a file first and inspect it "+
			"before re-running, or abandon this branch and ask the user "+
			"for clarification.",
		toolName,
	)
}

// emptyResponseNudge is prepended as a one-shot system message
// when the LLM returns content="" with no tool calls right after
// successful tool execution. Vertex (gemini-2.5-flash) sometimes
// returns 0 output tokens after a tool result, leaving the user
// staring at tool activity without a wrap-up. The nudge asks for
// a one-sentence summary; if the retry also returns empty, the
// loop exits silently as before.
const emptyResponseNudge = "System note: your previous response was empty. " +
	"Please briefly summarize the result of the tool calls for the user " +
	"in one or two sentences."
