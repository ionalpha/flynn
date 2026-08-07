package skillmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
)

// The directory layer, over an fs.FS rather than the filesystem.
//
// A skill is a directory, not a file: SKILL.md at its root plus optional scripts/,
// references/ and assets/ siblings. Taking fs.FS rather than a path means the same
// loader reads the pack embedded in the binary (embed.FS), a pack on disk
// (os.DirFS), and a pack in a test (fstest.MapFS), and it means the loader cannot
// reach outside the tree it was handed: fs.FS rejects "..", absolute paths and
// rooted paths by contract, so traversal is closed by construction rather than by a
// check someone has to remember to write.
//
// Siblings are addressed, never inlined. Their paths are recorded and their bytes
// are left where they are, which is what the format's execution-stage disclosure
// asks for: activation loads the body, and a script is read only if something
// decides to run it. Inlining would put every byte of every resource into the
// activation budget the layout exists to protect.

// MaxResources caps the sibling files one skill may address. The specification sets
// no limit; a loader reading a tree it did not author needs one, because a pack with
// a hundred thousand files in scripts/ otherwise decides how much memory this
// process uses. The cap is far above any plausible authored skill.
const MaxResources = 256

// ResourceDirs are the sibling directories the layout defines. Anything else at the
// skill root is not part of the pack and is reported in Pack.Ignored.
var ResourceDirs = []string{"scripts", "references", "assets"}

var (
	// ErrNoSkillMD reports a directory with no SKILL.md. It is the "this is not a
	// skill" error, distinct from a SKILL.md that breaks a rule.
	ErrNoSkillMD = errors.New("skillmd: no SKILL.md")
	// ErrLayout reports a directory tree the layout does not allow: a nested
	// resource directory, an entry that is not a regular file, or more resources
	// than MaxResources.
	ErrLayout = errors.New("skillmd: unexpected layout")
)

// Pack is one loaded skill directory: the validated document plus the relative
// paths of the siblings it may reach. Resources are paths, not contents.
type Pack struct {
	// Dir is the path Load was given, relative to the fs.FS root.
	Dir string
	// Doc is the parsed and validated SKILL.md. Doc.Name equals path.Base(Dir),
	// because the specification requires it and Load enforces it.
	Doc Doc
	// Resources are the sibling files, as paths relative to Dir ("scripts/build.sh"),
	// sorted. Reading one is the caller's decision, taken at execution time.
	Resources []string
	// Ignored holds root entries that are neither SKILL.md nor a resource directory,
	// as names relative to Dir. A README, a LICENSE the license field points at, or a
	// directory this layout does not define all land here. They are carried rather
	// than refused, because refusing them would reject most packs in the wild, and
	// they are reported rather than dropped, because a silently ignored directory is
	// a pack half-read.
	Ignored []string
}

// Load reads the skill directory dir from fsys. It applies the specification in
// full: the document parses, satisfies Validate, and its name matches the directory
// name. A directory with no SKILL.md is ErrNoSkillMD; one whose layout breaks a rule
// is ErrLayout; one whose document breaks a rule carries the codec's own error.
//
// dir must name a directory: the name-matches-directory rule has nothing to check
// against "." , so the fs root itself is refused rather than loaded under a name
// nobody chose.
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
			// Already read. Checked for regularity here so a SKILL.md that is a
			// symlink is refused rather than followed.
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

// LoadAll reads every skill directory directly under root. It is the packs-root
// reader: each subdirectory claims to be a skill, so one without a SKILL.md is an
// error rather than a directory to skip. Files directly under root are ignored,
// since only a directory can be a skill.
//
// Packs come back in the order fs.ReadDir gives them, which is by filename, so the
// result is deterministic across the embedded, on-disk and in-memory filesystems.
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

// loadResourceDir lists one sibling directory. Its entries stay one level deep and
// are regular files: a nested directory is refused rather than walked, because the
// format addresses resources as one-segment relative paths and a tree we descend is
// a tree whose size we did not agree to.
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

// requireRegular refuses anything that is not a plain file. A symlink is the case
// that matters: fs.FS closes lexical traversal, but os.DirFS follows a symlink when
// it opens one, so a pack holding scripts/x -> /etc/shadow would otherwise be a pack
// that addresses a file outside its own tree. Devices, sockets and named pipes are
// refused by the same rule, since none of them is a skill resource.
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

// readCapped reads a file, refusing one above MaxDocSize without reading it all. The
// limit is applied to the reader rather than checked after the fact, so a hostile
// SKILL.md cannot cost a gigabyte of memory on its way to being rejected.
func readCapped(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	// The close error is dropped deliberately: this reader never wrote anything, so
	// there is no buffered data whose loss a close could report.
	defer func() { _ = f.Close() }()

	src, err := io.ReadAll(io.LimitReader(f, MaxDocSize+1))
	if err != nil {
		return nil, fmt.Errorf("skillmd: read %s: %w", name, err)
	}
	if len(src) > MaxDocSize {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, MaxDocSize)
	}
	return src, nil
}
