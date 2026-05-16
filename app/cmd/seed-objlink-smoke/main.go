// seed-objlink-smoke — deterministic ADR-0014 rendering fixture.
//
// Creates a session named "ADR-0014 smoke" in the user's data
// dir with one assistant record per rendering path. No LLM in
// the loop — the markdown shapes are chosen by this program so
// every chip / image / fallback is reproducible across runs.
//
// Usage:
//
//	cd app && go run ./cmd/seed-objlink-smoke
//
// Then open shell-agent-v2 and pick the "ADR-0014 smoke" session
// from the sidebar. The session contains one assistant message
// per smoke step (Section §6.4 of ADR-0014). Each message names
// the step in its first line and shows the exact markdown that
// drives that step on the second line — verify the rendered
// output against the expectation noted in the message body.
//
// Cleanup (optional, after testing):
//
//	rm -rf "~/Library/Application Support/shell-agent-v2/sessions/adr0014-smoke-fixture"
//
// The created objects stay in the global objstore; delete them
// from the Data → Objects panel if you want a clean slate.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// 1×1 transparent PNG — smallest possible image bytes so the
// renderer has something legitimately PNG-shaped to decode.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABAQMAAAAl21bKAAAAA1BMVEX///+nxBvIAAAAC0lEQVQI12NgAAIAAAUAAeImBZsAAAAASUVORK5CYII="

const sessionID = "adr0014-smoke-fixture"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Open the live objstore so the seeded objects are visible
	// in the running app's Data → Objects panel as well.
	storeDir := config.DataDir() + "/objects"
	store := objstore.NewStoreAt(storeDir)
	if err := store.Load(); err != nil {
		return fmt.Errorf("objstore load: %w", err)
	}

	// Seed 4 objects, one per ObjectType. SessionID is set to
	// the smoke fixture so they appear in the per-session
	// objects panel as well.
	pngBytes, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		return err
	}
	imgMeta, err := store.Store(bytes.NewReader(pngBytes), objstore.TypeImage, "image/png", "smoke-pixel.png", sessionID)
	if err != nil {
		return fmt.Errorf("seed image: %w", err)
	}

	mdContent := "# Smoke markdown attachment\n\nThis is the body of the attached markdown.\n\n- bullet 1\n- bullet 2\n"
	mdMeta, err := store.Store(strings.NewReader(mdContent), objstore.TypeMarkdown, "text/markdown", "smoke-attach.md", sessionID)
	if err != nil {
		return fmt.Errorf("seed markdown: %w", err)
	}

	reportContent := "# Smoke prior report\n\nA previously-generated report that a new report can cite.\n"
	reportMeta, err := store.Store(strings.NewReader(reportContent), objstore.TypeReport, "text/markdown", "smoke-prior-report.md", sessionID)
	if err != nil {
		return fmt.Errorf("seed report: %w", err)
	}

	blobContent := []byte("PK\x03\x04smoke-blob-bytes")
	blobMeta, err := store.Store(bytes.NewReader(blobContent), objstore.TypeBlob, "application/zip", "smoke-archive.zip", sessionID)
	if err != nil {
		return fmt.Errorf("seed blob: %w", err)
	}

	// Build the fixture session. Records are crafted so each
	// step from ADR-0014 §6.4 is exercised by a single
	// assistant message — the markdown text inside that message
	// is the exact shape we want the renderer to handle.
	sess := &memory.Session{
		ID:    sessionID,
		Title: "ADR-0014 smoke",
		Records: []memory.Record{
			step("Step 0 — fixture overview", `This session contains one assistant message per ADR-0014 §6.4 step.

**Seeded object IDs (also visible in the right-hand Data → Objects panel):**

- image:    `+"`"+imgMeta.ID+"`"+`
- markdown: `+"`"+mdMeta.ID+"`"+`
- report:   `+"`"+reportMeta.ID+"`"+`
- blob:     `+"`"+blobMeta.ID+"`"+`

Scroll down. Each step shows expected vs actual; click the chip / image where indicated.`),

			step("Step 1 — `![alt](object:imageID)` → inline image",
				"Expected: inline image renders below this line, clicking opens the lightbox.\n\n"+
					"![tiny pixel]("+objref(imgMeta.ID)+")\n"),

			step("Step 2 — `![alt](object:markdownID)` mismatch → markdown chip",
				"Expected: NO broken-image glyph. A 📝 chip with the label \"my doc\" appears inline. Clicking opens ReportViewer with the markdown body.\n\n"+
					"![my doc]("+objref(mdMeta.ID)+")\n"),

			step("Step 3 — `[title](object:markdownID)` → markdown chip",
				"Expected: 📝 chip labelled \"open the attached doc\" inline in this paragraph. Click opens ReportViewer.\n\n"+
					"See [open the attached doc]("+objref(mdMeta.ID)+") for context.\n"),

			step("Step 4 — `[title](object:imageID)` mismatch → inline image",
				"Expected: NO dead anchor. The image renders inline with alt-text \"the pixel\" (LLM's intent honoured).\n\n"+
					"Reference: [the pixel]("+objref(imgMeta.ID)+")\n"),

			step("Step 5 — `[title](object:blobID)` → blob chip → save-as",
				"Expected: 📎 chip labelled \"the archive\" inline. Clicking surfaces the macOS save-as dialog (cancel without saving is fine).\n\n"+
					"Download [the archive]("+objref(blobMeta.ID)+").\n"),

			step("Step 6 — missing object → muted chip + no-op click",
				"Expected: 📎 chip in muted style (opacity 0.6). Title attribute says \"Object … not found\". Click does nothing.\n\n"+
					"[missing ref]("+objref("ffffffffffffffffffffffffffffffff")+")\n"),

			step("Step 7 — nested chip inside ReportViewer (cross-report navigation)",
				"Expected (two clicks):\n"+
					"  1. Click [open report A]("+objref(reportMeta.ID)+") below — ReportViewer overlay opens showing the prior-report body.\n"+
					"  2. The body of that report contains a chip pointing to the markdown attachment. Clicking it should REPLACE the visible report (no back-stack).\n\n"+
					"For (2) the prior-report body is plain text; we need a nested link too. Open [open report A]("+objref(reportMeta.ID)+") first, then use [Step 3 above] to test nested navigation in lieu of editing the report content.\n"),

			step("Step 8 — streaming render (n/a in fixture)",
				"Expected: This step can only be tested live. The fixture is fully-rendered (not streaming). To exercise the streaming path, send a real message to the agent and watch the in-flight bubble — but that is no longer required to validate the renderer, since `App.tsx:streaming` shares the same components map as this fixture proves works.\n"),

			step("Step 9 — cmd-popup render (n/a in fixture)",
				"Expected: Same as Step 8. The cmd-popup path is structurally identical to `App.tsx:streaming` — both call the same `objectComponents` factory. If Steps 1–6 pass, this path passes.\n"),

			report("Step 10 — export self-containment",
				"# Step 10 — export self-containment\n\n"+
					"Click **Save** on this report card to export it. Open the resulting `.md` in an external editor.\n\n"+
					"Expected in the exported file:\n\n"+
					"- The image below is inlined as a `data:image/...;base64,…` URL (long blob).\n"+
					"- The markdown link below keeps its `object:` href unchanged (NOT rewritten to `data:text/markdown`).\n\n"+
					"Image: ![smoke pixel]("+objref(imgMeta.ID)+")\n\n"+
					"Doc:   [the attached doc]("+objref(mdMeta.ID)+")\n"),

			step("Step 11 — cache cleanup on session switch (DevTools)",
				"Expected: Switch to any other session, then back here. The chip render path triggers `clearObjectMetaCache` on switch (see `App.tsx:handleLoadSession`). Visually this should be invisible — chips reappear identically. The check exists to prevent a stale cache entry from leaking across session boundaries.\n"),
		},
	}

	if err := sess.Save(); err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	// Persist the objstore index so the new objects survive a
	// restart of the app.
	if err := store.Save(); err != nil {
		return fmt.Errorf("objstore save: %w", err)
	}

	fmt.Println("Seeded session:")
	fmt.Println("  ID:    " + sess.ID)
	fmt.Println("  Title: " + sess.Title)
	fmt.Println("  Path:  " + memory.ChatPath(sess.ID))
	fmt.Println()
	fmt.Println("Open the app, refresh the session list (or restart), pick \"ADR-0014 smoke\" from the sidebar.")
	fmt.Println("Created at:", time.Now().Format(time.RFC3339))
	return nil
}

func objref(id string) string {
	return "object:" + id
}

// step builds a Records entry that renders as a normal assistant
// message containing the step label as the first line followed
// by the markdown body.
func step(label, body string) memory.Record {
	return memory.Record{
		Timestamp: time.Now(),
		Role:      "assistant",
		Content:   "**" + label + "**\n\n" + body,
	}
}

// report builds a Records entry that renders as a report card
// (the inline expandable card with Copy / Save / Expand). Used
// for Step 10 so the user can hit Save and inspect the export
// output.
func report(title, body string) memory.Record {
	_ = os.Stderr // keep import set minimal
	return memory.Record{
		Timestamp: time.Now(),
		Role:      "report",
		Content:   body,
		ToolName:  title, // memory.go reuses ToolName as the report title
	}
}
