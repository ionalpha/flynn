package skillmd_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ionalpha/flynn/skill/skillmd"
)

// TestParseEdges pins the grammar's branch paths that the main table and the property
// tests do not reliably reach: an empty value, block-scalar chomping and interior
// blank lines, the double-quote escapes, and the metadata-block boundaries.
func TestParseEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want skillmd.Doc
	}{
		{
			name: "an empty value parses to an empty string",
			src:  "---\nname: a\ndescription: d\nlicense:\n---\n",
			want: skillmd.Doc{Name: "a", Description: "d"},
		},
		{
			name: "a literal block keeps an interior blank line",
			src:  "---\nname: a\ndescription: |\n  first\n\n  second\n---\n",
			want: skillmd.Doc{Name: "a", Description: "first\n\nsecond\n"},
		},
		{
			name: "a literal block clips trailing blank lines to one newline",
			src:  "---\nname: a\ndescription: |\n  only\n\n---\n",
			want: skillmd.Doc{Name: "a", Description: "only\n"},
		},
		{
			name: "the keep indicator retains every trailing newline",
			src:  "---\nname: a\ndescription: |+\n  only\n\n---\n",
			want: skillmd.Doc{Name: "a", Description: "only\n\n"},
		},
		{
			name: "a folded block turns a blank line into a paragraph break",
			src:  "---\nname: a\ndescription: >-\n  first\n\n  second\n---\n",
			want: skillmd.Doc{Name: "a", Description: "first\nsecond"},
		},
		{
			name: "a double-quoted carriage-return escape resolves",
			src:  "---\nname: a\ndescription: \"a\\rb\"\n---\n",
			want: skillmd.Doc{Name: "a", Description: "a\rb"},
		},
		{
			name: "a double-quoted backslash escape resolves",
			src:  "---\nname: a\ndescription: \"a\\\\b\"\n---\n",
			want: skillmd.Doc{Name: "a", Description: "a\\b"},
		},
		{
			name: "a metadata block skips comments and blank lines",
			src:  "---\nname: a\ndescription: d\nmetadata:\n  # note\n\n  k: v\n---\n",
			want: skillmd.Doc{Name: "a", Description: "d", Metadata: map[string]string{"k": "v"}},
		},
		{
			name: "a top-level key ends the metadata block",
			src:  "---\nmetadata:\n  k: v\nname: a\n---\n",
			want: skillmd.Doc{Name: "a", Metadata: map[string]string{"k": "v"}},
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

// TestParseRejectsEdges pins the rejection paths the main table leaves out: the bare
// and never-closed openers, an inline metadata value, the metadata-block errors, a
// block scalar that dedents below its own first line, and the single- and
// double-quote failures.
func TestParseRejectsEdges(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, src string }{
		{"a bare opener with no newline", "---"},
		{"never closed and with no final newline", "---\nname: a\ndescription: d"},
		{"metadata with an inline scalar value", "---\nname: a\nmetadata: nope\n---\n"},
		{"a metadata line without a colon", "---\nmetadata:\n  noColon\n---\n"},
		{"a metadata value with an unterminated quote", "---\nmetadata:\n  k: \"oops\n---\n"},
		{"a block scalar that dedents below its first line", "---\ndescription: |\n    first\n  second\n---\n"},
		{"an unterminated single-quoted scalar", "---\nname: 'abc\n---\n"},
		{"a stray quote in a single-quoted scalar", "---\nname: 'a'b'\n---\n"},
		{"a trailing escape in a double-quoted scalar", "---\nname: \"a\\\"\n---\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := skillmd.Parse([]byte(tc.src)); err == nil {
				t.Fatalf("Parse(%q) = nil error, want a failure", tc.src)
			}
		})
	}
}

// TestValidateChecksName confirms Validate runs the name rule, not only the length and
// key rules that follow it: a document whose name is invalid never reaches those.
func TestValidateChecksName(t *testing.T) {
	t.Parallel()

	err := skillmd.Validate(skillmd.Doc{Name: "Bad Name", Description: "d"}, "")
	if !errors.Is(err, skillmd.ErrInvalid) {
		t.Fatalf("Validate with an invalid name = %v, want ErrInvalid", err)
	}
}

// TestFormatRendersInteriorBlankLine covers the writer's block path for a value that
// carries a blank line: it must render as a literal block and read back unchanged,
// not lose the paragraph break.
func TestFormatRendersInteriorBlankLine(t *testing.T) {
	t.Parallel()

	doc := skillmd.Doc{Name: "a", Description: "first\n\nsecond"}
	out, err := skillmd.Format(doc)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	got, err := skillmd.Parse(out)
	if err != nil {
		t.Fatalf("Parse of rendered doc: %v\nrendered:\n%s", err, out)
	}
	if got.Description != doc.Description {
		t.Errorf("interior blank line lost: got %q, want %q\nrendered:\n%s",
			got.Description, doc.Description, out)
	}
}
