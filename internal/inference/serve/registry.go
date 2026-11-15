package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
)

// Record is the persisted description of a started model server, so a later Flynn
// process (`models status`, `models stop`) can find, reach or kill it. It holds only
// loopback coordinates and a pid, never a secret.
type Record struct {
	// ModelID is the catalog id, the key a reuse or stop looks up.
	ModelID string `json:"modelID"`
	// PID is the host process id of a process-backed server. A container-backed server
	// has none and carries ContainerID instead.
	PID int `json:"pid"`
	// ContainerID and Engine identify a container-backed server, so a later process can
	// stop it through the engine. Exactly one of PID or ContainerID is set.
	ContainerID string `json:"containerID,omitempty"`
	Engine      string `json:"engine,omitempty"`
	// Port and BaseURL are the loopback coordinates a client targets.
	Port    int    `json:"port"`
	BaseURL string `json:"baseURL"`
	// Runtime names the serving runtime, for display.
	Runtime string `json:"runtime,omitempty"`
	// Version is the runtime build. A later process checks it against the advisory floor
	// before adopting the server, and treats a missing one as below every floor.
	Version string `json:"version,omitempty"`
	// Containment is the isolation the launch ran under, as the sandbox reported it. A
	// missing one reads as the weakest level.
	Containment string `json:"containment,omitempty"`
	// StartedAt is the Unix start time, for display.
	StartedAt int64 `json:"startedAt,omitempty"`
}

// Registry persists the running model servers to one JSON file, keyed by model id, so a
// fresh Flynn invocation can control servers an earlier one started. A mutex guards it
// within a process, and the file is rewritten atomically so a reader never sees a partial
// file. A record only claims a server was started; the manager confirms it with a health
// probe and prunes one that no longer answers.
type Registry struct {
	path string
	mu   sync.Mutex
}

// NewRegistry returns a registry backed by servers.json under dir, created on first
// write.
func NewRegistry(dir string) *Registry {
	return &Registry{path: filepath.Join(dir, "servers.json")}
}

// List returns all records sorted by model id. A missing or unreadable file is an empty
// registry, so a first run and a cleared registry behave the same.
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

// Delete removes the record for a model id and persists the registry. Deleting an absent
// id is not an error.
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
		// A corrupt registry reads as empty, so the next start rewrites it clean instead
		// of every command failing until someone deletes the file.
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
	if err := fsatomic.WriteFile(r.path, data, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, "serve_registry_write", err)
	}
	return nil
}
