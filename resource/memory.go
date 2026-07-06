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

// WithInstanceID sets the origin/last-writer instance stamped onto records this
// store creates (default "local").
func WithInstanceID(id string) Option {
	return func(s *memStore) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// WithClock sets the time source (default clock.System), so tests and replay can
// supply a clock.Manual.
func WithClock(c clock.Clock) Option {
	return func(s *memStore) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithEventLog backs the store with a specific spine.Log instead of a private
// in-memory one (inject a shared log to observe, audit, or Replay the stream).
func WithEventLog(l spine.Log) Option {
	return func(s *memStore) {
		if l != nil {
			s.log = l
		}
	}
}

// WithIDGenerator sets the source of resource IDs (default: a generator on the
// store's clock with crypto/rand entropy). Supply a seeded generator for
// deterministic replay.
func WithIDGenerator(g *ids.Generator) Option {
	return func(s *memStore) {
		if g != nil {
			s.gen = g
		}
	}
}

// WithSnapshotCodec makes the store's snapshots verified: Snapshot seals the
// projection payload through the codec before saving it, and Replay opens
// (verifies) a stored snapshot through it before restoring - one that fails to
// open is skipped and the stream is folded from the start instead. With a codec
// set, an unsigned or tampered snapshot is never restored.
func WithSnapshotCodec(c spine.SnapshotCodec) Option {
	return func(s *memStore) {
		s.snapCodec = c
	}
}

// WithSnapshotEvery makes the store checkpoint itself automatically: after every
// k successful mutations it writes a snapshot, so a later Replay folds at most k
// events past the last checkpoint instead of the whole stream. The snapshot is
// written after the mutation completes, outside any lock, and best effort: a
// snapshot failure never fails the write (a missing snapshot is only slower,
// never wrong). Zero or negative disables automatic snapshots (the default).
func WithSnapshotEvery(k int) Option {
	return func(s *memStore) {
		s.snapEvery = k
	}
}

// NewMemory returns an in-memory Store admitting against reg. Every mutation is
// recorded on a spine.Log and projected, so the store's state is always a fold of
// its log (see Replay). Safe for concurrent use; the zero-setup default backend.
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

// Log returns the spine this store records mutations on, so the stream can be
// observed, audited, or folded with Replay. It is the event-sourced capability the
// conformance suite holds the store to.
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
	// Admit the replicated record so a merge can never project a resource of an
	// unregistered kind or an invalid spec; kind definitions (themselves resources)
	// replicate before instances of that kind.
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
	// Collect matches under the read lock via the live index (only candidates of
	// this kind and scope are visited, never tombstones), then sort outside it so
	// the lock is held for the copy alone.
	c.mu.RLock()
	ids := c.live[kind][scope]
	out := make([]Resource, 0, len(ids))
	for id := range ids {
		r := c.byID[id]
		if !sel.Matches(r.Labels) {
			continue
		}
		out = append(out, r)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
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
	for _, ids := range c.live[kind] {
		for id := range ids {
			r := c.byID[id]
			if !sel.Matches(r.Labels) {
				continue
			}
			out = append(out, r)
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return lessByScopeName(out[i], out[j]) })
	return out, nil
}

// ListKeys is the KeyLister capability: the keys of every live resource of a
// kind, straight from the live index, so a resync sweep copies addresses rather
// than records.
func (s *memStore) ListKeys(_ context.Context, kind string) ([]Key, error) {
	c := s.core
	c.mu.RLock()
	var out []Key
	for scope, ids := range c.live[kind] {
		for id := range ids {
			out = append(out, Key{Kind: kind, Scope: scope, Name: c.byID[id].Name})
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i], out[j]) })
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

// lessKey is the total order ListKeys returns: by scope (instance, project,
// workspace), then name, mirroring lessByScopeName without a record in hand.
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

// lessByScopeName is the total order ListAll returns: by scope (instance, project,
// workspace), then name, with an ID tiebreak, so a cross-scope listing is
// deterministic even when a name repeats across scopes.
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

// core is the in-memory read model behind the command path. Every mutation
// appends an event and projects it under mu, so the log and projection never
// diverge. apply is the only mutator and is reached only from record (live writes)
// and Replay (reconstruction), so no write bypasses the log.
type core struct {
	st  *Stamper
	log spine.Log

	mu        sync.RWMutex
	lastSeq   int64
	byID      map[string]Resource
	nameIndex map[Key]string // (kind, scope, name) -> id, tombstones included
	// live indexes the ids of live (non-tombstoned) resources by kind and scope, so
	// List and ListAll visit only candidates instead of scanning every record ever
	// written. A tombstone leaves this index the moment it projects but stays in
	// byID: Merge still resolves a late remote write against the deletion by HLC,
	// and a snapshot still carries the tombstone, so eviction here changes only the
	// scan cost, never a read's result.
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
// replicated record lands on the log (and thus into Replay) exactly like a local
// write, with its remote envelope preserved. Callers hold mu.
func (c *core) recordMerge(ctx context.Context, r Resource) error {
	in, err := MergeEvent(r)
	if err != nil {
		return err
	}
	return c.record(ctx, in, r)
}

// record appends the event and projects its post-image onto the read model. post is
// the canonical record the caller already holds (the stamper's output for a write,
// the merge winner for a merge), so record projects it directly instead of decoding
// the raw payload again under the store lock. The stamper serialized post into
// in.RawPayload, so this is the same post-image a replay would decode from the event,
// keeping a live projection identical to a rebuilt-from-log one (TestReplayEquivalence
// holds the two to byte equality). Callers hold mu.
func (c *core) record(ctx context.Context, in spine.AppendInput, post Resource) error {
	e, err := c.log.Append(ctx, in)
	if err != nil {
		return err
	}
	c.project(post)
	c.lastSeq = e.Seq
	return nil
}

// apply projects one event onto the read model during Replay; the live write
// path (record) projects the same post-image from the raw payload instead, so a
// rebuilt-from-log store is identical to a live one. Callers hold mu.
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

// project indexes one post-image record. The single mutator behind apply
// (Replay) and record (live writes). Callers hold mu.
func (c *core) project(r Resource) {
	c.byID[r.ID] = r
	c.nameIndex[r.Key()] = r.ID
	if r.Deleted {
		c.unindexLive(r.Kind, r.Scope, r.ID)
		return
	}
	c.indexLive(r.Kind, r.Scope, r.ID)
}

// indexLive adds an id to the live index under (kind, scope). Callers hold mu.
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

// unindexLive drops an id from the live index, pruning emptied inner maps so a
// churn of short-lived scopes cannot grow the index forever. Callers hold mu.
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

// Replay reconstructs an in-memory Store by folding a log's resource stream: the
// running proof that the resource layer is a projection of the spine. It resumes from
// the log's latest resource snapshot when one exists and folds only the events
// after it, so a long-lived stream rebuilds in bounded work instead of from the
// start. The result is identical either way: a snapshot is a cache, not a source
// of truth.
func Replay(ctx context.Context, log spine.Log, reg *Registry, opts ...Option) (Store, error) {
	s := NewMemory(reg, append(opts, WithEventLog(log))...).(*memStore)
	s.core.mu.Lock()
	defer s.core.mu.Unlock()

	snap, found, err := log.LatestSnapshot(ctx, ResourceStream, 0)
	if err != nil {
		return nil, err
	}
	if found && s.snapCodec != nil {
		// A codec makes restore fail closed: only a snapshot the codec verifies is
		// restored. One that fails to open (tampered, unsigned, signed by an unknown
		// key) is skipped and the stream folds from the start - only slower, never
		// wrong.
		opened, oerr := s.snapCodec.Open(ctx, snap)
		if oerr != nil {
			found = false
		} else {
			snap = opened
		}
	}
	if found {
		if err := s.core.restore(snap.Payload); err != nil {
			// A snapshot is a derived cache; an unreadable one falls back to the log.
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

// snapshotPayload is the serialized resource projection at a Seq: every record
// (live and tombstoned) keyed by id, enough to restore the read model without
// folding from the start.
type snapshotPayload struct {
	Resources []Resource `json:"resources"`
	LastSeq   int64      `json:"lastSeq"`
}

// Snapshot checkpoints the store's current projection onto its event log as a
// spine.Snapshot on the resource stream, anchored at the last applied Seq, so a
// later Replay resumes from it. It keeps rebuild cost flat as the stream grows.
func (s *memStore) Snapshot(ctx context.Context) error {
	s.core.mu.RLock()
	resources := make([]Resource, 0, len(s.core.byID))
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

// maybeSnapshot counts one successful mutation toward the automatic snapshot
// cadence and checkpoints the store when the cadence is reached. It runs after
// the mutation, outside the store lock, and best effort: a snapshot failure never
// fails the write it followed.
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

// MarshalSnapshot serializes a projection (all its records, live and tombstoned,
// and the Seq it is current as of) into the snapshot payload Replay restores from.
// A backend builds the record set from its own storage and calls this, so every
// backend snapshots in one identical format. Records are sorted by id so the
// payload is deterministic.
func MarshalSnapshot(resources []Resource, lastSeq int64) ([]byte, error) {
	sort.Slice(resources, func(i, j int) bool { return resources[i].ID < resources[j].ID })
	return json.Marshal(snapshotPayload{Resources: resources, LastSeq: lastSeq})
}

// UnmarshalSnapshot decodes a snapshot payload back into the records and the Seq
// it is current as of - the inverse of MarshalSnapshot, so any backend restores
// from the one shared format.
func UnmarshalSnapshot(payload []byte) ([]Resource, int64, error) {
	var pay snapshotPayload
	if err := json.Unmarshal(payload, &pay); err != nil {
		return nil, 0, err
	}
	return pay.Resources, pay.LastSeq, nil
}

// restore rebuilds the read model from a snapshot payload, reconstructing the
// name index from the records. Callers hold mu.
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

// reset returns the projection to its empty state, so a failed restore leaves a
// store that folds from the start of the stream. Callers hold mu.
func (c *core) reset() {
	c.byID = map[string]Resource{}
	c.nameIndex = map[Key]string{}
	c.live = map[string]map[Scope]map[string]struct{}{}
	c.lastSeq = 0
}
