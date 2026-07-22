package skillmd_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillmd"
)

// ignoreUnexported lets a Doc be compared on the fields callers can set; the key
// order it remembers is an implementation detail of lossless rendering.
var docOpts = []cmp.Option{
	cmpopts.IgnoreUnexported(skillmd.Doc{}),
	cmpopts.EquateEmpty(),
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want skillmd.Doc
	}{
		{
			name: "minimal required fields",
			src:  "---\nname: pdf-filler\ndescription: Fill PDF forms.\n---\nBody text.\n",
			want: skillmd.Doc{
				Name:        "pdf-filler",
				Description: "Fill PDF forms.",
				Body:        "Body text.\n",
			},
		},
		{
			name: "every defined field",
			src: "---\nname: a\ndescription: d\nlicense: Apache-2.0\n" +
				"compatibility: Requires Python 3.14\nallowed-tools: Read Write Bash\n" +
				"metadata:\n  check: go test ./...\n  tier: craft\n---\n",
			want: skillmd.Doc{
				Name:          "a",
				Description:   "d",
				License:       "Apache-2.0",
				Compatibility: "Requires Python 3.14",
				AllowedTools:  []string{"Read", "Write", "Bash"},
				Metadata:      map[string]string{"check": "go test ./...", "tier": "craft"},
			},
		},
		{
			name: "undefined keys are preserved, not refused",
			src:  "---\nname: a\ndescription: d\nversion: 2.1.0\nauthor: someone\n---\n",
			want: skillmd.Doc{
				Name:        "a",
				Description: "d",
				Unknown:     map[string]string{"version": "2.1.0", "author": "someone"},
			},
		},
		{
			name: "literal block scalar keeps newlines",
			src:  "---\nname: a\ndescription: |\n  first\n  second\n---\n",
			want: skillmd.Doc{Name: "a", Description: "first\nsecond\n"},
		},
		{
			name: "strip indicator drops the trailing newline",
			src:  "---\nname: a\ndescription: |-\n  first\n  second\n---\n",
			want: skillmd.Doc{Name: "a", Description: "first\nsecond"},
		},
		{
			name: "folded block joins lines with spaces",
			src:  "---\nname: a\ndescription: >-\n  first\n  second\n---\n",
			want: skillmd.Doc{Name: "a", Description: "first second"},
		},
		{
			name: "quoted scalars resolve their escapes",
			src:  "---\nname: a\ndescription: \"line\\nbreak\"\nlicense: 'it''s mine'\n---\n",
			want: skillmd.Doc{Name: "a", Description: "line\nbreak", License: "it's mine"},
		},
		{
			name: "full-line comments and blanks are skipped",
			src:  "---\n# a comment\n\nname: a\ndescription: d\n---\n",
			want: skillmd.Doc{Name: "a", Description: "d"},
		},
		{
			name: "a trailing hash stays part of the value",
			src:  "---\nname: a\ndescription: use tag #2 here\n---\n",
			want: skillmd.Doc{Name: "a", Description: "use tag #2 here"},
		},
		{
			name: "crlf line endings parse",
			src:  "---\r\nname: a\r\ndescription: d\r\n---\r\nBody\r\n",
			want: skillmd.Doc{Name: "a", Description: "d", Body: "Body\r\n"},
		},
		{
			name: "a body may contain its own rule",
			src:  "---\nname: a\ndescription: d\n---\nintro\n\n---\n\noutro\n",
			want: skillmd.Doc{Name: "a", Description: "d", Body: "intro\n\n---\n\noutro\n"},
		},
		{
			name: "colon in a value splits only at the first one",
			src:  "---\nname: a\ndescription: Use when: the thing happens\n---\n",
			want: skillmd.Doc{Name: "a", Description: "Use when: the thing happens"},
		},
		{
			name: "empty frontmatter parses to an empty doc",
			src:  "---\n---\nbody\n",
			want: skillmd.Doc{Body: "body\n"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := skillmd.Parse([]byte(tc.src))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, docOpts...); diff != "" {
				t.Errorf("Parse mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want error
	}{
		{"no frontmatter at all", "just a markdown file\n", skillmd.ErrNoFrontmatter},
		{"frontmatter not at byte 0", "\n---\nname: a\n---\n", skillmd.ErrNoFrontmatter},
		{"never closed", "---\nname: a\ndescription: d\n", skillmd.ErrUnterminated},
		{"opener only", "---\n", skillmd.ErrUnterminated},
		{"line without a colon", "---\nname a\n---\n", skillmd.ErrSyntax},
		{"empty key", "---\n: value\n---\n", skillmd.ErrSyntax},
		{"unexpected indent", "---\nname: a\n  stray: x\n---\n", skillmd.ErrSyntax},
		{"anchor", "---\nname: &anchor a\n---\n", skillmd.ErrSyntax},
		{"alias", "---\nname: *alias\n---\n", skillmd.ErrSyntax},
		{"explicit tag", "---\nname: !!str a\n---\n", skillmd.ErrSyntax},
		{"flow sequence", "---\nname: [a, b]\n---\n", skillmd.ErrSyntax},
		{"flow mapping for metadata", "---\nmetadata: {a: b}\n---\n", skillmd.ErrSyntax},
		{"unterminated double quote", "---\nname: \"a\n---\n", skillmd.ErrSyntax},
		{"unsupported escape", "---\nname: \"a\\qb\"\n---\n", skillmd.ErrSyntax},
		{"inconsistent metadata indent", "---\nmetadata:\n  a: 1\n    b: 2\n---\n", skillmd.ErrSyntax},
		{"block scalar with no content", "---\ndescription: |\n---\n", skillmd.ErrSyntax},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := skillmd.Parse([]byte(tc.src))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseRejectsOversizedInput(t *testing.T) {
	t.Parallel()

	src := "---\nname: a\ndescription: " + strings.Repeat("x", skillmd.MaxDocSize) + "\n---\n"
	if _, err := skillmd.Parse([]byte(src)); !errors.Is(err, skillmd.ErrTooLarge) {
		t.Fatalf("Parse error = %v, want ErrTooLarge", err)
	}
}

func TestValidateName(t *testing.T) {
	t.Parallel()

	valid := []string{"a", "pdf-filler", "a1", "one-two-three", strings.Repeat("a", 64)}
	for _, name := range valid {
		if err := skillmd.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",                      // empty
		strings.Repeat("a", 65), // too long
		"-leading",              // leading hyphen
		"trailing-",             // trailing hyphen
		"double--hyphen",        // consecutive hyphens
		"Upper",                 // uppercase
		"has space",             // space
		"under_score",           // underscore
		"dot.separated",         // dot
		"unicodé",               // non-ascii
	}
	for _, name := range invalid {
		if err := skillmd.ValidateName(name); !errors.Is(err, skillmd.ErrInvalid) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	base := skillmd.Doc{Name: "a-skill", Description: "does a thing"}

	t.Run("accepts a conformant doc", func(t *testing.T) {
		t.Parallel()
		if err := skillmd.Validate(base, "a-skill"); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("skips the directory check when no directory is given", func(t *testing.T) {
		t.Parallel()
		if err := skillmd.Validate(base, ""); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	tests := []struct {
		name string
		doc  skillmd.Doc
		dir  string
	}{
		{"name must match the directory", base, "different"},
		{"description is required", skillmd.Doc{Name: "a-skill"}, "a-skill"},
		{
			name: "description length is capped",
			doc:  skillmd.Doc{Name: "a-skill", Description: strings.Repeat("x", skillmd.MaxDescriptionLen+1)},
			dir:  "a-skill",
		},
		{
			name: "compatibility length is capped",
			doc: skillmd.Doc{
				Name: "a-skill", Description: "d",
				Compatibility: strings.Repeat("x", skillmd.MaxCompatibilityLen+1),
			},
			dir: "a-skill",
		},
		{
			name: "undefined keys are refused on publish",
			doc:  skillmd.Doc{Name: "a-skill", Description: "d", Unknown: map[string]string{"version": "1"}},
			dir:  "a-skill",
		},
		{
			name: "metadata keys must not be blank",
			doc:  skillmd.Doc{Name: "a-skill", Description: "d", Metadata: map[string]string{" ": "v"}},
			dir:  "a-skill",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := skillmd.Validate(tc.doc, tc.dir); !errors.Is(err, skillmd.ErrInvalid) {
				t.Errorf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestValidateAcceptsWhatParseAccepts pins the boundary between the two levels of
// strictness: a document carrying an undefined key is readable and is not
// publishable. Import tolerance and export conformance are different questions and
// this is the test that keeps them different.
func TestParseToleratesWhatValidateRefuses(t *testing.T) {
	t.Parallel()

	src := "---\nname: a-skill\ndescription: d\nversion: 9\n---\n"
	doc, err := skillmd.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := doc.Unknown["version"]; got != "9" {
		t.Errorf("Unknown[version] = %q, want %q", got, "9")
	}
	if err := skillmd.Validate(doc, "a-skill"); !errors.Is(err, skillmd.ErrInvalid) {
		t.Errorf("Validate = %v, want ErrInvalid", err)
	}
}

// TestFormatPreservesForeignKeyOrder checks that re-exporting someone else's file
// does not quietly reorder it, which would make every imported pack show up as
// modified the first time we wrote it back.
func TestFormatPreservesForeignKeyOrder(t *testing.T) {
	t.Parallel()

	src := "---\nname: a\ndescription: d\nzeta: 1\nalpha: 2\n---\nbody\n"
	doc, err := skillmd.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := skillmd.Format(doc)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	zeta := strings.Index(string(out), "zeta")
	alpha := strings.Index(string(out), "alpha")
	if zeta < 0 || alpha < 0 || zeta > alpha {
		t.Errorf("foreign key order not preserved:\n%s", out)
	}
}

// genDoc builds documents that stress the writer's quoting decisions: values with
// newlines, leading and trailing spaces, quotes, colons, hashes and backslashes are
// exactly the ones a naive renderer loses.
func genDoc() *rapid.Generator[skillmd.Doc] {
	awkward := rapid.SampledFrom([]string{
		"", "plain", "with space", " leading", "trailing ", "a: colon", "#hash",
		"'single'", `"double"`, `back\slash`, "tab\there", "line\nbreak",
		"\nleading newline", "trailing newline\n", "  indented\nline", "多字节",
		"- dash", "| pipe", "> gt", "{brace}", "[bracket]", "&anchor", "*alias",
	})
	ident := rapid.StringMatching(`[a-z][a-z0-9_-]{0,12}`)
	tool := rapid.StringMatching(`[A-Za-z][A-Za-z0-9_]{0,10}`)

	return rapid.Custom(func(t *rapid.T) skillmd.Doc {
		unknown := map[string]string{}
		for k, v := range rapid.MapOfN(ident, awkward, 0, 3).Draw(t, "unknown") {
			if !isDefinedKey(k) {
				unknown[k] = v
			}
		}
		return skillmd.Doc{
			Name:          awkward.Draw(t, "name"),
			Description:   awkward.Draw(t, "description"),
			License:       awkward.Draw(t, "license"),
			Compatibility: awkward.Draw(t, "compatibility"),
			AllowedTools:  rapid.SliceOfN(tool, 0, 3).Draw(t, "allowedTools"),
			Metadata:      rapid.MapOfN(ident, awkward, 0, 3).Draw(t, "metadata"),
			Unknown:       unknown,
			Body:          rapid.SampledFrom([]string{"", "body\n", "# Heading\n\ntext\n", "---\n"}).Draw(t, "body"),
		}
	})
}

func isDefinedKey(k string) bool {
	switch k {
	case "name", "description", "license", "compatibility", "metadata", "allowed-tools":
		return true
	}
	return false
}

// TestRoundTripProperty is the contract the export path depends on: anything we
// render, we can read back unchanged. Without it, "import then export is lossless"
// is a hope rather than a property.
func TestRoundTripProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		want := genDoc().Draw(t, "doc")

		encoded, err := skillmd.Format(want)
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		got, err := skillmd.Parse(encoded)
		if err != nil {
			t.Fatalf("Parse of rendered doc failed: %v\nrendered:\n%s", err, encoded)
		}
		if diff := cmp.Diff(want, got, docOpts...); diff != "" {
			t.Fatalf("round trip mismatch (-want +got):\n%s\nrendered:\n%s", diff, encoded)
		}
	})
}

// TestReparseIsStable checks the second round trip is a fixed point. A parser that
// is merely self-consistent can still drift a value one step at a time; this pins it.
func TestReparseIsStable(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		doc := genDoc().Draw(t, "doc")

		first, err := skillmd.Format(doc)
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		parsed, err := skillmd.Parse(first)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		second, err := skillmd.Format(parsed)
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		if string(first) != string(second) {
			t.Fatalf("rendering is not a fixed point:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
	})
}

// FuzzParse drives the parser with arbitrary bytes. A pack fetched from a public
// registry is exactly this: input nobody vetted. The parser must return a document
// or an error, never panic, and anything it does accept must survive a round trip,
// so a hostile file cannot become a skill whose stored content differs from the file
// that was reviewed.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"---\nname: a\ndescription: d\n---\nbody\n",
		"---\nmetadata:\n  k: v\n---\n",
		"---\ndescription: |\n  block\n---\n",
		"---\r\nname: a\r\n---\r\n",
		"---\n---\n",
		"---\n",
		"",
		"not a skill file",
		"---\nname: \"unterminated\n---\n",
		"---\nname: &a x\n---\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		doc, err := skillmd.Parse(src)
		if err != nil {
			return
		}
		encoded, err := skillmd.Format(doc)
		if err != nil {
			t.Fatalf("Format of a parsed doc failed: %v\ninput: %q", err, src)
		}
		again, err := skillmd.Parse(encoded)
		if err != nil {
			t.Fatalf("re-parse of rendered doc failed: %v\ninput: %q\nrendered: %q", err, src, encoded)
		}
		if diff := cmp.Diff(doc, again, docOpts...); diff != "" {
			t.Fatalf("round trip mismatch (-first +second):\n%s\ninput: %q", diff, src)
		}
	})
}
