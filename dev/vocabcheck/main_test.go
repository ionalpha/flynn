package main

import (
	"bytes"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Test phrases are assembled from fragments at runtime so the flagged text
// never appears verbatim in this file: the gate scans this PR's own diff.
func banned(parts ...string) string { return strings.Join(parts, "") }

func denyFor(t *testing.T, phrases ...string) denylist {
	t.Helper()
	var raw strings.Builder
	for _, p := range phrases {
		tokens := tokensOf(p)
		if len(tokens) == 0 {
			t.Fatalf("phrase %q normalizes to nothing", p)
		}
		raw.WriteString(strings.Join([]string{intToStr(len(tokens)), hashPhrase(tokens)}, " ") + "\n")
	}
	d, err := parseDenylist(raw.String())
	if err != nil {
		t.Fatalf("parseDenylist: %v", err)
	}
	return d
}

func intToStr(n int) string { return string(rune('0' + n)) }

func TestTokensOf(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Hello, World!", "hello world"},
		{"it's a test", "its a test"},
		{"hyphen-joined words", "hyphen joined words"},
		{"under_scored and slash/split", "under scored and slash split"},
		{"  spaced   out  ", "spaced out"},
		{"MiXeD CaSe 123", "mixed case 123"},
		{"", ""},
		{"!!!", ""},
	}
	for _, c := range cases {
		got := strings.Join(tokensOf(c.in), " ")
		if got != c.want {
			t.Errorf("tokensOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCheckLineDenylist(t *testing.T) {
	word := banned("fly", "wheel")
	phrase := banned("unifi", "cation epi", "c")
	deny := denyFor(t, word, phrase)

	if got := checkLine("a perfectly ordinary sentence", deny); len(got) != 0 {
		t.Errorf("clean line flagged: %v", got)
	}
	if got := checkLine("the "+word+" spins", deny); len(got) != 1 {
		t.Errorf("single word: got %v, want 1 finding", got)
	}
	// Hyphenation and case do not hide a phrase: normalization splits on
	// punctuation and lowercases before hashing.
	parts := tokensOf(phrase)
	disguised := strings.ToUpper(parts[0]) + "-" + parts[1]
	if got := checkLine("part of the "+disguised+" work", deny); len(got) != 1 {
		t.Errorf("disguised phrase: got %v, want 1 finding", got)
	}
	// A phrase is matched whole: its individual words alone do not trip it.
	if got := checkLine("the "+parts[1]+" of "+parts[0], deny); len(got) != 0 {
		t.Errorf("split words flagged: %v", got)
	}
}

func TestCheckLineTypography(t *testing.T) {
	deny := denyFor(t, "placeholder")
	if got := checkLine("dash \u2014 and dots \u2026 here", deny); len(got) != 2 {
		t.Errorf("typography: got %v, want 2 findings", got)
	}
	if got := checkLine("plain hyphen - and three dots ...", deny); len(got) != 0 {
		t.Errorf("plain punctuation flagged: %v", got)
	}
}

func TestCheckLineInternalRef(t *testing.T) {
	deny := denyFor(t, "placeholder")
	// Assembled at runtime so this file's own diff never contains a
	// noun-plus-id pair the gate would flag.
	for _, line := range []string{
		"see task " + "`deadbeef` for details",
		"tracked as epic " + "cafebabe internally",
		"Task: " + "0badf00d covers this",
	} {
		if got := checkLine(line, deny); len(got) != 1 {
			t.Errorf("checkLine(%q) = %v, want 1 finding", line, got)
		}
	}
	for _, line := range []string{
		"fixes #204 and closes #180",
		"reverts commit 1a2b3c4d cleanly",
		"the task takes 30 seconds",
	} {
		if got := checkLine(line, deny); len(got) != 0 {
			t.Errorf("checkLine(%q) = %v, want no findings", line, got)
		}
	}
}

func TestParseDenylist(t *testing.T) {
	if _, err := parseDenylist("# comment\n\n2 " + strings.Repeat("ab", 32) + "\n"); err != nil {
		t.Errorf("valid list rejected: %v", err)
	}
	for _, bad := range []string{
		"nonsense\n",
		"2 tooshort\n",
		"0 " + strings.Repeat("ab", 32) + "\n",
	} {
		if _, err := parseDenylist(bad); err == nil {
			t.Errorf("parseDenylist(%q) accepted", bad)
		}
	}
}

func TestScanDiff(t *testing.T) {
	word := banned("fly", "wheel")
	deny := denyFor(t, word)
	diff := strings.Join([]string{
		"--- a/x.go",
		"+++ b/x.go",
		"@@ -1,2 +10,4 @@",
		" context line",
		"+a clean addition",
		"+the " + word + " turns",
		"-removed " + word + " does not count",
		"\\ No newline at end of file",
		"", // trailing newline
	}, "\n")
	got := scanDiff(strings.NewReader(diff), deny)
	if len(got) != 1 {
		t.Fatalf("scanDiff: got %v, want 1 finding", got)
	}
	// The hunk's new side starts at 10: context=10, clean addition=11, flagged=12.
	if got[0].where != "x.go" || got[0].line != 12 {
		t.Errorf("finding at %s:%d, want x.go:12", got[0].where, got[0].line)
	}
}

func TestRunExitCodes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, strings.NewReader("nothing wrong here\n"), &out, &errOut); code != 0 {
		t.Errorf("clean input: exit %d, output %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"-label", "body"}, strings.NewReader("dash \u2014 here\n"), &out, &errOut); code != 1 {
		t.Errorf("flagged input: exit %d", code)
	}
	if !strings.Contains(out.String(), "body:1:") {
		t.Errorf("finding not labeled: %q", out.String())
	}
	out.Reset()
	if code := run([]string{"-hash", "two words"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Errorf("-hash: exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "2 ") || len(strings.TrimSpace(out.String())) != 66 {
		t.Errorf("-hash output %q, want \"2 <64 hex>\"", out.String())
	}
}

// TestEmbeddedDenylist proves the shipped list catches a known entry end to
// end (the -hash printer and the scanner agree on normalization).
func TestEmbeddedDenylist(t *testing.T) {
	word := banned("fly", "wheel")
	var out, errOut bytes.Buffer
	if code := run([]string{"-label", "t"}, strings.NewReader("the "+word+"\n"), &out, &errOut); code != 1 {
		t.Errorf("embedded list missed %q: exit %d, out %q, err %q", word, code, out.String(), errOut.String())
	}
}

// Property: tokensOf yields only lowercase alphanumeric tokens, and matching is
// stable under the punctuation and casing an author might vary.
func TestTokensOfProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		tokens := tokensOf(s)
		for _, tok := range tokens {
			if tok == "" {
				t.Fatalf("empty token from %q", s)
			}
			for _, r := range tok {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
					t.Fatalf("token %q from %q has rune %q", tok, s, r)
				}
			}
		}
		// Re-joining with any separator and re-tokenizing is a fixpoint.
		for _, sep := range []string{" ", "-", "_", "/", ", "} {
			again := tokensOf(strings.Join(tokens, sep))
			if strings.Join(again, " ") != strings.Join(tokens, " ") {
				t.Fatalf("not a fixpoint with sep %q: %v vs %v", sep, again, tokens)
			}
		}
	})
}
