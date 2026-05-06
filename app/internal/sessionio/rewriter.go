// rewriter.go: object-ID reference rewriter.
//
// Imports always regenerate object IDs (design §5.3). The references
// that need rewriting live in two files and three loci:
//
//   chat.json:      Record.ObjectIDs[]  (structured)
//   chat.json:      Record.Content      (markdown `![](object:ID)` etc.)
//   summaries.json: SummaryEntry.Summary (paraphrased refs from above)
//
// session_memory.json, findings.json, and global_memory.json are
// intentionally NOT swept — see design §5.3 for the audit that
// justifies this. If a future change introduces ref embedding into
// any of those stores, this file and the design doc must be
// updated together.

package sessionio

import (
	"regexp"

	"github.com/nlink-jp/shell-agent-v2/internal/contextbuild"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

// objectIDPattern matches object IDs in free text. Two ID widths
// are accepted: 32 hex chars (current, IDByteLen=16) and 12 hex
// chars (legacy bundles produced before the width change). The
// optional `object:` prefix matches the markdown image-link form
// `![alt](object:ID)`. Word boundaries ensure we don't munge IDs
// that happen to be a prefix of a longer hex string.
var objectIDPattern = regexp.MustCompile(`\b(object:)?([a-f0-9]{32}|[a-f0-9]{12})\b`)

// RewriteText replaces every old object ID found in s with its
// mapped new ID. IDs not present in the map are left alone (this is
// safe: the user may have hex-looking text that isn't actually an
// object ref). The optional `object:` prefix is preserved in the
// output.
func RewriteText(s string, idMap map[string]string) string {
	if len(idMap) == 0 || s == "" {
		return s
	}
	return objectIDPattern.ReplaceAllStringFunc(s, func(match string) string {
		// ReplaceAllStringFunc gives us the whole match; re-run the
		// pattern to pull out the prefix and ID groups.
		sub := objectIDPattern.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		prefix, oldID := sub[1], sub[2]
		newID, ok := idMap[oldID]
		if !ok {
			return match
		}
		return prefix + newID
	})
}

// RewriteRecords rewrites every object reference in a session's
// records — both the structured ObjectIDs slice and any matching
// hex IDs embedded in Content. Records are mutated in place.
func RewriteRecords(records []memory.Record, idMap map[string]string) {
	if len(idMap) == 0 {
		return
	}
	for i := range records {
		r := &records[i]
		for j, oldID := range r.ObjectIDs {
			if newID, ok := idMap[oldID]; ok {
				r.ObjectIDs[j] = newID
			}
		}
		r.Content = RewriteText(r.Content, idMap)
	}
}

// RewriteSummaries sweeps SummaryEntry.Summary text for matching
// refs. The summarizer LLM may have paraphrased markdown image
// refs from the source records into the summary text, so this
// pass is required to avoid dangling references on the imported
// side.
func RewriteSummaries(entries []contextbuild.SummaryEntry, idMap map[string]string) {
	if len(idMap) == 0 {
		return
	}
	for i := range entries {
		entries[i].Summary = RewriteText(entries[i].Summary, idMap)
	}
}
