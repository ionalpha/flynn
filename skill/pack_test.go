package skill_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

var packScope = state.Scope{Instance: "@bundled"}

func TestFromDoc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  skillmd.Doc
		want state.Skill
	}{
		{
			name: "a document carrying only what the format requires",
			doc:  skillmd.Doc{Name: "debugging", Description: "How to debug.", Body: "Body.\n"},
			// The title falls back to the slug: a skill with no title still needs
			// something to show in a menu.
			want: state.Skill{Slug: "debugging", Name: "debugging", Description: "How to debug.", Body: "Body.\n", Scope: packScope},
		},
		{
			name: "our metadata is decoded",
			doc: skillmd.Doc{
				Name: "debugging", Description: "How to debug.",
				Metadata: map[string]string{
					skillmd.MetaTitle: "Systematic debugging",
					skillmd.MetaCheck: "go test ./...",
					skillmd.MetaTags:  skillmd.EncodeList([]string{"craft", "debug it"}),
				},
			},
			want: state.Skill{
				Slug: "debugging", Name: "Systematic debugging", Description: "How to debug.",
				Check: "go test ./...", Tags: []string{"craft", "debug it"}, Scope: packScope,
			},
		},
		{
			name: "a claim on tools is read and not honoured",
			doc: skillmd.Doc{
				Name: "debugging", Description: "How to debug.",
				AllowedTools: []string{"Bash", "Write"},
				Metadata:     map[string]string{"someone.example/tier": "craft"},
			},
			want: state.Skill{Slug: "debugging", Name: "debugging", Description: "How to debug.", Scope: packScope},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := skill.FromDoc(tt.doc, packScope)
			if err != nil {
				t.Fatalf("FromDoc: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("FromDoc (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFromDocRejectsAMalformedTagList(t *testing.T) {
	t.Parallel()

	doc := skillmd.Doc{
		Name: "debugging", Description: "How to debug.",
		Metadata: map[string]string{skillmd.MetaTags: "craft, debugging"},
	}
	if _, err := skill.FromDoc(doc, packScope); !errors.Is(err, skillmd.ErrInvalid) {
		t.Fatalf("FromDoc with a delimited tag list: err = %v, want ErrInvalid", err)
	}
}

func TestToDocRejectsASlugTheFormatCannotName(t *testing.T) {
	t.Parallel()

	for _, slug := range []string{"", "Debugging", "debug--it", "-debug", "debug_it"} {
		if _, err := skill.ToDoc(state.Skill{Slug: slug, Description: "d"}); !errors.Is(err, skillmd.ErrInvalid) {
			t.Errorf("ToDoc with slug %q: err = %v, want ErrInvalid", slug, err)
		}
	}
}

func TestFromPackReadsTheLoadedDocument(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"debugging/SKILL.md":          {Data: []byte("---\nname: debugging\ndescription: How to debug.\n---\nBody.\n")},
		"debugging/scripts/bisect.sh": {Data: []byte("#!/bin/sh\n")},
	}
	pack, err := skillmd.Load(fsys, "debugging")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sk, err := skill.FromPack(pack, packScope)
	if err != nil {
		t.Fatalf("FromPack: %v", err)
	}
	want := state.Skill{Slug: "debugging", Name: "debugging", Description: "How to debug.", Body: "Body.\n", Scope: packScope}
	if diff := cmp.Diff(want, sk); diff != "" {
		t.Errorf("FromPack (-want +got):\n%s", diff)
	}
}

func TestExport(t *testing.T) {
	t.Parallel()

	skills := []state.Skill{
		{Slug: "debugging", Name: "Systematic debugging", Description: "How to debug.", Body: "Body.\n", Tags: []string{"craft"}, Scope: packScope},
		{Slug: "testing", Name: "testing", Description: "How to test.", Body: "Body.\n", Check: "go test ./...", Scope: packScope},
	}
	dir := t.TempDir()
	if err := skill.Export(dir, skills); err != nil {
		t.Fatalf("Export: %v", err)
	}

	packs, err := skillmd.LoadAll(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("LoadAll what Export wrote: %v", err)
	}
	got := make([]state.Skill, 0, len(packs))
	for _, p := range packs {
		sk, err := skill.FromPack(p, packScope)
		if err != nil {
			t.Fatalf("FromPack: %v", err)
		}
		got = append(got, sk)
	}
	if diff := cmp.Diff(skills, got); diff != "" {
		t.Errorf("skills through an export (-want +got):\n%s", diff)
	}
}

func TestExportRefusesASkillTheFormatCannotCarry(t *testing.T) {
	t.Parallel()

	// Two ways a stored skill fails to be expressible. A skill distilled before
	// descriptions existed has none, and the format requires one; a slug minted as a
	// database key can be illegal as a skill name. Either stops the export with the
	// skill named, before anything is written.
	for _, tt := range []struct {
		name   string
		skills []state.Skill
	}{
		{
			name: "no description",
			skills: []state.Skill{
				{Slug: "debugging", Description: "How to debug.", Body: "Body.\n"},
				{Slug: "testing", Body: "Body.\n"},
			},
		},
		{
			name: "a slug the format cannot name",
			skills: []state.Skill{
				{Slug: "debugging", Description: "How to debug.", Body: "Body.\n"},
				{Slug: "testing_2026", Description: "How to test.", Body: "Body.\n"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			err := skill.Export(dir, tt.skills)
			if !errors.Is(err, skillmd.ErrInvalid) {
				t.Fatalf("Export: err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "testing") {
				t.Errorf("Export error %q does not name the skill that failed", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read the export directory: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused export left %d entries behind, want none", len(entries))
			}
		})
	}
}

// TestDocRoundTripProperty asserts that whatever content a skill carries, writing it
// as a SKILL.md and reading it back returns the same skill, and that what comes out
// is a document a conformant reader accepts. The name-matches-directory rule is
// checked against the slug, which is the directory a writer would use.
func TestDocRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		want := state.Skill{
			Slug:        rapid.StringMatching(`[a-z0-9]([a-z0-9]|-[a-z0-9]){0,31}`).Draw(rt, "slug"),
			Name:        rapid.StringMatching(`[^\x00-\x1f]{0,40}`).Draw(rt, "title"),
			Description: rapid.StringMatching(`[^\x00-\x1f]{1,80}`).Draw(rt, "description"),
			Body:        rapid.StringMatching(`(?s)[^\x00]{0,200}`).Draw(rt, "body"),
			Tags:        rapid.SliceOfN(rapid.StringMatching(`[^\x00-\x1f]{0,20}`), 0, 4).Draw(rt, "tags"),
			Check:       rapid.StringMatching(`[^\x00-\x1f]{0,40}`).Draw(rt, "check"),
			Scope:       packScope,
		}
		if want.Name == "" {
			// An empty title is not carried, and reading gives back the slug. The
			// round trip is over what a stored skill actually holds.
			want.Name = want.Slug
		}

		doc, err := skill.ToDoc(want)
		if err != nil {
			rt.Fatalf("ToDoc: %v", err)
		}
		if err := skillmd.Validate(doc, want.Slug); err != nil {
			rt.Fatalf("Validate the document we wrote: %v", err)
		}
		src, err := skillmd.Format(doc)
		if err != nil {
			rt.Fatalf("Format: %v", err)
		}
		parsed, err := skillmd.Parse(src)
		if err != nil {
			rt.Fatalf("Parse what Format wrote: %v", err)
		}
		got, err := skill.FromDoc(parsed, packScope)
		if err != nil {
			rt.Fatalf("FromDoc: %v", err)
		}
		if len(want.Tags) == 0 {
			// No tags key is written, so nothing distinguishes an empty list from
			// none once it has been through a file.
			want.Tags = nil
			got.Tags = nil
		}
		if diff := cmp.Diff(want, got); diff != "" {
			rt.Errorf("skill through a SKILL.md (-want +got):\n%s", diff)
		}
	})
}
