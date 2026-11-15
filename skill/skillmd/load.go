package skillmd

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
)

// MaxResources caps the sibling files one skill may address. The specification sets
// no limit, so without one a hostile pack would decide memory use.
const MaxResources = 256

// ResourceDirs are the sibling directories the layout defines. Anything else at the
// skill root is not part of the pack and is reported in Pack.Ignored.
var ResourceDirs = []string{"scripts", "references", "assets"}

var (
	// ErrNoSkillMD reports a directory with no SKILL.md, which is not a skill.
	ErrNoSkillMD = errors.New("skillmd: no SKILL.md")
	// ErrLayout reports a directory tree the layout does not allow: a nested
	// resource directory, an entry that is not a regular file, or more resources
	// than MaxResources.
	ErrLayout = errors.New("skillmd: unexpected layout")
)

// Pack is one loaded skill directory. It is read through an fs.FS, so the same code
// loads embed.FS, os.DirFS and fstest.MapFS, and fs.FS refuses ".." and absolute
// paths, which closes traversal. Resources are recorded as paths and never read, so
// activation loads only the body and a script is read when something runs it.
type Pack struct {
	// Dir is relative to the fs.FS root.
	Dir string
	// Doc is validated, and Doc.Name equals path.Base(Dir).
	Doc Doc
	// Resources are sorted paths relative to Dir, such as "scripts/build.sh".
	Resources []string
	// Ignored holds other root entries, such as a README. Refusing them would
	// reject most packs in the wild, and dropping them silently would hide a
	// half-read pack.
	Ignored []string
}

// Load reads the skill directory dir from fsys and applies the full specification,
// including [Validate] against the directory name. It returns [ErrNoSkillMD],
// [ErrLayout], or the codec's error for a bad document. dir "." is refused because
// the fs root has no name to match.
func Load(fsys fs.FS, dir string) (Pack, error) {
	if !fs.ValidPath(dir) || dir == "." {
		return Pack{}, fmt.Errorf("%w: %q does not name a skill directory", ErrLayout, dir)
	}
	name := path.Base(dir)

	src, err := readCapped(fsys, path.Join(dir, "SKILL.md"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Pack{}, fmt.Errorf("%w: %s", ErrNoSkillMD, dir)
		}
		return Pack{}, err
	}
	doc, err := Parse(src)
	if err != nil {
		return Pack{}, fmt.Errorf("%s: %w", dir, err)
	}
	if err := Validate(doc, name); err != nil {
		return Pack{}, fmt.Errorf("%s: %w", dir, err)
	}

	pack := Pack{Dir: dir, Doc: doc}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Pack{}, err
	}
	for _, entry := range entries {
		switch {
		case entry.Name() == "SKILL.md":
			// Refuses a SKILL.md symlink.
			if err := requireRegular(entry, path.Join(dir, entry.Name())); err != nil {
				return Pack{}, err
			}
		case entry.IsDir() && isResourceDir(entry.Name()):
			found, err := loadResourceDir(fsys, dir, entry.Name())
			if err != nil {
				return Pack{}, err
			}
			pack.Resources = append(pack.Resources, found...)
		default:
			pack.Ignored = append(pack.Ignored, entry.Name())
		}
	}
	if len(pack.Resources) > MaxResources {
		return Pack{}, fmt.Errorf("%w: %s addresses %d resources, at most %d", ErrLayout, dir, len(pack.Resources), MaxResources)
	}
	return pack, nil
}

// LoadAll loads every directory directly under root, in filename order. Every
// subdirectory must be a skill, so one without a SKILL.md is an error. Files under
// root are ignored.
func LoadAll(fsys fs.FS, root string) ([]Pack, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, err
	}
	var packs []Pack
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pack, err := Load(fsys, path.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		packs = append(packs, pack)
	}
	return packs, nil
}

// loadResourceDir refuses a nested directory, because the format addresses
// resources one level deep and walking deeper has no size bound.
func loadResourceDir(fsys fs.FS, dir, name string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, path.Join(dir, name))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		rel := path.Join(name, entry.Name())
		if entry.IsDir() {
			return nil, fmt.Errorf("%w: %s is nested; resources stay one level under %s/", ErrLayout, path.Join(dir, rel), name)
		}
		if err := requireRegular(entry, path.Join(dir, rel)); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}

// requireRegular refuses anything but a plain file. os.DirFS follows symlinks, so
// scripts/x -> /etc/shadow would address a file outside the pack. Devices, sockets
// and pipes are refused too.
func requireRegular(entry fs.DirEntry, where string) error {
	if entry.Type() != 0 {
		return fmt.Errorf("%w: %s is %s, want a regular file", ErrLayout, where, entry.Type())
	}
	return nil
}

func isResourceDir(name string) bool {
	for _, dir := range ResourceDirs {
		if name == dir {
			return true
		}
	}
	return false
}

// readCapped limits the reader to [MaxDocSize], so an oversized document is refused
// before it is read into memory.
func readCapped(fsys fs.FS, name string) ([]byte, error) {
	return readCappedAt(fsys, name, MaxDocSize)
}
