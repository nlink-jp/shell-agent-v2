package frontendlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frontendSrcRoot resolves the absolute path of frontend/src relative
// to this test file. The test file lives at
// app/internal/frontendlint/, so the frontend root is two `..` up.
func frontendSrcRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Join(wd, "..", "..", "frontend", "src")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("frontend/src not found at %s — skipping (running outside repo?)", root)
	}
	return root
}

// frontendPackageJSON returns the absolute path of frontend/package.json.
func frontendPackageJSON(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "frontend", "package.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("frontend/package.json not found at %s", path)
	}
	return path
}

// TestNoRehypeRaw enforces the security guard rail from
// security-hardening-2.md §8: the Markdown rendering pipeline must
// not enable `rehype-raw`.
//
// Why: rehype-raw passes raw HTML in markdown through to the React
// renderer, which in a Wails desktop app means an LLM-influenced
// `<script>` tag executes inside the same WebView2/WKWebView that has
// full filesystem reach. The current pipeline (remarkGfm + remarkBreaks
// + rehypeHighlight + rehypeKatex, no rehypeRaw) is XSS-safe by
// construction; adding rehype-raw later would silently undo that.
//
// This test catches three regression vectors:
//  1. Importing rehype-raw or rehype-sanitize in a .ts/.tsx file
//     (ReactMarkdown's `rehypePlugins` array taking the import).
//  2. Adding `rehype-raw` to package.json's dependencies / devDependencies.
//  3. A literal `dangerouslySetInnerHTML` introduced anywhere in the
//     React tree — the same XSS surface, different name.
//
// If you genuinely need richer-than-Markdown output, follow the
// "HTML output as a first-class object type" plan in TODO.md (separate
// object type rendered inside an iframe with strict CSP).
func TestNoRehypeRaw(t *testing.T) {
	srcRoot := frontendSrcRoot(t)

	forbidden := []string{
		"rehype-raw",
		"rehypeRaw",
		"dangerouslySetInnerHTML",
	}

	var hits []string
	err := filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".ts" && ext != ".tsx" && ext != ".js" && ext != ".jsx" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, needle := range forbidden {
			if strings.Contains(string(data), needle) {
				rel, _ := filepath.Rel(srcRoot, path)
				hits = append(hits, rel+": "+needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(hits) > 0 {
		t.Fatalf("forbidden patterns found in frontend/src (see security-hardening-2.md §8 / TODO.md HTML output entry):\n  - %s",
			strings.Join(hits, "\n  - "))
	}
}

// TestPackageJSONDoesNotDependOnRehypeRaw catches the case where
// rehype-raw is added to package.json before any source actually
// imports it — `npm install` would record it but the source-file
// scan above wouldn't fire yet. Belt-and-braces.
func TestPackageJSONDoesNotDependOnRehypeRaw(t *testing.T) {
	pkgPath := frontendPackageJSON(t)
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	for _, needle := range []string{"rehype-raw", "rehype-sanitize"} {
		if strings.Contains(string(data), needle) {
			t.Errorf("frontend/package.json must not depend on %q (security-hardening-2.md §8). "+
				"For richer-than-Markdown output, see TODO.md 'HTML output as a first-class object type'.", needle)
		}
	}
}

// TestDefaultUrlTransformIsCentralised guards ADR-0014 §2 / §3.1
// goal: only `markdown/objectMarkdown.tsx` should import
// `defaultUrlTransform` from react-markdown. The previous code
// duplicated the urlTransform wrapper across six ReactMarkdown
// sites, and adding the new a-component override on top would
// have multiplied the drift surface (ADR-0014 §1 item 5).
// Centralising imports also locks down the sole entry point for
// URL sanitisation — important now that the wrapper has to know
// the full set of permitted schemes (currently only `object:`).
//
// If this test ever fires, the right fix is to add the new
// import inside markdown/objectMarkdown.tsx, not to inline it
// at the call site.
func TestDefaultUrlTransformIsCentralised(t *testing.T) {
	srcRoot := frontendSrcRoot(t)
	expectedPath := filepath.Join(srcRoot, "markdown", "objectMarkdown.tsx")

	var importers []string
	err := filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".ts" && ext != ".tsx" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "defaultUrlTransform") {
			importers = append(importers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(importers) != 1 || importers[0] != expectedPath {
		rels := make([]string, len(importers))
		for i, p := range importers {
			rel, _ := filepath.Rel(srcRoot, p)
			rels[i] = rel
		}
		t.Fatalf("defaultUrlTransform should be imported only by markdown/objectMarkdown.tsx (ADR-0014). Importers found:\n  - %s",
			strings.Join(rels, "\n  - "))
	}
}
