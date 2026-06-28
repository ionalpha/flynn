// Package orchestration turns a single goal into a graph: its Spawner is the
// concrete fan-out that creates and governs child goals. It is the mission.Fanout
// the executor calls when a run delegates a sub-goal, and it composes the existing
// substrate rather than adding new mechanism: a child is an ordinary Goal resource
// owned by its parent (so it is torn down with the parent), carrying a capability
// grant narrowed to a subset of the parent's (so a delegation can never widen
// authority), charging one shared budget pool reserved per child before it is
// created (so a fan-out cannot overshoot), at a bounded delegation depth (so agents
// spawning agents cannot recurse without end), under a concurrency cap (so the
// blast radius is bounded). Poll reports each child's outcome from its goal status.
package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// defaultMaxDepth bounds delegation recursion when no depth is configured: a chain
// of agents spawning agents stops here even if each step looks locally reasonable.
const defaultMaxDepth = 8

// Spawner is the concrete mission.Fanout over the resource store. It creates child
// goals, governs them, and reports their outcomes. It is safe for concurrent use.
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

// childRec is the spawner's bookkeeping for one outstanding child, so Poll can
// resolve it by id and release its reservation and concurrency slot exactly once
// when it finishes.
type childRec struct {
	scope    resource.Scope
	pool     string
	released bool
}

// Option configures a Spawner.
type Option func(*Spawner)

// WithReservation bounds a fan-out by budget: each child reserves est against the
// shared pool before it is created, and the reservation is released when the child
// finishes, so the number of concurrent children is capped by what the pool can
// cover. The zero estimate (default) disables budget bounding.
func WithReservation(est budget.Spent) Option { return func(s *Spawner) { s.perChild = est } }

// WithMaxDepth sets the maximum delegation depth (default defaultMaxDepth). A child
// past this depth is refused, so recursion is bounded.
func WithMaxDepth(n int) Option {
	return func(s *Spawner) {
		if n > 0 {
			s.maxDepth = n
		}
	}
}

// WithConcurrency caps how many children may be outstanding at once; a spawn past
// the cap is refused rather than queued. Zero (default) leaves it uncapped (budget
// and depth still bound the fan-out).
func WithConcurrency(n int) Option {
	return func(s *Spawner) {
		if n > 0 {
			s.sem = make(chan struct{}, n)
		}
	}
}

// NewSpawner builds a Spawner over store and an optional budget ledger. Bind the
// enqueue function (SetEnqueue) before driving runs: it is how a created child is
// handed to the runtime, and is injected rather than taken at construction so the
// spawner does not depend on the runtime it feeds.
func NewSpawner(store resource.Store, ledger *budget.Ledger, opts ...Option) *Spawner {
	s := &Spawner{store: store, ledger: ledger, maxDepth: defaultMaxDepth, children: map[string]*childRec{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SetEnqueue binds the function that hands a created child goal to the runtime for
// reconciliation. It is set after the runtime exists, breaking the construction
// cycle between the executor (which holds the spawner) and the runtime (which holds
// the executor).
func (s *Spawner) SetEnqueue(fn func(ctx context.Context, key resource.Key) error) {
	s.enqueue = fn
}

var _ mission.Fanout = (*Spawner)(nil)

// Spawn creates a child goal for sub, owned by parent, with a narrowed grant, a
// reserved share of the shared budget, at the next depth, under the concurrency
// cap. It returns the child's id, or a refusal (depth exceeded, budget exhausted,
// at capacity) the model sees as a failed spawn it can adapt to.
func (s *Spawner) Spawn(ctx context.Context, parent resource.Resource, sub mission.SubGoal) (string, error) {
	if s.enqueue == nil {
		return "", fault.New(fault.Terminal, "spawner_unbound", "spawner: enqueue not bound")
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

	// Concurrency slot: refuse rather than queue, so the model serializes.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
		default:
			return "", fault.New(fault.Forbidden, "spawn_at_capacity",
				"spawn refused: too many children already running")
		}
	}

	// Budget: reserve a share against the shared pool before creating the child, so a
	// fan-out cannot overshoot. Release the slot if the pool cannot cover it.
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

	child, err := s.create(ctx, parent, parentSpec, sub, depth, pool)
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

// create builds and stores the child goal resource: owned by the parent (controller
// owner, for cascade teardown), with a grant narrowed to the subset of the parent's
// authority the sub-goal asked for, at the next depth, sharing the pool.
func (s *Spawner) create(ctx context.Context, parent resource.Resource, parentSpec goal.Spec, sub mission.SubGoal, depth int, pool string) (resource.Resource, error) {
	// A named Agent configures the child from its resolved bundle: its capabilities
	// (intersected with the parent's authority, never widened) and its system prompt.
	// An ad-hoc child takes its authority from the requested actions. An unknown Agent
	// fails the spawn closed.
	childGrant := narrowGrant(parentSpec.Grant, sub.Actions)
	childSystem := ""
	if sub.Agent != "" {
		resolved, err := archetype.Resolve(ctx, s.store, parent.Scope, sub.Agent)
		if err != nil {
			return resource.Resource{}, fault.Wrap(fault.Forbidden, "spawn_agent_resolve", err)
		}
		childGrant = narrowGrant(parentSpec.Grant, resolved.Capabilities)
		childSystem = resolved.System
	}
	childSpec := goal.Spec{
		Objective:     sub.Objective,
		StopCondition: "the delegated sub-goal is accomplished",
		Grant:         childGrant,
		Depth:         depth,
		BudgetPool:    pool,
		System:        childSystem,
	}
	raw, err := json.Marshal(childSpec)
	if err != nil {
		return resource.Resource{}, fault.Wrap(fault.Terminal, "spawn_encode", err)
	}
	child := resource.Resource{
		APIVersion:   goal.GroupVersion,
		Kind:         goal.Kind,
		GenerateName: parent.Name + "-child-",
		Scope:        parent.Scope,
		Spec:         raw,
	}
	// OwnerReferences is a promoted embedded field, so it is set after the literal.
	// The parent is the controller owner, so the child is garbage-collected when the
	// parent is gone (cascade teardown).
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

// Poll reports each child's outcome and whether all have finished. A converged
// child yields its final answer; a stalled one yields a failure. A child still
// running leaves allDone false. When a child reaches a terminal state, its budget
// reservation and concurrency slot are released exactly once.
func (s *Spawner) Poll(ctx context.Context, ids []string) ([]mission.ChildResult, bool, error) {
	results := make([]mission.ChildResult, 0, len(ids))
	allDone := true
	for _, id := range ids {
		s.mu.Lock()
		rec := s.children[id]
		s.mu.Unlock()
		scope := resource.Scope{}
		if rec != nil {
			scope = rec.scope
		}
		r, err := s.store.Get(ctx, goal.Kind, scope, id)
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

// finish releases a terminal child's reservation and concurrency slot exactly once.
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

// refund returns a child's budget reservation to the shared pool.
func (s *Spawner) refund(ctx context.Context, pool string, scope resource.Scope) {
	if s.ledger != nil && !s.perChild.IsZero() {
		_ = s.ledger.Release(ctx, pool, scope, s.perChild)
	}
}

// releaseSlot frees one concurrency slot.
func (s *Spawner) releaseSlot() {
	if s.sem != nil {
		select {
		case <-s.sem:
		default:
		}
	}
}

// narrowGrant returns the actions a child may take: the subset of the requested
// actions the parent is authorized for, so a child never exceeds its parent. A
// parent with no explicit grant is unconstrained, so the child is scoped to exactly
// what it requested.
func narrowGrant(parentGrant, requested []string) []string {
	if len(parentGrant) == 0 {
		return requested
	}
	return capability.NewGrant(parentGrant...).Narrow(requested...).Actions()
}
