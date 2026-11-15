package modelsource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
)

// Provenance is the durable record of a model source: where it came from, how it was
// classified, its weight format and its pinned digest. The digest is the
// trust-on-first-use anchor, so a source whose bytes change is refused. It never holds
// a secret.
type Provenance struct {
	// Key is the stable source key ([Source.Key]), the record's primary key.
	Key string `json:"key"`
	// Raw is the original reference the user supplied.
	Raw string `json:"raw"`
	// Trust is the classified trust level, recorded as its plain name.
	Trust string `json:"trust"`
	// Format is the detected weight format name.
	Format string `json:"format,omitempty"`
	// Digest is the "sha256:..." the source's bytes were pinned to, empty until the
	// weights have been fetched and verified once.
	Digest string `json:"digest,omitempty"`
	// FirstSeen and LastVerified are Unix times: the first record of the source and the
	// most recent successful integrity check.
	FirstSeen    int64 `json:"firstSeen,omitempty"`
	LastVerified int64 `json:"lastVerified,omitempty"`
}

// Ledger persists provenance to one JSON file keyed by source key, as both the
// trust-on-first-use store and the audit log. The file is rewritten atomically under a
// mutex, so a reader never sees a partial file. A corrupt ledger reads as empty so it
// cannot wedge a run.
type Ledger struct {
	path string
	mu   sync.Mutex
}

// NewLedger returns a ledger backed by provenance.json under dir.
func NewLedger(dir string) *Ledger {
	return &Ledger{path: filepath.Join(dir, "provenance.json")}
}

// PinnedDigest returns the digest recorded for a source key, and whether one is pinned.
// The caller passes it to the verified fetch, so a pinned source cannot be swapped
// under a later run.
func (l *Ledger) PinnedDigest(key string) (string, bool, error) {
	rec, ok, err := l.Get(key)
	if err != nil || !ok {
		return "", false, err
	}
	return rec.Digest, rec.Digest != "", nil
}

// Get returns the record for a source key, and whether one exists.
func (l *Ledger) Get(key string) (Provenance, bool, error) {
	recs, err := l.List()
	if err != nil {
		return Provenance{}, false, err
	}
	for _, r := range recs {
		if r.Key == key {
			return r, true, nil
		}
	}
	return Provenance{}, false, nil
}

// List returns all provenance records, sorted by key. A missing or unreadable file is an
// empty ledger.
func (l *Ledger) List() ([]Provenance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadLocked()
}

// Record inserts or updates the provenance for its key and persists the ledger. An
// update keeps FirstSeen, so the audit trail retains the original sighting.
func (l *Ledger) Record(p Provenance) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs, err := l.loadLocked()
	if err != nil {
		return err
	}
	replaced := false
	for i := range recs {
		if recs[i].Key == p.Key {
			if p.FirstSeen == 0 {
				p.FirstSeen = recs[i].FirstSeen
			}
			recs[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		recs = append(recs, p)
	}
	return l.saveLocked(recs)
}

func (l *Ledger) loadLocked() ([]Provenance, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fault.Wrap(fault.Terminal, "modelsource_ledger_read", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var recs []Provenance
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, nil //nolint:nilerr // a malformed ledger is intentionally read as empty, not surfaced
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Key < recs[j].Key })
	return recs, nil
}

func (l *Ledger) saveLocked(recs []Provenance) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o750); err != nil {
		return fault.Wrap(fault.Terminal, "modelsource_ledger_mkdir", err)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Key < recs[j].Key })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Terminal, "modelsource_ledger_encode", err)
	}
	if err := fsatomic.WriteFile(l.path, data, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, "modelsource_ledger_write", err)
	}
	return nil
}
