package github

import (
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// The parsers in this file all read bytes the agent did not author: a Link header
// GitHub sends, a comment body any user may write, and a patch the API returns. The
// properties below hold for every input, and the fuzz targets assert they never
// panic on one.

// TestProp_MarkerSurvivesRendering is the property behind comment reconciliation:
// whatever a finding says, the marker embedded in its rendered body is exactly the
// marker the finding computes. If this ever fails, a re-review stops recognising
// its own comments and starts duplicating them.
func TestProp_MarkerSurvivesRendering(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		f := Finding{
			Path:    rapid.String().Draw(rt, "path"),
			Line:    rapid.IntRange(1, 1<<20).Draw(rt, "line"),
			Rule:    rapid.String().Draw(rt, "rule"),
			Summary: rapid.String().Draw(rt, "summary"),
			Failure: rapid.String().Draw(rt, "failure"),
		}
		if got := markerIn(f.render()); got != f.marker() {
			rt.Fatalf("markerIn(render()) = %q, want %q", got, f.marker())
		}
	})
}

// TestProp_FindingIdentityIgnoresProse is the other half of reconciliation: a
// finding's identity is its location and its rule, never its wording. Rewording
// must update the existing comment, so the key may not move when only the prose
// changes, and must move when the location or rule does.
func TestProp_FindingIdentityIgnoresProse(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		path := rapid.String().Draw(rt, "path")
		line := rapid.IntRange(1, 1<<20).Draw(rt, "line")
		rule := rapid.String().Draw(rt, "rule")

		a := Finding{Path: path, Line: line, Rule: rule, Summary: rapid.String().Draw(rt, "s1"), Failure: rapid.String().Draw(rt, "f1")}
		b := Finding{Path: path, Line: line, Rule: rule, Summary: rapid.String().Draw(rt, "s2"), Failure: rapid.String().Draw(rt, "f2")}
		if a.key() != b.key() {
			rt.Fatalf("rewording moved the key: %q vs %q", a.key(), b.key())
		}

		// A different line is a different finding.
		c := a
		c.Line = line + 1
		if a.key() == c.key() {
			rt.Fatalf("a different line kept the key %q", a.key())
		}
	})
}

// TestProp_TruncateNeverExceedsTheCapOrCutsMidLine is the property behind the patch
// cap: a truncated patch is always a prefix of the original, never longer than the
// cap, reports truncation exactly when it shortened something, and never ends inside
// a line when a line boundary was available to end on.
//
// The last clause is the one that matters and the one that was wrong: a patch whose
// only newline is its first byte was cut mid-line, so a model could read half a line
// of diff as though it were the whole of one.
func TestProp_TruncateNeverExceedsTheCapOrCutsMidLine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		patch := rapid.String().Draw(rt, "patch")
		limit := rapid.IntRange(0, 4096).Draw(rt, "limit")

		out := truncatePatches([]ChangedFile{{Filename: "f", Patch: patch}}, limit)
		got := out[0]

		if !strings.HasPrefix(patch, got.Patch) {
			rt.Fatalf("result is not a prefix of the input: %q from %q", got.Patch, patch)
		}
		if len(got.Patch) > limit {
			rt.Fatalf("result of %d bytes exceeds the %d-byte cap", len(got.Patch), limit)
		}
		if got.PatchTruncated != (got.Patch != patch) {
			rt.Fatalf("PatchTruncated = %v but shortened = %v", got.PatchTruncated, got.Patch != patch)
		}
		// A shortened patch that kept any newline ended on a boundary. A shortened patch
		// with no newline had no boundary to end on, and a partial line survives only
		// there.
		if got.PatchTruncated && strings.Contains(got.Patch, "\n") && !strings.HasSuffix(got.Patch, "\n") {
			rt.Fatalf("truncated patch ends mid-line: %q (from %q, limit %d)", got.Patch, patch, limit)
		}
	})
}

// FuzzMarkerIn throws arbitrary comment bodies at the marker scanner: bodies with
// no marker, truncated markers, several markers, NULs, and non-ASCII. A body is
// written by whoever opened the pull request, so the scanner must never panic, and
// anything it returns must be a real marker found in the body. Mistaking a human's
// comment for the reviewer's own would let a re-review overwrite it.
func FuzzMarkerIn(f *testing.F) {
	seeds := []string{
		"",
		"a normal human comment",
		markerPrefix + "abc123 -->\nbody",
		markerPrefix,         // opened, never closed
		markerPrefix + "-->", // empty key
		"-->" + markerPrefix, // closer before opener
		markerPrefix + "a -->" + markerPrefix + "b -->", // two markers
		markerPrefix + "summary -->",                    // the marker shape a previous release used
		"<!-- flynn-review\x00 -->",
		"prefix <!-- flynn-review:🔥 --> suffix",
		strings.Repeat(markerPrefix, 8),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body string) {
		got := markerIn(body) // must not panic on any input
		if got == "" {
			return
		}
		if !strings.Contains(body, got) {
			t.Fatalf("returned a marker not present in the body: %q", got)
		}
		if !strings.HasPrefix(got, markerPrefix) || !strings.HasSuffix(got, "-->") {
			t.Fatalf("returned a malformed marker: %q", got)
		}
	})
}

// FuzzNextLink throws arbitrary Link headers at the pagination parser. GitHub sends
// this header, so it is foreign input: a malformed one must not panic, and any URL
// the parser follows must have come from the header it was handed. A parser that
// invented a URL would send an authenticated request somewhere nobody named.
func FuzzNextLink(f *testing.F) {
	seeds := []string{
		"",
		`<https://api.github.com/x?page=2>; rel="next"`,
		`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`,
		`<https://api.github.com/x?page=9>; rel="last"`,
		`<>; rel="next"`,
		`<https://a>; rel="next`,
		`rel="next"`,
		`<a>;rel="next"`,
		`<a>;  rel="next"`,
		`<<a>; rel="next"`,
		`<a<b>; rel="next"`,
		`<>>; rel="next"`,
		"<\x00>; rel=\"next\"",
		strings.Repeat(`<a>; rel="next", `, 32),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, header string) {
		got := nextLink(header) // must not panic on any input
		if got == "" {
			return
		}
		if !strings.Contains(header, got) {
			t.Fatalf("returned a URL not present in the header: %q", got)
		}
		if strings.ContainsAny(got, "<>") {
			t.Fatalf("returned a URL carrying its own delimiters: %q", got)
		}
		if strings.Contains(header, "rel=\"next\"") && got == "" {
			return // a malformed or absent next link is fine
		}
	})
}

// FuzzTruncatePatches throws arbitrary patch bytes and caps at the truncator. A
// patch is whatever the API returned for whatever a contributor wrote, so the
// truncator must never panic, never grow the input, and never exceed the cap.
func FuzzTruncatePatches(f *testing.F) {
	f.Add("", 0)
	f.Add("one line", 4)
	f.Add("a\nb\nc\n", 3)
	f.Add("\n\n\n", 1)
	f.Add("no newline at all", 1)
	f.Add("🔥🔥🔥", 5) // a cap landing inside a multi-byte rune
	f.Add("a\n🔥", 3)

	f.Fuzz(func(t *testing.T, patch string, cap_ int) {
		if cap_ < 0 {
			t.Skip("the cap is normalised to a positive value before it reaches here")
		}
		out := truncatePatches([]ChangedFile{{Patch: patch}}, cap_) // must not panic
		got := out[0]
		if len(got.Patch) > cap_ {
			t.Fatalf("result of %d bytes exceeds the %d-byte cap", len(got.Patch), cap_)
		}
		if !strings.HasPrefix(patch, got.Patch) {
			t.Fatalf("result is not a prefix of the input")
		}
		if got.PatchTruncated != (got.Patch != patch) {
			t.Fatalf("PatchTruncated = %v but shortened = %v", got.PatchTruncated, got.Patch != patch)
		}
	})
}

// FuzzFindingKey throws arbitrary finding coordinates at the identity function. The
// path and rule come from the model, so a key must always be computable, always be
// stable, and never collide for distinct coordinates within a single pull request.
func FuzzFindingKey(f *testing.F) {
	f.Add("a.go", 1, "rule")
	f.Add("", 0, "")
	f.Add("a\x00b", 2, "r")
	f.Add("path/with\nnewline", 3, "rule\x00with-nul")
	f.Add("🔥/main.go", 4, "🔥")

	f.Fuzz(func(t *testing.T, path string, line int, rule string) {
		a := Finding{Path: path, Line: line, Rule: rule}
		// Two findings built from the same coordinates are the same finding, which is
		// what lets a re-review recognise the comment it posted last time.
		again := Finding{Path: path, Line: line, Rule: rule}
		if a.key() != again.key() {
			t.Fatalf("key is not stable across identical coordinates: %q vs %q", a.key(), again.key())
		}
		// The separator must not be forgeable: a path ending where a rule begins
		// cannot produce the same key as the fields swapped around the boundary.
		b := Finding{Path: path + strconv.Itoa(line), Line: 0, Rule: rule}
		if line != 0 && a.key() == b.key() {
			t.Fatalf("fields ran together across the separator: %q", a.key())
		}
		if !strings.HasPrefix(a.marker(), markerPrefix) || !strings.HasSuffix(a.marker(), "-->") {
			t.Fatalf("malformed marker: %q", a.marker())
		}
	})
}

// FuzzParsePrivateKey throws arbitrary bytes at the App-key parser. The bytes come
// from a secret store or an environment variable, so a wrong, truncated, or
// re-encoded value is ordinary rather than exceptional: the parser must reject it
// with an error and never panic. A key it does accept must be usable, so an accepted
// input is asserted to carry a valid modulus rather than a zero-value struct.
func FuzzParsePrivateKey(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("ghp_a_token_pasted_where_a_key_belongs"))
	f.Add(pemFixture("RSA PRIVATE KEY", ""))
	f.Add(pemFixture("PRIVATE KEY", "AAAA\n"))
	f.Add([]byte(pemBegin("RSA PRIVATE KEY"))) // opened, never closed
	f.Add([]byte("\n\n\n"))
	f.Add(pemFixture("PUBLIC KEY", "AAAA\n"))

	f.Fuzz(func(t *testing.T, pemBytes []byte) {
		key, err := ParsePrivateKey(pemBytes) // must not panic on any input
		if err != nil {
			if key != nil {
				t.Fatal("returned both a key and an error")
			}
			return
		}
		if key == nil {
			t.Fatal("returned neither a key nor an error")
		}
		if key.N == nil || key.N.Sign() <= 0 {
			t.Fatal("accepted a key with no modulus")
		}
		if err := key.Validate(); err != nil {
			t.Fatalf("accepted a key that does not validate: %v", err)
		}
	})
}

// pemBegin and pemFixture assemble PEM armour at run time rather than embedding it
// as a literal. A secret scanner matches the armour, not the key: a test fixture
// carrying the literal header trips it, and the honest answers are either to
// allowlist a pattern (which would blind the scanner to a real key committed beside
// it) or to not write the literal. This is the second.
func pemBegin(kind string) string { return "-----BEGIN " + kind + "-----" }

func pemFixture(kind, body string) []byte {
	return []byte(pemBegin(kind) + "\n" + body + "-----END " + kind + "-----\n")
}
