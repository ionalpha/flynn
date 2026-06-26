package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// Record is the persisted description of a model server that was started, so a later,
// separate Flynn process (a `models status` or `models stop` invocation) can find it,
// report it, reach it, or kill it. It holds only loopback coordinates and a pid; no
// secret is ever recorded.
type Record struct {
	// ModelID is the catalog id the server runs, the key a reuse or stop looks up.
	ModelID string `json:"modelID"`
	// PID is the operating-system process id, so a separate process can stop the server.
	PID int `json:"pid"`
	// Port and BaseURL are the loopback coordinates a client targets.
	Port    int    `json:"port"`
	BaseURL string `json:"baseURL"`
	// Runtime names the runtime serving the model, for display.
	Runtime string `json:"runtime,omitempty"`
	// StartedAt is the Unix time the server was started, for display.
	StartedAt int64 `json:"startedAt,omitempty"`
}

// Registry persists the set of running model servers to a single JSON file under the
// data directory, keyed by model id. It is the cross-process record that lets a fresh
// Flynn invocation see and control servers an earlier one started. Access is guarded by
// a mutex within a process; the file is rewritten atomically so a concurrent reader
// never sees a half-written file. It does not itself decide liveness: a record is a
// claim that a server was started, and the manager confirms it with a health probe
// before trusting it, pruning a record whose server no longer answers.
type Registry struct {
	path string
	mu   sync.Mutex
}

// NewRegistry returns a registry backed by servers.json under dir (created on first
// write). dir is typically the data directory's run area.
func NewRegistry(dir string) *Registry {
	return &Registry{path: filepath.Join(dir, "servers.json")}
}

// List returns all recorded servers, sorted by model id for a stable display. A missing
// or unreadable file is treated as an empty registry, not an error, so a first run and a
// cleared registry behave the same.
func (r *Registry) List() ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

// Get returns the record for a model id, and whether one is present.
func (r *Registry) Get(modelID string) (Record, bool, error) {
	recs, err := r.List()
	if err != nil {
		return Record{}, false, err
	}
	for _, rec := range recs {
		if rec.ModelID == modelID {
			return rec, true, nil
		}
	}
	return Record{}, false, nil
}

// Put inserts or replaces the record for its model id and persists the registry.
func (r *Registry) Put(rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs, err := r.loadLocked()
	if err != nil {
		return err
	}
	replaced := false
	for i := range recs {
		if recs[i].ModelID == rec.ModelID {
			recs[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		recs = append(recs, rec)
	}
	return r.saveLocked(recs)
}

// Delete removes the record for a model id, if present, and persists the registry.
// Deleting an absent id is not an error.
func (r *Registry) Delete(modelID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs, err := r.loadLocked()
	if err != nil {
		return err
	}
	out := recs[:0]
	for _, rec := range recs {
		if rec.ModelID != modelID {
			out = append(out, rec)
		}
	}
	return r.saveLocked(out)
}

func (r *Registry) loadLocked() ([]Record, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fault.Wrap(fault.Terminal, "serve_registry_read", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		// A corrupt registry must not wedge the runtime: treat it as empty so a new
		// server can be started and the file rewritten clean, rather than failing every
		// command until a human deletes it.
		return nil, nil //nolint:nilerr // a malformed registry is intentionally read as empty, not surfaced
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ModelID < recs[j].ModelID })
	return recs, nil
}

func (r *Registry) saveLocked(recs []Record) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		return fault.Wrap(fault.Terminal, "serve_registry_mkdir", err)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ModelID < recs[j].ModelID })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Terminal, "serve_registry_encode", err)
	}
	// Write to a sibling temp file and rename, so a reader never sees a partial file and
	// a crash mid-write cannot corrupt the registry.
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, "serve_registry_write", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fault.Wrap(fault.Terminal, "serve_registry_rename", err)
	}
	return nil
}
