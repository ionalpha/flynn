package skillab

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/closeerr"
)

const (
	// FixturesDir holds one directory tree per fixture, the starting state copied
	// into a trial so an exercise like "the tests fail" has code to fix. It sits
	// beside the exercise set and outside the pack, so the agent cannot read it.
	FixturesDir = "fixtures"

	// maxFixtureFiles is far above a real fixture, so a wrong directory fails fast.
	maxFixtureFiles = 2048
)

// ValidateFixture refuses a name that is not one directory under [FixturesDir], so a
// stray "../" cannot write outside the working directory.
func ValidateFixture(name string) error {
	switch {
	case name == "":
		return errors.New("skillab: empty fixture name")
	case name == "." || name == "..":
		return fmt.Errorf("skillab: %q is not a fixture name", name)
	case strings.ContainsAny(name, `/\`):
		// Both separators on every host; this also refuses absolute paths.
		return fmt.Errorf("skillab: fixture %q holds a path separator; a fixture is one directory under %s", name, FixturesDir)
	}
	return nil
}

// CopyFixture copies fixture name from the exercise set at dir in fsys into dest.
// Symlinks are refused, because os.DirFS follows them and a link to /etc would be
// copied into every trial.
func CopyFixture(fsys fs.FS, dir, name, dest string) error {
	if err := ValidateFixture(name); err != nil {
		return err
	}
	src := path(path(dir, FixturesDir), name)
	if _, err := fs.Stat(fsys, src); err != nil {
		return fmt.Errorf("skillab: fixture %s: %w", name, err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	files, err := copyTree(fsys, src, root, ".", "fixture "+name, false)
	if err != nil {
		return err
	}
	if files == 0 {
		// An empty fixture is always an authoring mistake the run cannot notice.
		return fmt.Errorf("skillab: fixture %s is empty", name)
	}
	return nil
}

// copyTree copies the regular files under src in fsys to dst inside root and returns
// how many it wrote. Every write goes through root, which refuses a path that leaves
// it. With replace, each file displaces whatever stands at its path: after an agent
// has run in the directory, a symlink it left where a harness file belongs is removed
// and not written through. Without it an occupied path is an error.
func copyTree(fsys fs.FS, src string, root *os.Root, dst, label string, replace bool) (int, error) {
	files := 0
	err := fs.WalkDir(fsys, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		target := dst
		if rel != "" {
			target = filepath.Join(dst, filepath.FromSlash(rel))
		}
		switch {
		case d.IsDir():
			return root.MkdirAll(target, 0o750)
		case !d.Type().IsRegular():
			return fmt.Errorf("skillab: %s: %s is not a regular file", label, path(".", rel))
		}
		files++
		if files > maxFixtureFiles {
			return fmt.Errorf("skillab: %s holds more than %d files", label, maxFixtureFiles)
		}
		return copyFile(fsys, p, root, target, replace)
	})
	return files, err
}

// copyFile keeps the executable bit, so a reproduction script still runs.
func copyFile(fsys fs.FS, src string, root *os.Root, target string, replace bool) (err error) {
	in, err := fsys.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o600)
	if info.Mode()&0o111 != 0 {
		mode = 0o700
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if replace {
		if err := root.RemoveAll(target); err != nil {
			return err
		}
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	out, err := root.OpenFile(target, flags, mode)
	if err != nil {
		return err
	}
	defer closeerr.Keep(&err, out)
	_, err = io.Copy(out, in)
	return err
}

func fixturePath(name string) string { return path(FixturesDir, name) }
