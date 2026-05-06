package sessionio

import (
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/contextbuild"
	"github.com/nlink-jp/shell-agent-v2/internal/memory"
)

const (
	old32 = "0123456789abcdef0123456789abcdef"
	new32 = "fedcba9876543210fedcba9876543210"
	old12 = "abcdef012345"
	new12 = "987654fedcba"
)

func TestRewriteText_Variants(t *testing.T) {
	idMap := map[string]string{
		old32: new32,
		old12: new12,
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "32-hex with object: prefix",
			in:   "see ![alt](object:" + old32 + ") for details",
			want: "see ![alt](object:" + new32 + ") for details",
		},
		{
			name: "32-hex bare ID",
			in:   "ref " + old32 + " here",
			want: "ref " + new32 + " here",
		},
		{
			name: "12-hex legacy with prefix",
			in:   "old (object:" + old12 + ")",
			want: "old (object:" + new12 + ")",
		},
		{
			name: "multiple refs in one string",
			in:   "first " + old32 + " then " + old12 + " end",
			want: "first " + new32 + " then " + new12 + " end",
		},
		{
			name: "unmapped ID left alone",
			in:   "stranger 11112222333344445555666677778888 unchanged",
			want: "stranger 11112222333344445555666677778888 unchanged",
		},
		{
			name: "hex too short to match",
			in:   "abcd is too short",
			want: "abcd is too short",
		},
		{
			name: "non-hex chars in span don't match",
			in:   "word abcdefghij0123456789abcdef0123456789 word",
			want: "word abcdefghij0123456789abcdef0123456789 word",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "no IDs at all",
			in:   "plain message text",
			want: "plain message text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RewriteText(tc.in, idMap)
			if got != tc.want {
				t.Errorf("RewriteText:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewriteText_LongerHexBoundary(t *testing.T) {
	// A 33-char hex run should NOT match the 32-char pattern as a
	// prefix because the trailing char would extend the word.
	// Word boundary at position 32 fails because the next char is
	// also hex (\w).
	idMap := map[string]string{old32: new32}
	in := old32 + "f extra"
	got := RewriteText(in, idMap)
	if got != in {
		t.Errorf("longer hex run should not match a shorter ID prefix:\n  in:   %q\n  got:  %q", in, got)
	}
}

func TestRewriteText_EmptyMap(t *testing.T) {
	in := "ref " + old32 + " here"
	if got := RewriteText(in, nil); got != in {
		t.Errorf("nil map should be no-op, got %q", got)
	}
	if got := RewriteText(in, map[string]string{}); got != in {
		t.Errorf("empty map should be no-op, got %q", got)
	}
}

func TestRewriteRecords_StructuredAndContent(t *testing.T) {
	idMap := map[string]string{old32: new32}
	records := []memory.Record{
		{
			Role:      "user",
			Content:   "see ![](object:" + old32 + ")",
			ObjectIDs: []string{old32, "leave-me-alone-fffffffffffffffff"},
		},
		{
			Role:    "assistant",
			Content: "no refs here",
		},
	}
	RewriteRecords(records, idMap)

	if records[0].ObjectIDs[0] != new32 {
		t.Errorf("ObjectIDs[0] not remapped: got %q want %q", records[0].ObjectIDs[0], new32)
	}
	if records[0].ObjectIDs[1] != "leave-me-alone-fffffffffffffffff" {
		t.Errorf("ObjectIDs[1] should be left alone: got %q", records[0].ObjectIDs[1])
	}
	if !strings.Contains(records[0].Content, new32) {
		t.Errorf("Content should contain new ID: got %q", records[0].Content)
	}
	if strings.Contains(records[0].Content, old32) {
		t.Errorf("Content should not contain old ID: got %q", records[0].Content)
	}
	if records[1].Content != "no refs here" {
		t.Errorf("untouched record changed: got %q", records[1].Content)
	}
}

func TestRewriteSummaries_TextOnly(t *testing.T) {
	idMap := map[string]string{old12: new12}
	entries := []contextbuild.SummaryEntry{
		{Summary: "user uploaded an image (object:" + old12 + ") then asked..."},
		{Summary: "no refs in this one"},
	}
	RewriteSummaries(entries, idMap)
	if !strings.Contains(entries[0].Summary, new12) {
		t.Errorf("summary not rewritten: got %q", entries[0].Summary)
	}
	if entries[1].Summary != "no refs in this one" {
		t.Errorf("untouched summary changed: got %q", entries[1].Summary)
	}
}
