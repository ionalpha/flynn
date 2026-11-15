package skillmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/ionalpha/flynn/internal/fsatomic"
)

// MaxResourceSize caps one sibling file copied by [CopyPack], in bytes. The format
// sets no limit, so without it the source decides how large the destination gets.
const MaxResourceSize = 8 << 20

// ErrDuplicate reports two skills with one name in a write. The name is the
// directory, so one would overwrite the other.
var ErrDuplicate = errors.New("skillmd: duplicate skill name")

// Write renders doc as root/<name>/SKILL.md and returns the directory. doc must pass
// [Validate], which also makes the path safe: a valid name cannot be "..", hold a
// separator or be absolute. It takes a path because the standard library has no
// writable fs.FS. An existing SKILL.md is replaced and nothing else is removed.
func Write(root string, doc Doc) (string, error) {
	if err := Validate(doc, doc.Name); err != nil {
		return "", err
	}
	// Format writes to a strings.Builder and always returns nil.
	src, _ := Format(doc)
	dir := filepath.Join(root, doc.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := fsatomic.WriteFile(filepath.Join(dir, "SKILL.md"), src, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// WriteAll validates every document and checks for duplicate names before writing
// any, so a rejected set writes nothing. After an I/O failure the written files stay,
// because deleting after a failure can take a directory with it; the error names the
// skill.
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

// CopyPack writes p and its resources from src under root, as the inverse of [Load].
// Pack.Ignored is not copied, because the layout does not define those files.
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

// copyResource re-checks rel, because a caller can build a Pack by hand.
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
	return fsatomic.WriteFile(filepath.Join(dir, parent, name), data, 0o600)
}

func readCappedAt(fsys fs.FS, name string, limit int) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	// A read-only close has nothing to report.
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
