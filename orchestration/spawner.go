// Package orchestration creates and governs child goals when a run delegates.
// A child is an ordinary Goal resource owned by its parent, so it is torn down
// with it. Its grant is a subset of the parent's, so delegation never widens
// authority. It reserves its share of one budget pool before it is created, so
// a fan-out cannot overshoot, and depth and concurrency are capped.
package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/archetype"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// defaultMaxDepth stops a chain of agents spawning agents even when each step
// looks reasonable.
const defaultMaxDepth = 8

// Spawner is the mission.Fanout over the resource store. It is safe for
// concurrent use.
type Spawner struct {
	store    resource.Store
	ledger   *budget.Ledger
	enqueue  func(ctx context.Context, key resource.Key) error
	perChild budget.Spent // reservation held per child; zero disables budget bounding
	maxDepth int
	sem      chan struct{} // concurrency cap; nil disables it

	mu       sync.Mutex
	children map[string]*childRec
}

// childRec lets Poll release a child's reservation and slot exactly once.
type childRec struct {
	scope    resource.Scope
	pool     string
	released bool
}

// Option configures a Spawner.
type Option func(*Spawner)

// WithReservation makes each child reserve est from the shared pool before it
// is created and release it when it finishes, so the pool caps concurrent
// children. The zero value (default) disables it.
func WithReservation(est budget.Spent) Option { return func(s *Spawner) { s.perChild = est } }

// WithMaxDepth sets the deepest delegation allowed (default 8); n <= 0 keeps
// the default.
func WithMaxDepth(n int) Option {
	return func(s *Spawner) {
		if n > 0 {
			s.maxDepth = n
		}
	}
}

// WithConcurrency caps outstanding children. A spawn past the cap is refused
// instead of queued, so the model serializes. Zero (default) is uncapped.
func WithConcurrency(n int) Option {
	return func(s *Spawner) {
		if n > 0 {
			s.sem = make(chan struct{}, n)
		}
	}
}

// NewSpawner builds a Spawner; ledger may be nil. Call SetEnqueue before the
// first spawn.
func NewSpawner(store resource.Store, ledger *budget.Ledger, opts ...Option) *Spawner {
	s := &Spawner{store: store, ledger: ledger, maxDepth: defaultMaxDepth, children: map[string]*childRec{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SetEnqueue binds how a new child reaches the runtime. It is set after
// construction because the runtime holds the executor that holds the spawner.
func (s *Spawner) SetEnqueue(fn func(ctx context.Context, key resource.Key) error) {
	s.enqueue = fn
}

var _ mission.Fanout = (*Spawner)(nil)

// Request is one spawn. A model-driven spawn sets only Sub; a plan-driven one
// sets the rest too.
type Request struct {
	// Sub is the objective and authority: an action set, or a named archetype.
	Sub mission.SubGoal
	// Name makes the spawn idempotent: a repeat returns the same child and
	// charges once, covering a crash between create and the caller recording
	// it. Empty generates a name.
	Name string
	// StopCondition replaces the generic one. A child never inherits its
	// parent's, or it would run on after the parent is done.
	StopCondition string
	// Ledger is the child's definition of done. A child given one is created
	// already planned, so it does not plan a different one.
	Ledger []goal.LedgerItem
}

// Spawn creates a child goal for sub and returns its id. A refusal (depth,
// budget, capacity) is an error the model can adapt to.
func (s *Spawner) Spawn(ctx context.Context, parent resource.Resource, sub mission.SubGoal) (string, error) {
	return s.SpawnRequest(ctx, parent, Request{Sub: sub})
}

// SpawnRequest is Spawn with the plan-driven fields. Every rule that governs a
// spawn is applied here, so both paths get the same ones.
func (s *Spawner) SpawnRequest(ctx context.Context, parent resource.Resource, req Request) (string, error) {
	if s.enqueue == nil {
		return "", fault.New(fault.Terminal, "spawner_unbound", "spawner: enqueue not bound")
	}
	// An existing named child is returned without a new slot or reservation, so
	// a caller re-entering after a crash resumes its fan-out instead of doubling
	// it.
	if req.Name != "" {
		existing, err := s.store.Get(ctx, goal.Kind, parent.Scope, req.Name)
		switch {
		case err == nil:
			s.adopt(existing, parent)
			return existing.Name, nil
		case !errors.Is(err, resource.ErrNotFound):
			return "", fault.Wrap(fault.Transient, "spawn_lookup", err)
		}
	}
	parentSpec, err := goal.DecodeSpec(parent)
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "spawn_parent_decode", err)
	}

	depth := parentSpec.Depth + 1
	if depth > s.maxDepth {
		return "", fault.New(fault.Forbidden, "spawn_max_depth",
			fmt.Sprintf("spawn refused: delegation depth %d exceeds the maximum %d", depth, s.maxDepth))
	}

	pool := parentSpec.BudgetPool
	if pool == "" {
		pool = parent.Name // the parent is the root of the pool
	}
	scope := parent.Scope

	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
		default:
			return "", fault.New(fault.Forbidden, "spawn_at_capacity",
				"spawn refused: too many children already running")
		}
	}

	if s.ledger != nil && !s.perChild.IsZero() {
		ok, rerr := s.ledger.Reserve(ctx, pool, scope, s.perChild)
		if rerr != nil {
			s.releaseSlot()
			return "", rerr
		}
		if !ok {
			s.releaseSlot()
			return "", fault.New(fault.BudgetExceeded, "spawn_budget",
				"spawn refused: the shared budget cannot cover another child")
		}
	}

	child, err := s.create(ctx, parent, parentSpec, req, depth, pool)
	if err != nil {
		s.refund(ctx, pool, scope)
		s.releaseSlot()
		return "", err
	}
	if err := s.enqueue(ctx, child.Key()); err != nil {
		s.refund(ctx, pool, scope)
		s.releaseSlot()
		return "", fault.Wrap(fault.Transient, "spawn_enqueue", err)
	}

	s.mu.Lock()
	s.children[child.Name] = &childRec{scope: scope, pool: pool}
	s.mu.Unlock()
	return child.Name, nil
}

// create stores the child goal resource.
func (s *Spawner) create(ctx context.Context, parent resource.Resource, parentSpec goal.Spec, req Request, depth int, pool string) (resource.Resource, error) {
	sub := req.Sub
	// A named Agent supplies the capabilities (still narrowed by the parent) and
	// prompt; an unknown one fails the spawn closed.
	childGrant := narrowGrant(parentSpec.Grant, withModelGenerate(sub.Actions))
	childSystem, childDriver, childModel := "", "", ""
	if sub.Agent != "" {
		resolved, err := archetype.Resolve(ctx, s.store, parent.Scope, sub.Agent)
		if err != nil {
			return resource.Resource{}, fault.Wrap(fault.Forbidden, "spawn_agent_resolve", err)
		}
		childGrant = narrowGrant(parentSpec.Grant, withModelGenerate(resolved.Capabilities))
		childSystem, childDriver, childModel = resolved.System, resolved.Driver, resolved.Model
	}
	stop := req.StopCondition
	if stop == "" {
		stop = "the delegated sub-goal is accomplished"
	}
	childSpec := goal.Spec{
		Objective:     sub.Objective,
		StopCondition: stop,
		Grant:         childGrant,
		Depth:         depth,
		BudgetPool:    pool,
		Driver:        childDriver,
		Model:         childModel,
		System:        childSystem,
		Ledger:        req.Ledger,
		// Invariants are copied whole, so spawning is no way out of the parent's
		// terms; an obligation has no narrower form.
		Invariants: parentSpec.Invariants,
		// Allowances are copied and apply only within the narrowed grant. A run
		// cannot mint one for its child, or it would be declaring its own authority.
		Allowances: parentSpec.Allowances,
		// A child runs where its parent does, under the same ceiling.
		Confinement: parentSpec.Confinement,
	}
	raw, err := json.Marshal(childSpec)
	if err != nil {
		return resource.Resource{}, fault.Wrap(fault.Terminal, "spawn_encode", err)
	}
	child := resource.Resource{
		APIVersion: goal.GroupVersion,
		Kind:       goal.Kind,
		Name:       req.Name,
		Scope:      parent.Scope,
		Spec:       raw,
	}
	if req.Name == "" {
		child.GenerateName = parent.Name + "-child-"
	}
	// Marking it planned stops the runtime planning a second, different ledger.
	if len(req.Ledger) > 0 {
		enc, err := goal.Status{Planned: true}.Encode()
		if err != nil {
			return resource.Resource{}, fault.Wrap(fault.Terminal, "spawn_status_encode", err)
		}
		child.Status = enc
	}
	// OwnerReferences is a promoted field, so it is set after the literal. The
	// controller owner makes the child go when the parent does.
	child.OwnerReferences = []resource.OwnerReference{{
		APIVersion: goal.GroupVersion,
		Kind:       goal.Kind,
		Name:       parent.Name,
		ID:         parent.ID,
		Controller: true,
	}}
	saved, err := s.store.Put(ctx, child)
	if err != nil {
		return resource.Resource{}, fault.Wrap(fault.Terminal, "spawn_put", err)
	}
	return saved, nil
}

// Poll reports each finished child's result and whether all have finished. A
// stalled child is Failed. A finished child's reservation and slot are released
// exactly once.
func (s *Spawner) Poll(ctx context.Context, ids []string) ([]mission.ChildResult, bool, error) {
	results := make([]mission.ChildResult, 0, len(ids))
	allDone := true
	for _, id := range ids {
		r, err := s.store.Get(ctx, goal.Kind, s.childScope(id), id)
		if err != nil {
			return nil, false, fault.Wrap(fault.Transient, "spawn_poll_get", err)
		}
		status, err := goal.DecodeStatus(r)
		if err != nil {
			return nil, false, fault.Wrap(fault.Terminal, "spawn_poll_decode", err)
		}
		switch status.Phase {
		case goal.PhaseConverged:
			results = append(results, mission.ChildResult{ID: id, Result: status.Message, Failed: false})
			s.finish(ctx, id)
		case goal.PhaseStalled:
			results = append(results, mission.ChildResult{ID: id, Result: status.Message, Failed: true})
			s.finish(ctx, id)
		default:
			allDone = false
		}
	}
	return results, allDone, nil
}

// adopt records a child found already existing. It acts only after a restart:
// the reservation persists on the budget resource while this map does not, so
// the child is recorded as holding it and releases it when it finishes. The
// pool is read off the child spec, where its spawn wrote it.
func (s *Spawner) adopt(child resource.Resource, parent resource.Resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.children[child.Name]; known {
		return
	}
	pool := parent.Name
	if spec, err := goal.DecodeSpec(child); err == nil && spec.BudgetPool != "" {
		pool = spec.BudgetPool
	}
	s.children[child.Name] = &childRec{scope: child.Scope, pool: pool}
}

func (s *Spawner) finish(ctx context.Context, id string) {
	s.mu.Lock()
	rec := s.children[id]
	if rec == nil || rec.released {
		s.mu.Unlock()
		return
	}
	rec.released = true
	pool, scope := rec.pool, rec.scope
	s.mu.Unlock()

	s.refund(ctx, pool, scope)
	s.releaseSlot()
}

func (s *Spawner) refund(ctx context.Context, pool string, scope resource.Scope) {
	if s.ledger != nil && !s.perChild.IsZero() {
		_ = s.ledger.Release(ctx, pool, scope, s.perChild)
	}
}

func (s *Spawner) releaseSlot() {
	if s.sem != nil {
		select {
		case <-s.sem:
		default:
		}
	}
}

// narrowGrant returns the requested actions the parent holds. A parent with no
// grant is unconstrained, so the child gets what it requested.
func narrowGrant(parentGrant, requested []string) []string {
	if len(parentGrant) == 0 {
		return requested
	}
	return capability.NewGrant(parentGrant...).Narrow(requested...).Actions()
}

// withModelGenerate adds the model-call action to a request, because a child
// granted only tools could never call the model. It is added before narrowing,
// so it never widens the parent's grant.
func withModelGenerate(actions []string) []string {
	for _, a := range actions {
		if a == mission.ActionModelGenerate {
			return actions
		}
	}
	return append(append(make([]string, 0, len(actions)+1), actions...), mission.ActionModelGenerate)
}
