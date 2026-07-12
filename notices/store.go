package notices

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
)

// Trust is what a client remembers between runs. It is small on purpose: the highest feed
// version it has ever trusted (which is what makes a rollback detectable across runs, not
// just within one), when it last managed to check, and which notices it has already
// shown.
type Trust struct {
	// Version is the highest feed version ever accepted. It only ever increases.
	Version uint64 `json:"version"`
	// Checked is when a feed was last accepted, used to decide whether to look again.
	Checked time.Time `json:"checked"`
	// Shown holds the ids of notices already displayed, so a release notice appears once
	// instead of on every run. Security notices ignore this: they are shown for as long
	// as they apply, because a user who is still on a vulnerable version still needs to
	// know, however bored they are of hearing it.
	Shown []string `json:"shown"`
}

// hasShown reports whether id has already been displayed.
func (t Trust) hasShown(id string) bool {
	for _, s := range t.Shown {
		if s == id {
			return true
		}
	}
	return false
}

// Store is the client's on-disk notice state under the data directory: the last accepted
// feed document, and the trust state above.
//
// The cached document is kept as the signed bytes, not as a decoded feed. It is therefore
// re-verified from scratch on every run, which means a local attacker who edits the cache
// file has not forged a notice, they have merely destroyed one: the edited document fails
// its signature check and is discarded. Caching decoded text would have handed them a way
// to put words in Flynn's mouth.
type Store struct {
	dir string
}

// NewStore returns the store rooted at the data directory.
func NewStore(dataDir string) *Store {
	return &Store{dir: filepath.Join(dataDir, "notices")}
}

func (s *Store) feedPath() string  { return filepath.Join(s.dir, "feed.cose") }
func (s *Store) trustPath() string { return filepath.Join(s.dir, "trust.json") }

// LoadTrust reads the trust state. A missing file is the first run and yields a zero
// Trust, which trusts nothing and has seen nothing. A corrupt file is an error rather
// than a silent reset: resetting would drop the highest-version-ever mark, which is
// exactly the state a rollback attack needs gone, so a client that quietly rebuilt it
// from nothing would be doing the attacker's work.
func (s *Store) LoadTrust() (Trust, error) {
	b, err := os.ReadFile(s.trustPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Trust{}, nil
	}
	if err != nil {
		return Trust{}, fault.Wrap(fault.Terminal, CodeStateCorrupt, err)
	}
	var t Trust
	if err := json.Unmarshal(b, &t); err != nil {
		return Trust{}, fault.Wrap(fault.Terminal, CodeStateCorrupt, err)
	}
	return t, nil
}

// SaveTrust writes the trust state atomically, so an interrupted write cannot leave a
// truncated file that the next run reads as "no rollback mark".
func (s *Store) SaveTrust(t Trust) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	b, err := json.Marshal(t)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	if err := fsatomic.WriteFile(s.trustPath(), b, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	return nil
}

// LoadFeed returns the cached signed document, or nil when there is none.
func (s *Store) LoadFeed() ([]byte, error) {
	b, err := os.ReadFile(s.feedPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeStateCorrupt, err)
	}
	return b, nil
}

// SaveFeed caches the signed document.
func (s *Store) SaveFeed(doc []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	if err := fsatomic.WriteFile(s.feedPath(), doc, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	return nil
}

// MarkShown records that these notices have been displayed. Security notices are not
// recorded: they are meant to keep appearing while they apply.
func (s *Store) MarkShown(t Trust, shown []Notice) (Trust, error) {
	changed := false
	for _, n := range shown {
		if n.Severity == Security || t.hasShown(n.ID) {
			continue
		}
		t.Shown = append(t.Shown, n.ID)
		changed = true
	}
	if !changed {
		return t, nil
	}
	// The shown list is bounded by the same ceiling as a feed, dropping the oldest ids
	// first. A dropped id can only cause an old notice to be shown once more, which is
	// harmless; an unbounded list would grow forever on a long-lived install.
	if len(t.Shown) > MaxNotices*4 {
		t.Shown = t.Shown[len(t.Shown)-MaxNotices*4:]
	}
	return t, s.SaveTrust(t)
}
