package skillmd_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillmd"
)

func TestWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	doc := skillmd.Doc{
		Name: "debugging", Description: "How to debug.", Body: "Body.\n",
		Metadata: map[string]string{skillmd.MetaCheck: "go test ./..."},
	}
	dir, err := skillmd.Write(root, doc)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := filepath.Join(root, "debugging"); dir != want {
		t.Errorf("Write returned %q, want %q", dir, want)
	}

	// What was written is what Load reads back, which is the contract that matters:
	// the writer's output is the reader's input.
	pack, err := skillmd.Load(os.DirFS(root), "debugging")
	if err != nil {
		t.Fatalf("Load what Write wrote: %v", err)
	}
	if diff := cmp.Diff(doc, pack.Doc, docOpts...); diff != "" {
		t.Errorf("document through the filesystem (-want +got):\n%s", diff)
	}
}

func TestWriteReplacesOnlyItsOwnFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	doc := skillmd.Doc{Name: "debugging", Description: "First.", Body: "One.\n"}
	dir, err := skillmd.Write(root, doc)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	keep := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(keep, []byte("hand written\n"), 0o600); err != nil {
		t.Fatalf("write a sibling: %v", err)
	}

	doc.Description = "Second."
	if _, err := skillmd.Write(root, doc); err != nil {
		t.Fatalf("Write again: %v", err)
	}
	pack, err := skillmd.Load(os.DirFS(root), "debugging")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if pack.Doc.Description != "Second." {
		t.Errorf("description = %q, want the rewritten one", pack.Doc.Description)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file we did not write was removed: %v", err)
	}
}

func TestWriteRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  skillmd.Doc
	}{
		{name: "no description", doc: skillmd.Doc{Name: "debugging"}},
		{name: "a name the format does not allow", doc: skillmd.Doc{Name: "Debugging", Description: "d"}},
		{name: "a name that would escape the root", doc: skillmd.Doc{Name: "../elsewhere", Description: "d"}},
		{name: "undefined frontmatter keys", doc: skillmd.Doc{Name: "debugging", Description: "d", Unknown: map[string]string{"version": "2"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if _, err := skillmd.Write(root, tt.doc); !errors.Is(err, skillmd.ErrInvalid) {
				t.Fatalf("Write: err = %v, want ErrInvalid", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("read the root: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused document left %d entries behind, want none", len(entries))
			}
		})
	}
}

// TestWriteReportsAnIOFailure blocks each write with something already occupying the
// path it needs. A writer that swallows these reports a successful export of a skill
// that is not on disk, which is the one outcome worse than failing.
func TestWriteReportsAnIOFailure(t *testing.T) {
	t.Parallel()

	valid := skillmd.Doc{Name: "debugging", Description: "How to debug."}

	t.Run("the skill directory cannot be created", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "debugging"), []byte("in the way\n"), 0o600); err != nil {
			t.Fatalf("occupy the path: %v", err)
		}
		if _, err := skillmd.Write(root, valid); err == nil {
			t.Fatal("Write over a file where the directory goes: err = nil, want a failure")
		}
	})

	t.Run("the document cannot be written", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "debugging", "SKILL.md"), 0o700); err != nil {
			t.Fatalf("occupy the path: %v", err)
		}
		if _, err := skillmd.Write(root, valid); err == nil {
			t.Fatal("Write where SKILL.md is a directory: err = nil, want a failure")
		}
	})

	t.Run("WriteAll names the skill that failed", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "testing"), []byte("in the way\n"), 0o600); err != nil {
			t.Fatalf("occupy the path: %v", err)
		}
		err := skillmd.WriteAll(root, []skillmd.Doc{valid, {Name: "testing", Description: "How to test."}})
		if err == nil {
			t.Fatal("WriteAll: err = nil, want a failure")
		}
		if !strings.Contains(err.Error(), "testing") {
			t.Errorf("WriteAll error %q does not name the skill that failed", err)
		}
	})

	t.Run("a resource directory cannot be created", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "debugging"), 0o700); err != nil {
			t.Fatalf("prepare the skill directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "debugging", "scripts"), []byte("in the way\n"), 0o600); err != nil {
			t.Fatalf("occupy the path: %v", err)
		}
		src := fstest.MapFS{
			"debugging/SKILL.md":          doc("debugging"),
			"debugging/scripts/bisect.sh": file("#!/bin/sh\n"),
		}
		pack, err := skillmd.Load(src, "debugging")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, err := skillmd.CopyPack(root, src, pack); err == nil {
			t.Fatal("CopyPack over a file where scripts/ goes: err = nil, want a failure")
		}
	})
}

func TestWriteAll(t *testing.T) {
	t.Parallel()

	t.Run("writes the set", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		docs := []skillmd.Doc{
			{Name: "debugging", Description: "How to debug."},
			{Name: "testing", Description: "How to test."},
		}
		if err := skillmd.WriteAll(root, docs); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}
		packs, err := skillmd.LoadAll(os.DirFS(root), ".")
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		var names []string
		for _, p := range packs {
			names = append(names, p.Doc.Name)
		}
		if diff := cmp.Diff([]string{"debugging", "testing"}, names); diff != "" {
			t.Errorf("names (-want +got):\n%s", diff)
		}
	})

	t.Run("a rejected set leaves nothing behind", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct {
			name string
			docs []skillmd.Doc
			want error
		}{
			{
				name: "two skills claiming one directory",
				docs: []skillmd.Doc{
					{Name: "debugging", Description: "First."},
					{Name: "debugging", Description: "Second."},
				},
				want: skillmd.ErrDuplicate,
			},
			{
				name: "one skill the format cannot express",
				docs: []skillmd.Doc{
					{Name: "debugging", Description: "How to debug."},
					{Name: "testing"},
				},
				want: skillmd.ErrInvalid,
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				if err := skillmd.WriteAll(root, tt.docs); !errors.Is(err, tt.want) {
					t.Fatalf("WriteAll: err = %v, want %v", err, tt.want)
				}
				entries, err := os.ReadDir(root)
				if err != nil {
					t.Fatalf("read the root: %v", err)
				}
				if len(entries) != 0 {
					t.Errorf("a refused set left %d entries behind, want none", len(entries))
				}
			})
		}
	})
}

func TestCopyPack(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{
		"debugging/SKILL.md":            doc("debugging"),
		"debugging/scripts/bisect.sh":   file("#!/bin/sh\nbisect\n"),
		"debugging/references/notes.md": file("notes\n"),
		"debugging/README.md":           file("read me\n"),
	}
	pack, err := skillmd.Load(src, "debugging")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	root := t.TempDir()
	if _, err := skillmd.CopyPack(root, src, pack); err != nil {
		t.Fatalf("CopyPack: %v", err)
	}
	got, err := skillmd.Load(os.DirFS(root), "debugging")
	if err != nil {
		t.Fatalf("Load the copy: %v", err)
	}
	// The copy carries the document and every addressed resource. It does not carry
	// Ignored, which is the point: those files were never part of the pack.
	if diff := cmp.Diff(pack.Doc, got.Doc, docOpts...); diff != "" {
		t.Errorf("document through a copy (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(pack.Resources, got.Resources); diff != "" {
		t.Errorf("resources through a copy (-want +got):\n%s", diff)
	}
	if len(got.Ignored) != 0 {
		t.Errorf("the copy holds %v, want only what the layout defines", got.Ignored)
	}
	body, err := os.ReadFile(filepath.Join(root, "debugging", "scripts", "bisect.sh"))
	if err != nil {
		t.Fatalf("read the copied script: %v", err)
	}
	if string(body) != "#!/bin/sh\nbisect\n" {
		t.Errorf("copied script = %q, want the source bytes", body)
	}
	if _, err := os.Stat(filepath.Join(root, "debugging", "README.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an ignored file was copied; CopyPack writes what the layout defines")
	}
}

func TestCopyPackRejects(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{
		"debugging/SKILL.md":          doc("debugging"),
		"debugging/scripts/bisect.sh": file("#!/bin/sh\n"),
		"debugging/scripts/huge.bin":  file(strings.Repeat("x", skillmd.MaxResourceSize+1)),
	}
	valid := skillmd.Doc{Name: "debugging", Description: "What it does."}

	t.Run("a resource above the size cap", func(t *testing.T) {
		t.Parallel()

		pack := skillmd.Pack{Dir: "debugging", Doc: valid, Resources: []string{"scripts/huge.bin"}}
		if _, err := skillmd.CopyPack(t.TempDir(), src, pack); !errors.Is(err, skillmd.ErrTooLarge) {
			t.Fatalf("CopyPack: err = %v, want ErrTooLarge", err)
		}
	})

	t.Run("a hand-built pack addressing a path the layout does not allow", func(t *testing.T) {
		t.Parallel()

		for _, rel := range []string{"../escape.sh", "scripts/nested/x.sh", "elsewhere/x.sh", "scripts/", "bare.sh"} {
			pack := skillmd.Pack{Dir: "debugging", Doc: valid, Resources: []string{rel}}
			if _, err := skillmd.CopyPack(t.TempDir(), src, pack); !errors.Is(err, skillmd.ErrLayout) {
				t.Errorf("CopyPack with resource %q: err = %v, want ErrLayout", rel, err)
			}
		}
	})

	t.Run("a resource that is not there", func(t *testing.T) {
		t.Parallel()

		pack := skillmd.Pack{Dir: "debugging", Doc: valid, Resources: []string{"scripts/absent.sh"}}
		if _, err := skillmd.CopyPack(t.TempDir(), src, pack); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("CopyPack: err = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("a document the format cannot express", func(t *testing.T) {
		t.Parallel()

		pack := skillmd.Pack{Dir: "debugging", Doc: skillmd.Doc{Name: "debugging"}}
		if _, err := skillmd.CopyPack(t.TempDir(), src, pack); !errors.Is(err, skillmd.ErrInvalid) {
			t.Fatalf("CopyPack: err = %v, want ErrInvalid", err)
		}
	})
}

// TestWriteRoundTripProperty asserts that whatever a document carries, writing it and
// reading the directory back returns the same document, and that a second write of
// what was read produces the same bytes. The second half is what makes an export
// reproducible: re-exporting an unchanged skill must not produce a different file.
func TestWriteRoundTripProperty(t *testing.T) {
	// One temporary root for the whole property, and a fresh directory inside it per
	// case, so two draws of the same skill name cannot read each other's files.
	base := t.TempDir()

	rapid.Check(t, func(rt *rapid.T) {
		want := skillmd.Doc{
			Name:          rapid.StringMatching(`[a-z0-9]([a-z0-9]|-[a-z0-9]){0,31}`).Draw(rt, "name"),
			Description:   rapid.StringMatching(`[^\x00-\x1f]{1,80}`).Draw(rt, "description"),
			License:       rapid.StringMatching(`[^\x00-\x1f]{0,20}`).Draw(rt, "license"),
			Compatibility: rapid.StringMatching(`[^\x00-\x1f]{0,40}`).Draw(rt, "compatibility"),
			Body:          rapid.StringMatching(`(?s)[^\x00]{0,200}`).Draw(rt, "body"),
			Metadata: rapid.MapOfN(
				rapid.StringMatching(`[a-z][a-z0-9./-]{0,20}`),
				rapid.StringMatching(`[^\x00-\x1f]{0,40}`), 0, 4,
			).Draw(rt, "metadata"),
		}

		root, err := os.MkdirTemp(base, "case")
		if err != nil {
			rt.Fatalf("temporary root: %v", err)
		}
		dir, err := skillmd.Write(root, want)
		if err != nil {
			rt.Fatalf("Write: %v", err)
		}
		first, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			rt.Fatalf("read what Write wrote: %v", err)
		}

		pack, err := skillmd.Load(os.DirFS(root), want.Name)
		if err != nil {
			rt.Fatalf("Load: %v", err)
		}
		if diff := cmp.Diff(want, pack.Doc, docOpts...); diff != "" {
			rt.Errorf("document through the filesystem (-want +got):\n%s", diff)
		}

		if _, err := skillmd.Write(root, pack.Doc); err != nil {
			rt.Fatalf("Write what Load read: %v", err)
		}
		second, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			rt.Fatalf("read the second write: %v", err)
		}
		if diff := cmp.Diff(string(first), string(second)); diff != "" {
			rt.Errorf("re-exporting an unchanged skill changed the bytes (-first +second):\n%s", diff)
		}
	})
}
