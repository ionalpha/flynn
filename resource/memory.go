package resource

import (
	"context"
	"sort"
	"sync"

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
}

// Log returns the spine this store records mutations on, so the stream can be
// observed, audited, or folded with Replay. It is the event-sourced capability the
// conformance suite holds the store to.
func (s *memStore) Log() spine.Log { return s.log }

func (s *memStore) Close() error { return nil }

func (s *memStore) Put(ctx context.Context, r Resource) (Resource, error) {
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
	if err := c.record(ctx, ev); err != nil {
		return Resource{}, err
	}
	return rec, nil
}

func (s *memStore) Get(_ context.Context, kind string, scope Scope, name string) (Resource, error) {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
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
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byID[id]
	if !ok || r.Deleted {
		return Resource{}, ErrNotFound
	}
	return r, nil
}

func (s *memStore) List(_ context.Context, kind string, scope Scope, sel Selector) ([]Resource, error) {
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Resource, 0)
	for _, r := range c.byID {
		if r.Deleted || r.Kind != kind || r.Scope != scope {
			continue
		}
		if !sel.Matches(r.Labels) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *memStore) Delete(ctx context.Context, kind string, scope Scope, name string) error {
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
	_, ev, err := c.st.Delete(r)
	if err != nil {
		return err
	}
	return c.record(ctx, ev)
}

// core is the in-memory read model behind the command path. Every mutation
// appends an event and projects it under mu, so the log and projection never
// diverge. apply is the only mutator and is reached only from record (live writes)
// and Replay (reconstruction), so no write bypasses the log.
type core struct {
	st  *Stamper
	log spine.Log

	mu        sync.Mutex
	lastSeq   int64
	byID      map[string]Resource
	nameIndex map[Key]string // (kind, scope, name) -> id, tombstones included
}

func newCore(st *Stamper, log spine.Log) *core {
	return &core{st: st, log: log, byID: map[string]Resource{}, nameIndex: map[Key]string{}}
}

func (c *core) record(ctx context.Context, in spine.AppendInput) error {
	e, err := c.log.Append(ctx, in)
	if err != nil {
		return err
	}
	if err := c.apply(e); err != nil {
		return err
	}
	c.lastSeq = e.Seq
	return nil
}

// apply projects one event onto the read model. Shared by record and Replay, so a
// rebuilt-from-log store is identical to a live one. Callers hold mu.
func (c *core) apply(e spine.Event) error {
	switch e.Type {
	case EvPut, EvDeleted:
		r, err := DecodeResource(e.Payload)
		if err != nil {
			return err
		}
		c.byID[r.ID] = r
		c.nameIndex[r.Key()] = r.ID
		return nil
	default:
		return ErrInvalid
	}
}

// Replay reconstructs an in-memory Store purely by folding a log's resource
// stream: the running proof that the substrate is a projection of the spine.
func Replay(ctx context.Context, log spine.Log, reg *Registry, opts ...Option) (Store, error) {
	s := NewMemory(reg, append(opts, WithEventLog(log))...).(*memStore)
	events, err := log.Read(ctx, spine.Query{Stream: ResourceStream})
	if err != nil {
		return nil, err
	}
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	for _, e := range events {
		if err := s.core.apply(e); err != nil {
			return nil, err
		}
		s.core.lastSeq = e.Seq
	}
	return s, nil
}
