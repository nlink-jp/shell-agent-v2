package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/findings"
	"github.com/nlink-jp/shell-agent-v2/internal/llm"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
	"github.com/nlink-jp/shell-agent-v2/internal/objstore"
)

// stubTextBackend is a deterministic Backend impl for analyze-text
// integration tests. Returns a fixed JSON response per Chat call;
// counts invocations so tests can assert the window loop ran the
// expected number of times.
type stubTextBackend struct {
	response string
	calls    atomic.Int32
}

func (s *stubTextBackend) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (*llm.Response, error) {
	s.calls.Add(1)
	return &llm.Response{Content: s.response}, nil
}

func (s *stubTextBackend) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, cb llm.StreamCallback) (*llm.Response, error) {
	return s.Chat(ctx, msgs, tools)
}

func (s *stubTextBackend) Name() string { return "stub" }

// newTextToolsTestAgent builds a fresh Agent rooted in a TempDir
// with a stubTextBackend, an active session, and a (possibly
// markdown-attached) object pre-populated in objstore.
func newTextToolsTestAgent(t *testing.T, objType objstore.ObjectType, mime, body string) (*Agent, *objstore.ObjectMeta, *stubTextBackend) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	a := New(config.Default())
	stub := &stubTextBackend{
		response: `{"summary": "Test summary about the document.", "new_findings": [{"description": "Found a marker", "severity": "info", "evidence": "line 1"}]}`,
	}
	a.backend = stub

	a.objects = objstore.NewStoreAt(filepath.Join(config.DataDir(), "objects"))
	if err := a.objects.Load(); err != nil {
		t.Fatalf("objstore load: %v", err)
	}

	// Active session needed for analyze-text findings auto-promote.
	session := &memory.Session{
		ID:    "sess-text",
		Title: "T",
		Records: []memory.Record{
			{Role: "user", Content: "hello"},
		},
	}
	if err := session.Save(); err != nil {
		t.Fatalf("session save: %v", err)
	}
	if err := a.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	meta, err := a.objects.Store(strings.NewReader(body), objType, mime, "doc.md", "sess-text")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	return a, meta, stub
}

// TestAnalyzeText_Roundtrip — happy path: attach a small markdown,
// run analyze-text, observe (1) chunks produced > 0, (2) stub
// backend called once per chunk, (3) Findings promoted with
// SourceAnalyzeText.
func TestAnalyzeText_Roundtrip(t *testing.T) {
	body := "# Doc\n\nLine 1\nLine 2\nLine 3\n"
	a, meta, stub := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	args := `{"object": "object:` + meta.ID + `", "perspective": "summarise"}`
	out, err := a.toolAnalyzeText(context.Background(), args)
	if err != nil {
		t.Fatalf("toolAnalyzeText: %v", err)
	}
	if !strings.Contains(out, "Test summary") {
		t.Errorf("output missing summary: %q", out)
	}
	if stub.calls.Load() < 1 {
		t.Errorf("stub backend was not called")
	}
	// Findings should have been promoted with SourceAnalyzeText.
	all := a.findings.All()
	if len(all) == 0 {
		t.Fatal("no findings promoted")
	}
	for _, f := range all {
		if f.Source != findings.SourceAnalyzeText {
			t.Errorf("finding Source = %q, want %q", f.Source, findings.SourceAnalyzeText)
		}
	}
}

// TestAnalyzeText_TypeReportAlsoAccepted — agent-generated
// reports (TypeReport) flow through the same tool unchanged.
// This is the "report on report" chain the design calls out.
func TestAnalyzeText_TypeReportAlsoAccepted(t *testing.T) {
	body := "# Previous Report\n\nFindings: A, B, C.\n"
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeReport, "text/markdown", body)

	args := `{"object": "` + meta.ID + `", "perspective": "follow-up"}`
	_, err := a.toolAnalyzeText(context.Background(), args)
	if err != nil {
		t.Errorf("TypeReport should be accepted: %v", err)
	}
}

// TestGrepText_HitsAndContext — match a known pattern, verify
// line-numbered format and the configured number of context
// lines around each hit.
func TestGrepText_HitsAndContext(t *testing.T) {
	body := "line one\nline two\nERROR here\nline four\nline five\n"
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	args := `{"object": "` + meta.ID + `", "pattern": "ERROR", "context_lines": 1}`
	out, err := a.toolGrepText(args)
	if err != nil {
		t.Fatalf("toolGrepText: %v", err)
	}
	if !strings.Contains(out, "3> ERROR here") {
		t.Errorf("missing matched-line marker (line 3): %q", out)
	}
	if !strings.Contains(out, "2: line two") {
		t.Errorf("missing -B context line: %q", out)
	}
	if !strings.Contains(out, "4: line four") {
		t.Errorf("missing -A context line: %q", out)
	}
	// 1 match → "1 match(es)" header.
	if !strings.Contains(out, "1 match(es)") {
		t.Errorf("match count header missing/wrong: %q", out)
	}
}

// TestGrepText_TooManyMatches — exceeding max_matches yields a
// specific error wording so the LLM knows to narrow.
func TestGrepText_TooManyMatches(t *testing.T) {
	// 50 matching lines.
	var sb strings.Builder
	for range 50 {
		sb.WriteString("hit here\n")
	}
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", sb.String())

	args := `{"object": "` + meta.ID + `", "pattern": "hit", "max_matches": 5}`
	_, err := a.toolGrepText(args)
	if err == nil {
		t.Fatal("expected too-many-matches error")
	}
	if !strings.Contains(err.Error(), "too many matches") {
		t.Errorf("error wording = %q, want contains 'too many matches'", err.Error())
	}
}

// TestGrepText_NoMatches — zero matches returns a friendly
// "(no matches for /pattern/ ...)" string instead of erroring.
func TestGrepText_NoMatches(t *testing.T) {
	body := "alpha\nbeta\ngamma\n"
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	args := `{"object": "` + meta.ID + `", "pattern": "zzznotfound"}`
	out, err := a.toolGrepText(args)
	if err != nil {
		t.Fatalf("toolGrepText: %v", err)
	}
	if !strings.Contains(out, "no matches") {
		t.Errorf("expected 'no matches' message, got: %q", out)
	}
}

// TestGetText_RangeRead — read lines 2-4 verbatim, line-prefixed.
func TestGetText_RangeRead(t *testing.T) {
	body := "one\ntwo\nthree\nfour\nfive\n"
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	args := `{"object": "` + meta.ID + `", "lines": "2-4"}`
	out, err := a.toolGetText(args)
	if err != nil {
		t.Fatalf("toolGetText: %v", err)
	}
	want := "2: two\n3: three\n4: four\n"
	if out != want {
		t.Errorf("output:\n%q\nwant:\n%q", out, want)
	}
}

// TestGetText_RangeTooLarge — > 1000 lines requested → error.
func TestGetText_RangeTooLarge(t *testing.T) {
	body := strings.Repeat("x\n", 2000)
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	args := `{"object": "` + meta.ID + `", "lines": "1-1001"}`
	_, err := a.toolGetText(args)
	if err == nil {
		t.Fatal("expected range-too-large error")
	}
	if !strings.Contains(err.Error(), "line range too large") {
		t.Errorf("error wording = %q", err.Error())
	}
}

// TestTextTools_RejectWrongType — image objects must not be
// accepted; error wording should help the LLM pivot.
func TestTextTools_RejectWrongType(t *testing.T) {
	body := "\x89PNG\r\n\x1a\nfakedata"
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeImage, "image/png", body)

	args := `{"object": "` + meta.ID + `", "perspective": "p"}`
	_, err := a.toolAnalyzeText(context.Background(), args)
	if err == nil {
		t.Fatal("expected type-mismatch error")
	}
	if !strings.Contains(err.Error(), "markdown or report") {
		t.Errorf("error wording = %q, want contains 'markdown or report'", err.Error())
	}
}

// TestTextTools_ResolveObjectPrefix — "object:abc" and "abc"
// must resolve identically so the LLM can use either form.
func TestTextTools_ResolveObjectPrefix(t *testing.T) {
	body := "alpha\nbeta\n"
	a, meta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	args1 := `{"object": "` + meta.ID + `", "lines": "1"}`
	args2 := `{"object": "object:` + meta.ID + `", "lines": "1"}`

	out1, err1 := a.toolGetText(args1)
	out2, err2 := a.toolGetText(args2)
	if err1 != nil || err2 != nil {
		t.Fatalf("get-text errors: %v, %v", err1, err2)
	}
	if out1 != out2 {
		t.Errorf("prefix and bare resolve differently: %q vs %q", out1, out2)
	}
}

// TestListObjects_LinesTokensColumns — when a session contains
// text-bearing objects (markdown / report), the list-objects
// output includes Lines and Tokens columns. Image / blob entries
// still omit those columns so the format stays compact.
func TestListObjects_LinesTokensColumns(t *testing.T) {
	// Trailing newline → bytes.Count('\n')+1 yields 4 (last element
	// of strings.Split is an empty trailing line). This matches the
	// internal get-text semantics (strings.Split based) so the line
	// count surfaced to the LLM addresses the same lines get-text
	// can return.
	body := "line one\nline two\nline three\n"
	a, mdMeta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", body)

	// Also store a non-text object so we can confirm columns omit.
	imgMeta, err := a.objects.Store(strings.NewReader("\x89PNG\r\n\x1a\nfake"), objstore.TypeImage, "image/png", "i.png", "sess-text")
	if err != nil {
		t.Fatalf("Store image: %v", err)
	}

	out := a.toolListObjects("{}")

	// Markdown row carries Lines/Tokens.
	if !strings.Contains(out, "Lines: 4") {
		t.Errorf("expected 'Lines: 4' in markdown row; out=%q", out)
	}
	if !strings.Contains(out, mdMeta.ID) {
		t.Errorf("expected markdown ID %s in output", mdMeta.ID)
	}

	// Image row does NOT carry Lines/Tokens. The simplest assertion
	// is that the substring "Lines:" appears exactly once (in the
	// markdown row) — if it appeared twice the image would have
	// erroneously got the columns too.
	if strings.Count(out, "Lines:") != 1 {
		t.Errorf("Lines should appear exactly once (markdown only); out=%q", out)
	}
	if !strings.Contains(out, imgMeta.ID) {
		t.Errorf("expected image ID %s in output", imgMeta.ID)
	}
}

// TestAgent_SendWithAttachments_PopulatesDocumentIDs pins the
// v0.5 plumbing: a SendWithAttachments call carrying a non-empty
// documentObjectIDs slice writes those IDs to Record.DocumentIDs
// on the new user record. The agent's contextbuild ObjectLookup
// then resolves them into anchor lines on every subsequent turn;
// this test only covers the population step (anchor injection is
// covered by the contextbuild render tests).
func TestAgent_SendWithAttachments_PopulatesDocumentIDs(t *testing.T) {
	// Reuse the same fixture used by the analyze-text tests.
	a, mdMeta, _ := newTextToolsTestAgent(t, objstore.TypeMarkdown, "text/markdown", "hi\n")

	// Stub doesn't actually need to honour the message — it returns
	// the constant JSON response from the fixture for every Chat
	// call. We just need SendWithAttachments to run far enough to
	// append the user record. Use a very short context so the
	// LLM loop terminates promptly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = a.SendWithAttachments(ctx, "summarise this", nil, nil, []string{mdMeta.ID})

	if a.session == nil {
		t.Fatal("session unexpectedly nil after SendWithAttachments")
	}
	// Find the user record we just added; it must carry DocumentIDs.
	var userRec *memory.Record
	for i := range a.session.Records {
		if a.session.Records[i].Role == "user" && a.session.Records[i].Content == "summarise this" {
			userRec = &a.session.Records[i]
			break
		}
	}
	if userRec == nil {
		t.Fatal("user record not found in session")
	}
	if len(userRec.DocumentIDs) != 1 || userRec.DocumentIDs[0] != mdMeta.ID {
		t.Errorf("Record.DocumentIDs = %v, want [%s]", userRec.DocumentIDs, mdMeta.ID)
	}
}

// TestParseLineRange — table-driven coverage of the range
// parser, including clamps and inverted-range error.
func TestParseLineRange(t *testing.T) {
	cases := []struct {
		s          string
		total      int
		wantStart  int
		wantEnd    int
		wantErr    bool
	}{
		{"", 10, 1, 10, false},
		{"5", 10, 5, 5, false},
		{"3-7", 10, 3, 7, false},
		{"5-", 10, 5, 10, false},
		{"-5", 10, 1, 5, false},
		{"3-100", 10, 3, 10, false}, // clamp upper
		{"0-5", 10, 1, 5, false},    // clamp lower
		{"-", 10, 1, 10, false},
		{"foo", 10, 0, 0, true},
		{"3-foo", 10, 0, 0, true},
		{"7-3", 10, 0, 0, true}, // inverted after clamp
		{"5", 0, 0, 0, true},    // empty doc
	}
	for _, c := range cases {
		got1, got2, err := parseLineRange(c.s, c.total)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLineRange(%q,%d) err=%v wantErr=%v", c.s, c.total, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if got1 != c.wantStart || got2 != c.wantEnd {
			t.Errorf("parseLineRange(%q,%d) = (%d,%d), want (%d,%d)", c.s, c.total, got1, got2, c.wantStart, c.wantEnd)
		}
	}
}
