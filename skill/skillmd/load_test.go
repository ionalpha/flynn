package skillmd_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/ionalpha/flynn/skill/skillmd"
)

// doc renders a minimal valid SKILL.md for the named skill.
func doc(name string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: What it does.\n---\nBody.\n")}
}

func file(content string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(content)} }

func TestLoad(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"debugging/SKILL.md":             doc("debugging"),
		"debugging/scripts/bisect.sh":    file("#!/bin/sh\n"),
		"debugging/scripts/trace.sh":     file("#!/bin/sh\n"),
		"debugging/references/notes.md":  file("notes\n"),
		"debugging/assets/diagram.svg":   file("<svg/>\n"),
		"debugging/LICENSE":              file("Apache-2.0\n"),
		"debugging/examples/sample.txt":  file("sample\n"),
		"packs/testing/SKILL.md":         doc("testing"),
		"packs/testing/scripts/gen.sh":   file("#!/bin/sh\n"),
		"packs/planning/SKILL.md":        doc("planning"),
		"packs/notes.txt":                file("not a skill\n"),
		"nested/deep/SKILL.md":           doc("deep"),
		"nested/deep/scripts/inner/x.sh": file("#!/bin/sh\n"),
	}

	t.Run("resources are addressed and extras reported", func(t *testing.T) {
		t.Parallel()

		pack, err := skillmd.Load(fsys, "debugging")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if pack.Doc.Name != "debugging" || pack.Dir != "debugging" {
			t.Errorf("Load: name %q dir %q, want both debugging", pack.Doc.Name, pack.Dir)
		}
		wantResources := []string{"assets/diagram.svg", "references/notes.md", "scripts/bisect.sh", "scripts/trace.sh"}
		if diff := cmp.Diff(wantResources, pack.Resources); diff != "" {
			t.Errorf("Load resources (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"LICENSE", "examples"}, pack.Ignored); diff != "" {
			t.Errorf("Load ignored (-want +got):\n%s", diff)
		}
	})

	t.Run("a resource directory is not walked", func(t *testing.T) {
		t.Parallel()

		_, err := skillmd.Load(fsys, "nested/deep")
		if !errors.Is(err, skillmd.ErrLayout) {
			t.Fatalf("Load a nested resource dir: err = %v, want ErrLayout", err)
		}
	})

	t.Run("LoadAll reads every skill directory under a root", func(t *testing.T) {
		t.Parallel()

		packs, err := skillmd.LoadAll(fsys, "packs")
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		var names []string
		for _, p := range packs {
			names = append(names, p.Doc.Name)
		}
		if diff := cmp.Diff([]string{"planning", "testing"}, names); diff != "" {
			t.Errorf("LoadAll names (-want +got):\n%s", diff)
		}
	})
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
		dir  string
		want error
	}{
		{
			name: "no SKILL.md",
			fsys: fstest.MapFS{"empty/scripts/x.sh": file("#!/bin/sh\n")},
			dir:  "empty",
			want: skillmd.ErrNoSkillMD,
		},
		{
			name: "name does not match the directory",
			fsys: fstest.MapFS{"debugging/SKILL.md": doc("something-else")},
			dir:  "debugging",
			want: skillmd.ErrInvalid,
		},
		{
			name: "not a SKILL.md at all",
			fsys: fstest.MapFS{"debugging/SKILL.md": file("just markdown\n")},
			dir:  "debugging",
			want: skillmd.ErrNoFrontmatter,
		},
		{
			name: "missing the required description",
			fsys: fstest.MapFS{"debugging/SKILL.md": file("---\nname: debugging\n---\nBody.\n")},
			dir:  "debugging",
			want: skillmd.ErrInvalid,
		},
		{
			name: "a SKILL.md above the size cap",
			fsys: fstest.MapFS{"debugging/SKILL.md": file("---\nname: debugging\ndescription: " + strings.Repeat("x", skillmd.MaxDocSize) + "\n---\n")},
			dir:  "debugging",
			want: skillmd.ErrTooLarge,
		},
		{
			name: "an escaping path",
			fsys: fstest.MapFS{"debugging/SKILL.md": doc("debugging")},
			dir:  "../debugging",
			want: skillmd.ErrLayout,
		},
		{
			name: "an absolute path",
			fsys: fstest.MapFS{"debugging/SKILL.md": doc("debugging")},
			dir:  "/debugging",
			want: skillmd.ErrLayout,
		},
		{
			name: "the fs root, which has no name to match against",
			fsys: fstest.MapFS{"SKILL.md": doc("root")},
			dir:  ".",
			want: skillmd.ErrLayout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := skillmd.Load(tt.fsys, tt.dir)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Load: err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestLoadAllRejectsADirectoryThatIsNotASkill(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"packs/testing/SKILL.md": doc("testing"),
		"packs/leftovers/notes":  file("notes\n"),
	}
	_, err := skillmd.LoadAll(fsys, "packs")
	if !errors.Is(err, skillmd.ErrNoSkillMD) {
		t.Fatalf("LoadAll over a directory with no SKILL.md: err = %v, want ErrNoSkillMD", err)
	}
	if _, err := skillmd.LoadAll(fsys, "absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadAll over a missing root: err = %v, want fs.ErrNotExist", err)
	}
}

// irregularFS wraps an fs.FS and reports one path as a symlink, which fstest.MapFS
// cannot express. It is the case the regular-file rule exists for: fs.FS closes
// lexical traversal, but os.DirFS follows a symlink it opens, so a resource that is
// a link is a resource pointing anywhere on the machine.
type irregularFS struct {
	fs.FS
	link string
	mode fs.FileMode
}

func (f irregularFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return nil, err
	}
	for i, entry := range entries {
		if name+"/"+entry.Name() == f.link {
			entries[i] = irregularEntry{DirEntry: entry, mode: f.mode}
		}
	}
	return entries, nil
}

type irregularEntry struct {
	fs.DirEntry
	mode fs.FileMode
}

func (e irregularEntry) Type() fs.FileMode { return e.mode }
func (e irregularEntry) IsDir() bool       { return e.mode.IsDir() }

func TestLoadRefusesAnIrregularFile(t *testing.T) {
	t.Parallel()

	base := fstest.MapFS{
		"debugging/SKILL.md":          doc("debugging"),
		"debugging/scripts/bisect.sh": file("#!/bin/sh\n"),
	}
	for _, tt := range []struct {
		name string
		link string
		mode fs.FileMode
	}{
		{name: "a linked resource", link: "debugging/scripts/bisect.sh", mode: fs.ModeSymlink},
		{name: "a linked SKILL.md", link: "debugging/SKILL.md", mode: fs.ModeSymlink},
		{name: "a device in scripts", link: "debugging/scripts/bisect.sh", mode: fs.ModeDevice},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := skillmd.Load(irregularFS{FS: base, link: tt.link, mode: tt.mode}, "debugging")
			if !errors.Is(err, skillmd.ErrLayout) {
				t.Fatalf("Load: err = %v, want ErrLayout", err)
			}
		})
	}
}

// errFS injects an I/O failure at one path, the case a MapFS cannot produce: a
// directory that lists a file and then fails to hand it over. A loader that swallows
// those reports a skill with no resources, or no skill at all, which is worse than
// reporting the disk.
type errFS struct {
	fs.FS
	failReadDir string
	failRead    string
}

var errInjected = errors.New("injected I/O failure")

func (f errFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.failReadDir {
		return nil, errInjected
	}
	return fs.ReadDir(f.FS, name)
}

func (f errFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil || name != f.failRead {
		return file, err
	}
	return unreadableFile{File: file}, nil
}

type unreadableFile struct{ fs.File }

func (unreadableFile) Read([]byte) (int, error) { return 0, errInjected }

func TestLoadReportsAnIOFailure(t *testing.T) {
	t.Parallel()

	base := fstest.MapFS{
		"debugging/SKILL.md":          doc("debugging"),
		"debugging/scripts/bisect.sh": file("#!/bin/sh\n"),
	}
	for _, tt := range []struct {
		name string
		fsys fs.FS
	}{
		{name: "listing the skill directory", fsys: errFS{FS: base, failReadDir: "debugging"}},
		{name: "listing a resource directory", fsys: errFS{FS: base, failReadDir: "debugging/scripts"}},
		{name: "reading the SKILL.md", fsys: errFS{FS: base, failRead: "debugging/SKILL.md"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := skillmd.Load(tt.fsys, "debugging"); !errors.Is(err, errInjected) {
				t.Fatalf("Load: err = %v, want the injected failure", err)
			}
		})
	}
}

func TestLoadRefusesTooManyResources(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"debugging/SKILL.md": doc("debugging")}
	for i := 0; i <= skillmd.MaxResources; i++ {
		fsys["debugging/scripts/"+string(rune('a'+i%26))+strings.Repeat("x", i/26+1)+".sh"] = file("#!/bin/sh\n")
	}
	if _, err := skillmd.Load(fsys, "debugging"); !errors.Is(err, skillmd.ErrLayout) {
		t.Fatalf("Load past the resource cap: err = %v, want ErrLayout", err)
	}
}
