package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
)

// OverlayFile is the name, inside the data directory, of the user's own catalog entries.
// [OverlayPath] gives the full path.
const OverlayFile = "catalog.user.json"

// maxOverlayBytes bounds the overlay file read, so a corrupt or hostile file in the data
// directory cannot be used to exhaust memory on every command that loads the catalog.
const maxOverlayBytes = 8 << 20

// Overlay is the on-disk shape of the user's catalog entries.
type Overlay struct {
	// Models are the entries the user added. Whatever Trust a file records, [Merge] holds
	// every one at [TrustLocal].
	Models []ModelSpec `json:"models"`
}

// OverlayPath is where the overlay lives for a data directory.
func OverlayPath(dataDir string) string { return filepath.Join(dataDir, OverlayFile) }

// RefusalError is a user entry the catalog would not take, and why. It is an error so a command
// that adds one can return it as it stands.
type RefusalError struct {
	// ID is the entry's id as the user wrote it.
	ID string
	// Reason is a sentence a user can act on.
	Reason string
}

func (r RefusalError) Error() string { return fmt.Sprintf("catalog: refused %q: %s", r.ID, r.Reason) }

// foldID is the form ids are compared in. Case and surrounding space are ignored, so
// "Qwen3.5:9B " cannot stand beside the blessed "qwen3.5:9b" and be mistaken for it.
func foldID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// Merge adds a user's entries under a catalog and returns the result with the entries it
// refused. The precedence is fixed, highest first: a signed remote list when one is
// present, then the embedded catalog, then the user overlay. base is whatever the higher
// sources produced, and a user entry never replaces anything in it: an id base already
// holds, compared without regard to case or surrounding space, is refused with the id
// named, so a user can neither shadow a blessed entry nor stand in for one.
//
// Every accepted entry is held at [TrustLocal] whatever its file claims, which is how
// callers tell it from a vetted entry. An entry is refused as well when it is not a local
// model, when it fails [Catalog.Validate], or when the same id appears twice in the
// overlay (the first wins).
//
// base is not modified.
func Merge(base Catalog, user []ModelSpec) (Catalog, []RefusalError) {
	taken := make(map[string]bool, len(base.Models)+len(user))
	for _, m := range base.Models {
		taken[foldID(m.ID)] = true
	}
	merged := base
	merged.Models = append(make([]ModelSpec, 0, len(base.Models)+len(user)), base.Models...)

	var refused []RefusalError
	for _, m := range user {
		if reason := entryProblem(m, taken); reason != "" {
			refused = append(refused, RefusalError{ID: m.ID, Reason: reason})
			continue
		}
		m.Trust = TrustLocal
		taken[foldID(m.ID)] = true
		merged.Models = append(merged.Models, m)
	}
	return merged, refused
}

// entryProblem says why a user entry cannot join a catalog whose ids are taken, or returns
// "" when it can.
func entryProblem(m ModelSpec, taken map[string]bool) string {
	switch {
	case strings.TrimSpace(m.ID) == "":
		return "the entry has no id"
	case m.ID != strings.TrimSpace(m.ID) || strings.ContainsAny(m.ID, " \t\r\n"):
		return "an id cannot contain spaces"
	case taken[foldID(m.ID)]:
		return "that id is already in the catalog; a user entry cannot replace or shadow it, so choose another id"
	case m.Kind != KindLocal:
		return "only local models can be added"
	}
	if err := (Catalog{Models: []ModelSpec{m}}).Validate(); err != nil {
		return strings.TrimPrefix(err.Error(), "catalog: ")
	}
	return ""
}

// ReadOverlay returns the user's entries for a data directory. A missing file is no
// entries, not an error. The entries are returned as written; [Merge] is what checks them.
func ReadOverlay(dataDir string) ([]ModelSpec, error) {
	path := OverlayPath(dataDir)
	b, err := readCapped(path, maxOverlayBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "catalog_overlay_read", err)
	}
	var o Overlay
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fault.New(fault.Terminal, "catalog_overlay_decode", fmt.Sprintf("catalog: %s is not valid: %v", path, err))
	}
	return o.Models, nil
}

// readCapped reads a file through one handle and fails if it holds more than limit bytes, so a
// file that grows between a size check and the read cannot get past the cap.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the overlay path under the data directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}

func writeOverlay(dataDir string, models []ModelSpec) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fault.Wrap(fault.Terminal, "catalog_overlay_write", err)
	}
	b, err := json.MarshalIndent(Overlay{Models: models}, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Terminal, "catalog_overlay_write", err)
	}
	if err := fsatomic.WriteFile(OverlayPath(dataDir), append(b, '\n'), 0o600); err != nil {
		return fault.Wrap(fault.Terminal, "catalog_overlay_write", err)
	}
	return nil
}

// LoadWithOverlay is [Load] with the data directory's overlay merged under it, as [Merge]
// describes, and the entries the merge refused. An unreadable overlay returns the embedded
// catalog alone with the error, so a damaged user file never takes the curated list away.
// An empty dataDir has no overlay.
func LoadWithOverlay(dataDir string) (Catalog, []RefusalError, error) {
	base, err := Load()
	if err != nil || dataDir == "" {
		return base, nil, err
	}
	user, err := ReadOverlay(dataDir)
	if err != nil {
		return base, nil, err
	}
	merged, refused := Merge(base, user)
	return merged, refused, nil
}

// CheckUser reports whether the embedded catalog would refuse m as a user entry, with the
// [RefusalError] [AddUser] would return. A command calls it before asking for consent or
// copying weights, so a refused id costs the person nothing.
func CheckUser(m ModelSpec) error {
	base, err := Load()
	if err != nil {
		return err
	}
	if _, refused := Merge(base, []ModelSpec{m}); len(refused) > 0 {
		return refused[0]
	}
	return nil
}

// AddUser records a user entry in the overlay, replacing an earlier overlay entry with the
// same id, and reports whether it replaced one. An id the embedded catalog holds is refused
// as [Merge] would, with a [RefusalError] naming it, and nothing is written. The entry is stored
// at [TrustLocal].
func AddUser(dataDir string, m ModelSpec) (replaced bool, err error) {
	base, err := Load()
	if err != nil {
		return false, err
	}
	user, err := ReadOverlay(dataDir)
	if err != nil {
		return false, err
	}
	m.Trust = TrustLocal
	kept := make([]ModelSpec, 0, len(user)+1)
	for _, u := range user {
		if foldID(u.ID) == foldID(m.ID) {
			replaced = true
			continue
		}
		kept = append(kept, u)
	}
	// The same merge the next load runs decides the entry, so one accepted here is one that
	// will load.
	kept = append(kept, m)
	_, refused := Merge(base, kept)
	for _, r := range refused {
		if foldID(r.ID) == foldID(m.ID) {
			return false, r
		}
	}
	return replaced, writeOverlay(dataDir, kept)
}

// ErrNotUserEntry is returned by [RemoveUser] for an id the overlay does not hold.
var ErrNotUserEntry = errors.New("catalog: no such user entry")

// RemoveUser deletes a user entry from the overlay. An id in the embedded catalog is not a
// user entry and is refused, so a command cannot be used to drop a curated model.
func RemoveUser(dataDir, id string) error {
	user, err := ReadOverlay(dataDir)
	if err != nil {
		return err
	}
	kept := make([]ModelSpec, 0, len(user))
	for _, u := range user {
		if foldID(u.ID) != foldID(id) {
			kept = append(kept, u)
		}
	}
	if len(kept) == len(user) {
		if base, err := Load(); err == nil {
			for _, m := range base.Models {
				if foldID(m.ID) == foldID(id) {
					return RefusalError{ID: id, Reason: "it is a catalog entry, not one you added, so it cannot be removed"}
				}
			}
		}
		return fmt.Errorf("%w: %q", ErrNotUserEntry, id)
	}
	return writeOverlay(dataDir, kept)
}

// Unverified reports whether the entry is anything short of [TrustBlessed], so a listing
// can say that no one has vetted it.
func (m ModelSpec) Unverified() bool { return m.Trust != TrustBlessed }
