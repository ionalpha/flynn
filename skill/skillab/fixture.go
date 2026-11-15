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
		// An empty fixture is always an authoring mistake the run cannot notice.
		return fmt.Errorf("skillab: fixture %s is empty", name)
	}
	return nil
}

// copyFile keeps the executable bit, so a reproduction script still runs.
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
	defer closeerr.Keep(&err, out)
	_, err = io.Copy(out, in)
	return err
}

func fixturePath(name string) string { return path(FixturesDir, name) }
