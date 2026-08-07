package skillmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// The writing half of the directory layer.
//
// Write takes a real directory path rather than an fs.FS, because the standard
// library has no writable filesystem interface and inventing one here would buy
// nothing: everything that exports writes to a directory a person named.
//
// The safety property that makes a plain path acceptable is that a skill's name is
// validated before it is joined to anything. The specification's name rule allows
// lowercase letters, digits and hyphens, so a validated name cannot be "..", cannot
// hold a separator, and cannot be absolute. Every path this file builds is root
// joined with a name that has passed ValidateName, so the write stays under root by
// construction rather than by a traversal check applied afterwards.

// MaxResourceSize caps a single sibling file copied by CopyPack. As with MaxDocSize,
// the format sets no limit, and copying from a tree we did not author means the
// alternative to a cap is letting the source decide how large the destination gets.
const MaxResourceSize = 8 << 20

// ErrDuplicate reports two skills claiming the same name in one write. The name is
// the directory, so writing both would silently leave one of them.
var ErrDuplicate = errors.New("skillmd: duplicate skill name")

// Write renders doc as a conformant skill directory under root and returns the path
// it wrote. The document must satisfy Validate: this is the publishing gate, and
// emitting a document a conformant reader would reject is the failure this package
// exists to prevent.
//
// An existing SKILL.md at the destination is replaced, because it is the file we are
// writing. Nothing else in the directory is touched or removed: a caller exporting
// over an earlier export gets a current SKILL.md, not a directory silently pruned of
// files it did not ask us to manage.
func Write(root string, doc Doc) (string, error) {
	if err := Validate(doc, doc.Name); err != nil {
		return "", err
	}
	// Format's error is discarded for the same reason EncodeList discards
	// json.Marshal's: it cannot occur. Format writes into a strings.Builder, whose
	// writes never fail, and it returns nil unconditionally. Branching on it here
	// would put an untestable path in the middle of the export.
	src, _ := Format(doc)
	dir := filepath.Join(root, doc.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), src, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// WriteAll writes every document under root, as a set. Each is validated and the
// names are checked for collisions before anything is written, so a rejected set
// leaves the destination as it found it rather than half-exported.
//
// An I/O failure partway through is a different matter: what is on disk stays there,
// because deleting files after a failed write is how an export takes a directory
// with it. The error names the skill that failed.
func WriteAll(root string, docs []Doc) error {
	seen := make(map[string]struct{}, len(docs))
	for _, doc := range docs {
		if err := Validate(doc, doc.Name); err != nil {
			return err
		}
		if _, dup := seen[doc.Name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicate, doc.Name)
		}
		seen[doc.Name] = struct{}{}
	}
	for _, doc := range docs {
		if _, err := Write(root, doc); err != nil {
			return fmt.Errorf("write %q: %w", doc.Name, err)
		}
	}
	return nil
}

// CopyPack writes a loaded pack under root: its document, plus the sibling files it
// addresses, copied from the filesystem it was loaded from. It is Load's inverse, so
// a pack that goes through both comes out the same pack.
//
// Only the resources Load recorded are copied. Entries in Pack.Ignored are left
// behind deliberately: they are files whose meaning the layout does not define, and
// copying a directory we could not interpret would make this a tree-copier rather
// than a skill writer.
func CopyPack(root string, src fs.FS, p Pack) (string, error) {
	dir, err := Write(root, p.Doc)
	if err != nil {
		return "", err
	}
	for _, rel := range p.Resources {
		if err := copyResource(dir, src, p.Dir, rel); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// copyResource copies one addressed sibling. The relative path came from Load, which
// built it from a directory listing of one of the three resource directories, so it
// is one known segment plus one filename; it is re-checked here anyway, because a
// Pack is a struct a caller can also fill in by hand.
func copyResource(dir string, src fs.FS, packDir, rel string) error {
	parent, name := path.Split(rel)
	parent = path.Clean(parent)
	if !isResourceDir(parent) || name == "" || !fs.ValidPath(rel) {
		return fmt.Errorf("%w: %q is not an addressable resource path", ErrLayout, rel)
	}
	data, err := readCappedAt(src, path.Join(packDir, rel), MaxResourceSize)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, parent), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, parent, name), data, 0o600)
}

// readCappedAt reads a file, refusing one above limit without reading it whole.
func readCappedAt(fsys fs.FS, name string, limit int) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	// The close error is dropped deliberately: this reader never wrote anything, so
	// there is no buffered data whose loss a close could report.
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("skillmd: read %s: %w", name, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, limit)
	}
	return data, nil
}
