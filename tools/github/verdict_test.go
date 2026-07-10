package github

import (
	"strings"
	"testing"
)

// The verdict links the findings rather than restating them. GitHub requires a body on
// a COMMENT or REQUEST_CHANGES review, so the body exists either way; what it must not
// contain is the findings written out a second time.
func TestVerdictBodyLinksFindingsInDiffOrder(t *testing.T) {
	findings := []ReviewComment{
		{Path: "z.go", Line: 9, HTMLURL: "https://gh/z9"},
		{Path: "a.go", Line: 40, HTMLURL: "https://gh/a40"},
		{Path: "a.go", Line: 4, HTMLURL: "https://gh/a4"},
	}
	body := verdictBody("  These need addressing.  ", findings)

	want := "These need addressing.\n\n3 findings:\n" +
		"- [`a.go:4`](https://gh/a4)\n" +
		"- [`a.go:40`](https://gh/a40)\n" +
		"- [`z.go:9`](https://gh/z9)\n"
	if body != want {
		t.Fatalf("verdict body =\n%q\nwant\n%q", body, want)
	}
}

// A verdict with nothing posted inline is just its conclusion: no empty list, no
// heading for findings that do not exist.
func TestVerdictBodyWithoutFindingsIsJustTheConclusion(t *testing.T) {
	if got := verdictBody("Nothing blocking.", nil); got != "Nothing blocking." {
		t.Fatalf("verdict body = %q", got)
	}
}

// One finding reads as one finding. A reviewer that says "1 findings" is a reviewer
// nobody trusts with the sentence above it.
func TestVerdictBodySingularOneFinding(t *testing.T) {
	got := verdictBody("Needs a change.", []ReviewComment{{Path: "a.go", Line: 1, HTMLURL: "https://gh/a1"}})
	if !strings.Contains(got, "One finding:\n") {
		t.Fatalf("verdict body = %q, want a singular heading", got)
	}
}

// A comment GitHub returned without an address is still named, never dropped: a reader
// who cannot click it can still find it.
func TestVerdictBodyNamesAnUnlinkableFinding(t *testing.T) {
	got := verdictBody("Needs a change.", []ReviewComment{{Path: "a.go", Line: 7}})
	if !strings.Contains(got, "- `a.go:7`\n") || strings.Contains(got, "](") {
		t.Fatalf("verdict body = %q, want the finding named without a link", got)
	}
}
