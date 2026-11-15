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

// Trust is what a client remembers between runs.
type Trust struct {
	// Version is the highest feed version ever accepted, so a rollback is detected
	// across runs. It never decreases.
	Version uint64 `json:"version"`
	// Checked is when a feed was last accepted; [Due] reads it.
	Checked time.Time `json:"checked"`
	// Shown holds the ids of one-shot notices already displayed. Security notices
	// are never recorded.
	Shown []string `json:"shown"`
}

func (t Trust) hasShown(id string) bool {
	for _, s := range t.Shown {
		if s == id {
			return true
		}
	}
	return false
}

// Store holds the last accepted feed and the [Trust] state under the data directory.
// The feed is cached as signed bytes and re-verified on every run, so a local attacker
// who edits it can destroy a notice but cannot forge one.
type Store struct {
	dir string
}

// NewStore returns the store under dataDir/notices.
func NewStore(dataDir string) *Store {
	return &Store{dir: filepath.Join(dataDir, "notices")}
}

func (s *Store) feedPath() string  { return filepath.Join(s.dir, "feed.cose") }
func (s *Store) trustPath() string { return filepath.Join(s.dir, "trust.json") }

// LoadTrust returns a zero Trust when the file is missing. A corrupt file is an error,
// because resetting it would drop the rollback mark a rollback attack needs gone.
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

// SaveTrust writes atomically, so an interrupted write cannot leave a truncated file
// that loses the rollback mark.
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

// SaveFeed writes doc atomically; [Refresh] calls it only after doc verified.
func (s *Store) SaveFeed(doc []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	if err := fsatomic.WriteFile(s.feedPath(), doc, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, CodeStateNotSaved, err)
	}
	return nil
}

// MarkShown records the one-shot notices in shown and saves t. Security notices are
// skipped so they keep appearing.
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
	// Keep the newest ids so the list stays bounded; a dropped id only lets an old
	// notice show once more.
	if len(t.Shown) > MaxNotices*4 {
		t.Shown = t.Shown[len(t.Shown)-MaxNotices*4:]
	}
	return t, s.SaveTrust(t)
}
