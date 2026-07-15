package catalog

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
)

// fakeCatalogFS is a hand-built fs.FS that feeds parseCatalog exactly the directory
// listing and file bytes a test wants, including the failures a valid embedded catalog
// can never produce (an unreadable directory, an unreadable file, two specs that reserve
// the same name). Only ReadDir and ReadFile are exercised, so Open is a stub.
type fakeCatalogFS struct {
	entries []fs.DirEntry
	dirErr  error
	files   map[string][]byte
	fileErr map[string]error
}

func (fakeCatalogFS) Open(string) (fs.File, error) { return nil, errors.New("unused") }

func (f fakeCatalogFS) ReadDir(string) ([]fs.DirEntry, error) {
	if f.dirErr != nil {
		return nil, f.dirErr
	}
	return f.entries, nil
}

func (f fakeCatalogFS) ReadFile(name string) ([]byte, error) {
	base := name[strings.LastIndex(name, "/")+1:]
	if err := f.fileErr[base]; err != nil {
		return nil, err
	}
	if b, ok := f.files[base]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}

type fakeDirEntry struct {
	name string
	dir  bool
}

func (e fakeDirEntry) Name() string { return e.name }
func (e fakeDirEntry) IsDir() bool  { return e.dir }
func (e fakeDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (fakeDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("unused") }

// devSpecJSON is a spec whose process surface names an unsigned local path, the one thing
// a bundled extension may never carry.
func devSpecJSON(t *testing.T) []byte {
	t.Helper()
	block, err := json.Marshal(extension.ProcessBlock{Dev: &extension.DevSource{Path: "/tmp/x"}})
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	spec := extension.Spec{Surfaces: map[string]json.RawMessage{extension.SurfaceProcess: block}}
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return out
}

// TestParseCatalogErrorPaths drives every failure parseCatalog can report. The embedded
// catalog is always valid, so these paths are reachable only through a crafted fs, which
// is exactly why parseCatalog takes one.
func TestParseCatalogErrorPaths(t *testing.T) {
	cases := []struct {
		name string
		fsys fs.FS
		want string
	}{
		{
			name: "unreadable specs directory",
			fsys: fakeCatalogFS{dirErr: errors.New("boom")},
			want: "read embedded specs",
		},
		{
			name: "unreadable spec file",
			fsys: fakeCatalogFS{
				entries: []fs.DirEntry{fakeDirEntry{name: "token.json"}},
				fileErr: map[string]error{"token.json": errors.New("boom")},
			},
			want: "read token.json",
		},
		{
			name: "spec that does not decode",
			fsys: fakeCatalogFS{
				entries: []fs.DirEntry{fakeDirEntry{name: "token.json"}},
				files:   map[string][]byte{"token.json": []byte("not json")},
			},
			want: "decode token.json",
		},
		{
			name: "two specs reserving the same name",
			fsys: fakeCatalogFS{
				entries: []fs.DirEntry{
					fakeDirEntry{name: "token.json"},
					fakeDirEntry{name: "token.json"},
				},
				files: map[string][]byte{"token.json": []byte("{}")},
			},
			want: "duplicate official extension name",
		},
		{
			name: "bundled spec carrying a dev source",
			fsys: fakeCatalogFS{
				entries: []fs.DirEntry{fakeDirEntry{name: "token.json"}},
				files:   map[string][]byte{"token.json": devSpecJSON(t)},
			},
			want: "declares a dev source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseCatalog(tc.fsys)
			if err == nil {
				t.Fatal("parseCatalog accepted a catalog it should have rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestParseCatalogSkipsNonSpecsAndOrders proves the reader ignores anything that is not a
// .json file (a subdirectory, a stray README) and returns the remaining entries ordered by
// name so a sync is deterministic regardless of directory order.
func TestParseCatalogSkipsNonSpecsAndOrders(t *testing.T) {
	fsys := fakeCatalogFS{
		entries: []fs.DirEntry{
			fakeDirEntry{name: "nested", dir: true},
			fakeDirEntry{name: "README.md"},
			fakeDirEntry{name: "zeta.json"},
			fakeDirEntry{name: "alpha.json"},
		},
		files: map[string][]byte{
			"zeta.json":  []byte("{}"),
			"alpha.json": []byte("{}"),
		},
	}
	entries, reserved, err := parseCatalog(fsys)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the non-.json items must be skipped)", len(entries))
	}
	if entries[0].Name != "alpha" || entries[1].Name != "zeta" {
		t.Fatalf("entries not ordered by name: %q, %q", entries[0].Name, entries[1].Name)
	}
	if !reserved["alpha"] || !reserved["zeta"] {
		t.Fatalf("reserved set missing a name: %v", reserved)
	}
}
