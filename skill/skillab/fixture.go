package skillab

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A trial starts in an empty directory, which is fine for an exercise that says
// "write me a parser" and useless for one that says "the tests fail after my
// change". Work that begins from an existing state has to be given that state, or
// the exercise measures whether the model can invent a plausible bug to fix.
//
// A fixture is that state: a directory tree copied into the fresh working directory
// before the run starts. It lives beside the exercise set rather than in the pack,
// for the reason the whole set does. A defect the agent can read the answer to is
// not a defect.
const (
	// FixturesDir holds one subdirectory per fixture, under a skill's exercise set.
	FixturesDir = "fixtures"

	// maxFixtureFiles bounds what one exercise can copy. Well above any real fixture
	// and low enough that a directory pointed at the wrong place fails fast.
	maxFixtureFiles = 2048
)

// ValidateFixture refuses a fixture name that could address anything other than one
// directory under FixturesDir. Names come from a file an author edits, so this is
// the check that keeps a stray "../" from writing outside the working directory.
func ValidateFixture(name string) error {
	switch {
	case name == "":
		return errors.New("skillab: empty fixture name")
	case name == "." || name == "..":
		return fmt.Errorf("skillab: %q is not a fixture name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("skillab: fixture %q holds a path separator; a fixture is one directory under %s", name, FixturesDir)
	case filepath.IsAbs(name):
		return fmt.Errorf("skillab: fixture %q is an absolute path", name)
	}
	return nil
}

// CopyFixture writes the fixture named by an exercise into dest, which is the trial's
// working directory. dir is the skill's exercise set directory within fsys.
//
// Only regular files and directories are copied. A symbolic link is refused rather
// than followed: fs.FS closes lexical traversal, and os.DirFS follows a link once it
// opens one, so a fixture holding a link to /etc would otherwise copy it into the
// working directory of every trial.
func CopyFixture(fsys fs.FS, dir, name, dest string) error {
	if err := ValidateFixture(name); err != nil {
		return err
	}
	root := path(path(dir, FixturesDir), name)
	if _, err := fs.Stat(fsys, root); err != nil {
		return fmt.Errorf("skillab: fixture %s: %w", name, err)
	}
	files := 0
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		target := dest
		if rel != "" {
			target = filepath.Join(dest, filepath.FromSlash(rel))
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o750)
		case !d.Type().IsRegular():
			return fmt.Errorf("skillab: fixture %s: %s is not a regular file", name, path(".", rel))
		}
		files++
		if files > maxFixtureFiles {
			return fmt.Errorf("skillab: fixture %s holds more than %d files", name, maxFixtureFiles)
		}
		return copyFile(fsys, p, target)
	})
	if err != nil {
		return err
	}
	if files == 0 {
		// An empty fixture is the same as no fixture, and it is always a mistake: the
		// author meant to seed a state and seeded nothing, which the run cannot notice.
		return fmt.Errorf("skillab: fixture %s is empty", name)
	}
	return nil
}

// copyFile writes one fixture file, preserving whether it is executable. A fixture
// carrying a reproduction script is useless if the run cannot run it.
func copyFile(fsys fs.FS, src, dest string) (err error) {
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
	// #nosec G304 -- dest is the trial's own temporary directory joined with a path
	// from inside a fixture whose name has passed ValidateFixture, and every entry
	// walked is a regular file or a directory.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// fixturePath is the set-relative path of a fixture directory, for the error a load
// raises when an exercise names one that is not there.
func fixturePath(name string) string { return path(FixturesDir, name) }
