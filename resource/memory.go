package resource

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
)

// Option configures the in-memory Store.
type Option func(*memStore)

// WithInstanceID sets the instance stamped as origin and last writer (default
// "local").
func WithInstanceID(id string) Option {
	return func(s *memStore) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// WithClock sets the time source (default clock.System).
func WithClock(c clock.Clock) Option {
	return func(s *memStore) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithEventLog backs the store with l, a shared log to observe, audit or Replay.
func WithEventLog(l spine.Log) Option {
	return func(s *memStore) {
		if l != nil {
			s.log = l
		}
	}
}

// WithIDGenerator sets the ID source (default: the store's clock with crypto/rand).
// A seeded generator makes replay deterministic.
func WithIDGenerator(g *ids.Generator) Option {
	return func(s *memStore) {
		if g != nil {
			s.gen = g
		}
	}
}

// WithSnapshotCodec verifies snapshots: Snapshot seals the payload through c and
// Replay opens it before restoring. A snapshot that fails to open is skipped and the
// stream folds from the start, so a tampered or unsigned snapshot is never restored.
func WithSnapshotCodec(c spine.SnapshotCodec) Option {
	return func(s *memStore) {
		s.snapCodec = c
	}
}

// WithSnapshotEvery writes a snapshot after every k successful mutations, so Replay
// folds at most k events past it. It runs after the mutation, outside any lock, and
// a failure never fails the write, since a missing snapshot is only slower. k <= 0
// disables it (the default).
func WithSnapshotEvery(k int) Option {
	return func(s *memStore) {
		s.snapEvery = k
	}
}

// NewMemory returns an in-memory Store admitting against reg. Every mutation is
// recorded on a spine.Log and projected, so the state is a fold of its log (see
// Replay). It is safe for concurrent use.
func NewMemory(reg *Registry, opts ...Option) Store {
	s := &memStore{instanceID: "local", reg: reg}
	for _, o := range opts {
		o(s)
	}
	if s.clk == nil {
		s.clk = clock.System{}
	}
	if s.hlc == nil {
		s.hlc = hlc.NewClock(hlc.WithPhysical(s.clk))
	}
	if s.log == nil {
		s.log = spine.NewMemoryLog(spine.WithClock(s.clk))
	}
	if s.gen == nil {
		s.gen = ids.NewGenerator(ids.WithClock(s.clk))
	}
	st := NewStamper(s.instanceID, s.clk, s.hlc, s.gen, reg)
	s.core = newCore(st, s.log)
	return s
}

var _ Store = (*memStore)(nil)

type memStore struct {
	instanceID string
	clk        clock.Clock
	hlc        *hlc.Clock
	log        spine.Log
	gen        *ids.Generator
	reg        *Registry
	core       *core

	snapCodec   spine.SnapshotCodec
	snapEvery   int
	snapPending atomic.Int64
}

// Log returns the spine the store records on, for observing, auditing or Replay.
func (s *memStore) Log() spine.Log { return s.log }

func (s *memStore) Close() error { return nil }

func (s *memStore) Put(ctx context.Context, r Resource) (Resource, error) {
	rec, err := s.put(ctx, r)
	if err == nil {
		s.maybeSnapshot(ctx)
	}
	return rec, err
}

func (s *memStore) put(ctx context.Context, r Resource) (Resource, error) {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	var existing *Resource
	if id, ok := c.nameIndex[r.Key()]; ok {
		e := c.byID[id]
		existing = &e
	}
	rec, ev, err := c.st.Put(existing, r)
	if err != nil {
		return Resource{}, err
	}
	if err := c.record(ctx, ev, rec); err != nil {
		return Resource{}, err
	}
	return rec, nil
}

func (s *memStore) Merge(ctx context.Context, remote Resource) (MergeResult, error) {
	res, err := s.merge(ctx, remote)
	if err == nil && res.Outcome == MergeApplied {
		s.maybeSnapshot(ctx)
	}
	return res, err
}

func (s *memStore) merge(ctx context.Context, remote Resource) (MergeResult, error) {
	if err := ValidateForMerge(remote); err != nil {
		return MergeResult{}, err
	}
	// Admit the replicated record, so a merge never projects an unregistered kind
	// or invalid spec; kind definitions replicate before their instances.
	if err := s.reg.Validate(remote.APIVersion, remote.Kind, remote.Spec); err != nil {
		return MergeResult{}, err
	}
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.byID[remote.ID]
	if !ok {
		if err := c.recordMerge(ctx, remote); err != nil {
			return MergeResult{}, err
		}
		return MergeResult{Outcome: MergeApplied, Resource: remote}, nil
	}
	winner, take := Resolve(remote, current)
	if !take {
		out := MergeUnchanged
		if winner.UpdatedHLC != remote.UpdatedHLC || winner.LastWriterID != remote.LastWriterID {
			out = MergeIgnored
		}
		return MergeResult{Outcome: out, Resource: current}, nil
	}
	if err := c.recordMerge(ctx, winner); err != nil {
		return MergeResult{}, err
	}
	return MergeResult{Outcome: MergeApplied, Resource: winner}, nil
}

func (s *memStore) Get(_ context.Context, kind string, scope Scope, name string) (Resource, error) {
	c := s.core
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.nameIndex[Key{Kind: kind, Scope: scope, Name: name}]
	if !ok {
		return Resource{}, ErrNotFound
	}
	r := c.byID[id]
	if r.Deleted {
		return Resource{}, ErrNotFound
	}
	return r, nil
}

func (s *memStore) GetByID(_ context.Context, id string) (Resource, error) {
	c := s.core
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.byID[id]
	if !ok || r.Deleted {
		return Resource{}, ErrNotFound
	}
	return r, nil
}

func (s *memStore) List(_ context.Context, kind string, scope Scope, sel Selector) ([]Resource, error) {
	c := s.core
	// Collect through the live index under the read lock and sort outside it, so the
	// lock covers only the copy.
	c.mu.RLock()
	ids := c.live[kind][scope]
	out := make([]Resource, 0, len(ids))
	//ordered:ok sorted below by name and ID
	for id := range ids {
		r := c.byID[id]
		if !sel.Matches(r.Labels) {
			continue
		}
		out = append(out, r)
	}
	c.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *memStore) ListAll(_ context.Context, kind string, sel Selector) ([]Resource, error) {
	c := s.core
	c.mu.RLock()
	out := make([]Resource, 0)
	//ordered:ok sorted below by scope, name and ID
	for _, ids := range c.live[kind] {
		//ordered:ok sorted below by scope, name and ID
		for id := range ids {
			r := c.byID[id]
			if !sel.Matches(r.Labels) {
				continue
			}
			out = append(out, r)
		}
	}
	c.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return lessByScopeName(out[i], out[j]) })
	return out, nil
}

// ListKeys returns the keys of every live resource of a kind from the live index, so
// a resync sweep copies addresses rather than records.
func (s *memStore) ListKeys(_ context.Context, kind string) ([]Key, error) {
	c := s.core
	c.mu.RLock()
	var out []Key
	//ordered:ok sorted below by scope and name, unique within a kind
	for scope, ids := range c.live[kind] {
		//ordered:ok sorted below by scope and name, unique within a kind
		for id := range ids {
			out = append(out, Key{Kind: kind, Scope: scope, Name: c.byID[id].Name})
		}
	}
	c.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return lessKey(out[i], out[j]) })
	return out, nil
}

func (s *memStore) Delete(ctx context.Context, kind string, scope Scope, name string) error {
	err := s.deleteRecord(ctx, kind, scope, name)
	if err == nil {
		s.maybeSnapshot(ctx)
	}
	return err
}

func (s *memStore) deleteRecord(ctx context.Context, kind string, scope Scope, name string) error {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.nameIndex[Key{Kind: kind, Scope: scope, Name: name}]
	if !ok {
		return ErrNotFound
	}
	r := c.byID[id]
	if r.Deleted {
		return ErrNotFound
	}
	if r.DeletionTimestamp != nil {
		return nil // already terminating; deletion completes when finalizers clear
	}
	rec, ev, err := c.st.Delete(r)
	if err != nil {
		return err
	}
	return c.record(ctx, ev, rec)
}

// lessKey orders ListKeys by scope then name, as lessByScopeName does for records.
func lessKey(a, b Key) bool {
	if a.Scope.Instance != b.Scope.Instance {
		return a.Scope.Instance < b.Scope.Instance
	}
	if a.Scope.Project != b.Scope.Project {
		return a.Scope.Project < b.Scope.Project
	}
	if a.Scope.Workspace != b.Scope.Workspace {
		return a.Scope.Workspace < b.Scope.Workspace
	}
	return a.Name < b.Name
}

// lessByScopeName orders ListAll by scope, then name, then ID, so a cross-scope
// listing is deterministic when a name repeats across scopes.
func lessByScopeName(a, b Resource) bool {
	if a.Scope.Instance != b.Scope.Instance {
		return a.Scope.Instance < b.Scope.Instance
	}
	if a.Scope.Project != b.Scope.Project {
		return a.Scope.Project < b.Scope.Project
	}
	if a.Scope.Workspace != b.Scope.Workspace {
		return a.Scope.Workspace < b.Scope.Workspace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.ID < b.ID
}

// core is the in-memory read model. Every mutation appends an event and projects it
// under mu, so log and projection never diverge. Only record (live writes) and
// Replay reach the mutators, so no write bypasses the log.
type core struct {
	st  *Stamper
	log spine.Log

	mu        sync.RWMutex
	lastSeq   int64
	byID      map[string]Resource
	nameIndex map[Key]string // (kind, scope, name) -> id, tombstones included
	// live indexes live resource ids by kind and scope, so List and ListAll visit only
	// candidates. A tombstone leaves it on projection but stays in byID, where Merge
	// resolves late writes against it and snapshots carry it, so the index changes
	// scan cost only.
	live map[string]map[Scope]map[string]struct{}
}

func newCore(st *Stamper, log spine.Log) *core {
	return &core{
		st: st, log: log,
		byID:      map[string]Resource{},
		nameIndex: map[Key]string{},
		live:      map[string]map[Scope]map[string]struct{}{},
	}
}

// recordMerge appends a merge event carrying r verbatim and projects it, so a
// replicated record reaches the log and Replay with its remote envelope. Callers
// hold mu.
func (c *core) recordMerge(ctx context.Context, r Resource) error {
	in, err := MergeEvent(r)
	if err != nil {
		return err
	}
	return c.record(ctx, in, r)
}

// record appends the event and projects post, the canonical record the caller holds,
// instead of decoding the payload again under the lock. The stamper serialized post
// into in.RawPayload, so a live projection equals a replayed one;
// TestReplayEquivalence holds them to byte equality. Callers hold mu.
func (c *core) record(ctx context.Context, in spine.AppendInput, post Resource) error {
	e, err := c.log.Append(ctx, in)
	if err != nil {
		return err
	}
	c.project(post)
	c.lastSeq = e.Seq
	return nil
}

// apply projects one event during Replay. Callers hold mu.
func (c *core) apply(e spine.Event) error {
	switch e.Type {
	case EvPut, EvDeleted, EvMerged:
		r, err := DecodeResource(e.Payload)
		if err != nil {
			return err
		}
		c.project(r)
		return nil
	default:
		return ErrInvalid
	}
}

// project indexes one post-image, the single mutator behind apply and record.
// Callers hold mu.
func (c *core) project(r Resource) {
	c.byID[r.ID] = r
	c.nameIndex[r.Key()] = r.ID
	if r.Deleted {
		c.unindexLive(r.Kind, r.Scope, r.ID)
		return
	}
	c.indexLive(r.Kind, r.Scope, r.ID)
}

// indexLive adds id to the live index. Callers hold mu.
func (c *core) indexLive(kind string, scope Scope, id string) {
	scopes, ok := c.live[kind]
	if !ok {
		scopes = map[Scope]map[string]struct{}{}
		c.live[kind] = scopes
	}
	ids, ok := scopes[scope]
	if !ok {
		ids = map[string]struct{}{}
		scopes[scope] = ids
	}
	ids[id] = struct{}{}
}

// unindexLive drops id from the live index, pruning empty inner maps so churning
// short-lived scopes cannot grow it forever. Callers hold mu.
func (c *core) unindexLive(kind string, scope Scope, id string) {
	scopes, ok := c.live[kind]
	if !ok {
		return
	}
	ids, ok := scopes[scope]
	if !ok {
		return
	}
	delete(ids, id)
	if len(ids) == 0 {
		delete(scopes, scope)
		if len(scopes) == 0 {
			delete(c.live, kind)
		}
	}
}

// Replay rebuilds an in-memory Store by folding a log's resource stream. It resumes
// from the latest resource snapshot and folds only later events, so rebuild work is
// bounded; the result is the same either way, because a snapshot is only a cache.
func Replay(ctx context.Context, log spine.Log, reg *Registry, opts ...Option) (Store, error) {
	s := NewMemory(reg, append(opts, WithEventLog(log))...).(*memStore)
	s.core.mu.Lock()
	defer s.core.mu.Unlock()

	snap, found, err := log.LatestSnapshot(ctx, ResourceStream, 0)
	if err != nil {
		return nil, err
	}
	if found && s.snapCodec != nil {
		// With a codec, only a verified snapshot is restored; one that fails to open
		// is skipped and the stream folds from the start.
		opened, oerr := s.snapCodec.Open(ctx, snap)
		if oerr != nil {
			found = false
		} else {
			snap = opened
		}
	}
	if found {
		if err := s.core.restore(snap.Payload); err != nil {
			// An unreadable snapshot falls back to the log.
			s.core.reset()
		}
	}

	events, err := log.Read(ctx, spine.Query{Stream: ResourceStream, AfterSeq: s.core.lastSeq})
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if err := s.core.apply(e); err != nil {
			return nil, err
		}
		s.core.lastSeq = e.Seq
	}
	return s, nil
}

// snapshotPayload is the projection at a Seq: every record, live and tombstoned,
// keyed by id.
type snapshotPayload struct {
	Resources []Resource `json:"resources"`
	LastSeq   int64      `json:"lastSeq"`
}

// Snapshot checkpoints the projection onto the log as a spine.Snapshot anchored at
// the last applied Seq, so Replay cost stays flat as the stream grows.
func (s *memStore) Snapshot(ctx context.Context) error {
	s.core.mu.RLock()
	resources := make([]Resource, 0, len(s.core.byID))
	//ordered:ok MarshalSnapshot sorts by ID
	for _, r := range s.core.byID {
		resources = append(resources, r)
	}
	lastSeq := s.core.lastSeq
	s.core.mu.RUnlock()

	b, err := MarshalSnapshot(resources, lastSeq)
	if err != nil {
		return err
	}
	snap := spine.Snapshot{Stream: ResourceStream, Seq: lastSeq, Payload: b}
	if s.snapCodec != nil {
		if snap, err = s.snapCodec.Seal(ctx, s.log, snap); err != nil {
			return err
		}
	}
	return s.log.SaveSnapshot(ctx, snap)
}

// maybeSnapshot counts a mutation toward WithSnapshotEvery's cadence and snapshots
// when it is reached. It runs outside the lock, and a failure never fails the write.
func (s *memStore) maybeSnapshot(ctx context.Context) {
	if s.snapEvery <= 0 {
		return
	}
	if s.snapPending.Add(1) < int64(s.snapEvery) {
		return
	}
	s.snapPending.Store(0)
	_ = s.Snapshot(ctx)
}

// MarshalSnapshot serializes a projection's records, live and tombstoned, and its
// Seq into the payload Replay restores. Every backend calls it, so all snapshot in
// one format; records are sorted by id so the payload is deterministic.
func MarshalSnapshot(resources []Resource, lastSeq int64) ([]byte, error) {
	sort.SliceStable(resources, func(i, j int) bool { return resources[i].ID < resources[j].ID })
	return json.Marshal(snapshotPayload{Resources: resources, LastSeq: lastSeq})
}

// UnmarshalSnapshot is the inverse of MarshalSnapshot.
func UnmarshalSnapshot(payload []byte) ([]Resource, int64, error) {
	var pay snapshotPayload
	if err := json.Unmarshal(payload, &pay); err != nil {
		return nil, 0, err
	}
	return pay.Resources, pay.LastSeq, nil
}

// restore rebuilds the read model and name index from a snapshot payload. Callers
// hold mu.
func (c *core) restore(payload []byte) error {
	resources, lastSeq, err := UnmarshalSnapshot(payload)
	if err != nil {
		return err
	}
	c.byID = make(map[string]Resource, len(resources))
	c.nameIndex = make(map[Key]string, len(resources))
	c.live = map[string]map[Scope]map[string]struct{}{}
	for _, r := range resources {
		c.project(r)
	}
	c.lastSeq = lastSeq
	return nil
}

// reset empties the projection, so a failed restore folds from the start. Callers
// hold mu.
func (c *core) reset() {
	c.byID = map[string]Resource{}
	c.nameIndex = map[Key]string{}
	c.live = map[string]map[Scope]map[string]struct{}{}
	c.lastSeq = 0
}
