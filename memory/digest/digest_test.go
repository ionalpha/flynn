package digest_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/memory/digest"
	"github.com/ionalpha/flynn/state"
)

func TestSummarize(t *testing.T) {
	tests := []struct {
		name    string
		content string
		max     int
		want    string
	}{
		{"empty", "", 80, ""},
		{"whitespace only", "  \n\t ", 80, ""},
		{
			"collapses whitespace to one line",
			"the deploy needs\n\tthe   release tag",
			80,
			"the deploy needs the release tag",
		},
		{
			"cuts at the first sentence",
			"The operator prefers short answers. They also dislike tables. And lists.",
			200,
			"The operator prefers short answers.",
		},
		{
			"keeps the terminator when it ends the content",
			"The operator prefers short answers.",
			200,
			"The operator prefers short answers.",
		},
		{
			"a terminator inside a word does not end a sentence",
			"Deploys run from ci/release.yaml on every tagged commit",
			200,
			"Deploys run from ci/release.yaml on every tagged commit",
		},
		{
			"an early abbreviation does not end the sentence",
			"Use e.g. the staging cluster for anything unproven.",
			200,
			"Use e.g. the staging cluster for anything unproven.",
		},
		{
			"truncates on a word boundary",
			"the operator wants every release announced in the channel before it ships",
			30,
			"the operator wants every...",
		},
		{
			"truncates a single long word where it falls",
			strings.Repeat("x", 60),
			20,
			strings.Repeat("x", 17) + "...",
		},
		{
			"a zero cap does not truncate",
			"the operator wants every release announced in the channel",
			0,
			"the operator wants every release announced in the channel",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := digest.Summarize(tc.content, tc.max); got != tc.want {
				t.Fatalf("Summarize(%q, %d) = %q, want %q", tc.content, tc.max, got, tc.want)
			}
		})
	}
}

func TestSummarizeNeverExceedsTheCap(t *testing.T) {
	content := "The operator prefers short answers and dislikes tables in every report they read."
	for limit := 1; limit <= len(content)+5; limit++ {
		if got := digest.Summarize(content, limit); len(got) > limit {
			t.Fatalf("Summarize(_, %d) = %q, %d chars over the cap", limit, got, len(got)-limit)
		}
	}
}

func TestSummarizeKeepsMultiByteRunesWhole(t *testing.T) {
	content := strings.Repeat("é", 40)
	for limit := 4; limit <= 60; limit++ {
		got := digest.Summarize(content, limit)
		if !utf8Valid(got) {
			t.Fatalf("Summarize(_, %d) = %q, cut a rune in half", limit, got)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestLineText(t *testing.T) {
	tests := []struct {
		name string
		line digest.Line
		want string
	}{
		{
			"id, kind and summary",
			digest.Line{MemoryID: "m1", Kind: "preference", Summary: "short answers"},
			"- m1 [preference]: short answers",
		},
		{
			"no kind",
			digest.Line{MemoryID: "m1", Summary: "short answers"},
			"- m1: short answers",
		},
		{
			"no summary",
			digest.Line{MemoryID: "m1", Kind: "fact"},
			"- m1 [fact]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.line.Text(); got != tc.want {
				t.Fatalf("Text() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDigestTextAndIDs(t *testing.T) {
	d := digest.Digest{Lines: []digest.Line{
		{MemoryID: "m1", Kind: "fact", Summary: "one"},
		{MemoryID: "m2", Kind: "fact", Summary: "two"},
	}}
	if got, want := d.Text(), "- m1 [fact]: one\n- m2 [fact]: two"; got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
	if got := d.IDs(); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("IDs() = %v, want [m1 m2]", got)
	}
}

func TestEmptyDigestRendersNothing(t *testing.T) {
	var d digest.Digest
	if got := d.Text(); got != "" {
		t.Fatalf("Text() = %q, want empty", got)
	}
	if got := d.IDs(); len(got) != 0 {
		t.Fatalf("IDs() = %v, want empty", got)
	}
}

func TestQueryWidensToTheAncestorChain(t *testing.T) {
	scope := state.Scope{Instance: "i", Project: "p", Workspace: "w"}
	q := digest.Query(scope)
	if q.Scope != scope {
		t.Fatalf("Scope = %+v, want %+v", q.Scope, scope)
	}
	if !q.IncludeAncestors {
		t.Fatal("IncludeAncestors = false, want the widened read")
	}
	if got, want := len(q.ScopeChain()), 4; got != want {
		t.Fatalf("ScopeChain() has %d scopes, want %d", got, want)
	}
}
