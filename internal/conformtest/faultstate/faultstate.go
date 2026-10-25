// Package faultstate wraps correct state stores so that one behaviour can be
// replaced with a defective one. The conformance suites for state.Provider,
// state.SkillStore and state.MemoryStore are proved able to fail by running
// them against these wrappers (see [conformtest.Prove]); the wrappers live
// here so the proving tests share one decorator instead of each restating a
// page of forwarding methods.
//
// Every hook receives the wrapped store as its first argument, so a defect is
// written as a change to what the correct store would have done: drop a field,
// skip a filter, swallow an error.
package faultstate

import (
	"context"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// Skills is a [state.SkillStore] whose methods run the matching hook when set
// and forward to the embedded store otherwise.
type Skills struct {
	state.SkillStore
	OnUpsert func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error)
	OnGet    func(ctx context.Context, in state.SkillStore, idOrSlug string) (state.Skill, error)
	OnList   func(ctx context.Context, in state.SkillStore, scope state.Scope) ([]state.Skill, error)
	OnSearch func(ctx context.Context, in state.SkillStore, query string, limit int) ([]state.Skill, error)
	OnDelete func(ctx context.Context, in state.SkillStore, id string) error
}

// Upsert runs OnUpsert, or the embedded store's Upsert when it is unset.
func (s *Skills) Upsert(ctx context.Context, sk state.Skill) (state.Skill, error) {
	if s.OnUpsert != nil {
		return s.OnUpsert(ctx, s.SkillStore, sk)
	}
	return s.SkillStore.Upsert(ctx, sk)
}

// Get runs OnGet, or the embedded store's Get when it is unset.
func (s *Skills) Get(ctx context.Context, idOrSlug string) (state.Skill, error) {
	if s.OnGet != nil {
		return s.OnGet(ctx, s.SkillStore, idOrSlug)
	}
	return s.SkillStore.Get(ctx, idOrSlug)
}

// List runs OnList, or the embedded store's List when it is unset.
func (s *Skills) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	if s.OnList != nil {
		return s.OnList(ctx, s.SkillStore, scope)
	}
	return s.SkillStore.List(ctx, scope)
}

// Search runs OnSearch, or the embedded store's Search when it is unset.
func (s *Skills) Search(ctx context.Context, query string, limit int) ([]state.Skill, error) {
	if s.OnSearch != nil {
		return s.OnSearch(ctx, s.SkillStore, query, limit)
	}
	return s.SkillStore.Search(ctx, query, limit)
}

// Delete runs OnDelete, or the embedded store's Delete when it is unset.
func (s *Skills) Delete(ctx context.Context, id string) error {
	if s.OnDelete != nil {
		return s.OnDelete(ctx, s.SkillStore, id)
	}
	return s.SkillStore.Delete(ctx, id)
}

// Memory is a [state.MemoryStore] whose methods run the matching hook when set
// and forward to the embedded store otherwise.
type Memory struct {
	state.MemoryStore
	OnWrite      func(ctx context.Context, in state.MemoryStore, m state.MemoryItem) (state.MemoryItem, error)
	OnRecall     func(ctx context.Context, in state.MemoryStore, q state.RecallQuery) ([]state.MemoryItem, error)
	OnDelete     func(ctx context.Context, in state.MemoryStore, id string) error
	OnRecordPush func(ctx context.Context, in state.MemoryStore, ids []string) error
	OnRecordUse  func(ctx context.Context, in state.MemoryStore, id string, origin state.UsageOrigin) error
	OnPromote    func(ctx context.Context, in state.MemoryStore, d state.PromotionDecision) (state.MemoryPromotion, error)
}

// Write runs OnWrite, or the embedded store's Write when it is unset.
func (m *Memory) Write(ctx context.Context, it state.MemoryItem) (state.MemoryItem, error) {
	if m.OnWrite != nil {
		return m.OnWrite(ctx, m.MemoryStore, it)
	}
	return m.MemoryStore.Write(ctx, it)
}

// Recall runs OnRecall, or the embedded store's Recall when it is unset.
func (m *Memory) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	if m.OnRecall != nil {
		return m.OnRecall(ctx, m.MemoryStore, q)
	}
	return m.MemoryStore.Recall(ctx, q)
}

// Delete runs OnDelete, or the embedded store's Delete when it is unset.
func (m *Memory) Delete(ctx context.Context, id string) error {
	if m.OnDelete != nil {
		return m.OnDelete(ctx, m.MemoryStore, id)
	}
	return m.MemoryStore.Delete(ctx, id)
}

// RecordPush runs OnRecordPush, or the embedded store's RecordPush when it is unset.
func (m *Memory) RecordPush(ctx context.Context, ids []string) error {
	if m.OnRecordPush != nil {
		return m.OnRecordPush(ctx, m.MemoryStore, ids)
	}
	return m.MemoryStore.RecordPush(ctx, ids)
}

// RecordUse runs OnRecordUse, or the embedded store's RecordUse when it is unset.
func (m *Memory) RecordUse(ctx context.Context, id string, origin state.UsageOrigin) error {
	if m.OnRecordUse != nil {
		return m.OnRecordUse(ctx, m.MemoryStore, id, origin)
	}
	return m.MemoryStore.RecordUse(ctx, id, origin)
}

// Promote runs OnPromote, or the embedded store's Promote when it is unset.
func (m *Memory) Promote(ctx context.Context, d state.PromotionDecision) (state.MemoryPromotion, error) {
	if m.OnPromote != nil {
		return m.OnPromote(ctx, m.MemoryStore, d)
	}
	return m.MemoryStore.Promote(ctx, d)
}

// Sessions is a [state.SessionStore] whose methods run the matching hook when
// set and forward to the embedded store otherwise.
type Sessions struct {
	state.SessionStore
	OnGet        func(ctx context.Context, in state.SessionStore, id string) (state.Session, error)
	OnList       func(ctx context.Context, in state.SessionStore) ([]state.Session, error)
	OnAppendTurn func(ctx context.Context, in state.SessionStore, t state.Turn) (state.Turn, error)
	OnTurns      func(ctx context.Context, in state.SessionStore, sessionID string) ([]state.Turn, error)
	OnDelete     func(ctx context.Context, in state.SessionStore, id string) error
}

// Get runs OnGet, or the embedded store's Get when it is unset.
func (s *Sessions) Get(ctx context.Context, id string) (state.Session, error) {
	if s.OnGet != nil {
		return s.OnGet(ctx, s.SessionStore, id)
	}
	return s.SessionStore.Get(ctx, id)
}

// List runs OnList, or the embedded store's List when it is unset.
func (s *Sessions) List(ctx context.Context) ([]state.Session, error) {
	if s.OnList != nil {
		return s.OnList(ctx, s.SessionStore)
	}
	return s.SessionStore.List(ctx)
}

// AppendTurn runs OnAppendTurn, or the embedded store's AppendTurn when it is unset.
func (s *Sessions) AppendTurn(ctx context.Context, t state.Turn) (state.Turn, error) {
	if s.OnAppendTurn != nil {
		return s.OnAppendTurn(ctx, s.SessionStore, t)
	}
	return s.SessionStore.AppendTurn(ctx, t)
}

// Turns runs OnTurns, or the embedded store's Turns when it is unset.
func (s *Sessions) Turns(ctx context.Context, sessionID string) ([]state.Turn, error) {
	if s.OnTurns != nil {
		return s.OnTurns(ctx, s.SessionStore, sessionID)
	}
	return s.SessionStore.Turns(ctx, sessionID)
}

// Delete runs OnDelete, or the embedded store's Delete when it is unset.
func (s *Sessions) Delete(ctx context.Context, id string) error {
	if s.OnDelete != nil {
		return s.OnDelete(ctx, s.SessionStore, id)
	}
	return s.SessionStore.Delete(ctx, id)
}

// Provider is a [state.Provider] that serves the stores and name it is given
// in place of the wrapped provider's. A nil store field means the wrapped
// provider's own.
type Provider struct {
	state.Provider
	SessionStore state.SessionStore
	SkillStore   state.SkillStore
	MemoryStore  state.MemoryStore
	// ProviderName replaces Name when non-nil.
	ProviderName *string
	// CloseErr is returned from Close in place of closing the provider.
	CloseErr error
}

// Name returns ProviderName when set.
func (p *Provider) Name() string {
	if p.ProviderName != nil {
		return *p.ProviderName
	}
	return p.Provider.Name()
}

// Sessions returns SessionStore when set.
func (p *Provider) Sessions() state.SessionStore {
	if p.SessionStore != nil {
		return p.SessionStore
	}
	return p.Provider.Sessions()
}

// Skills returns SkillStore when set.
func (p *Provider) Skills() state.SkillStore {
	if p.SkillStore != nil {
		return p.SkillStore
	}
	return p.Provider.Skills()
}

// Memory returns MemoryStore when set.
func (p *Provider) Memory() state.MemoryStore {
	if p.MemoryStore != nil {
		return p.MemoryStore
	}
	return p.Provider.Memory()
}

// Close returns CloseErr when set.
func (p *Provider) Close() error {
	if p.CloseErr != nil {
		return p.CloseErr
	}
	return p.Provider.Close()
}

// Logged is a [Provider] that also exposes an event log, which is the
// capability the suite's event-sourcing check looks for. It is a separate type
// so a wrapped provider with no log is never mistaken for one with an empty
// log.
type Logged struct {
	*Provider
	// Spine is the log the provider exposes.
	Spine spine.Log
}

// Log returns the log the provider exposes.
func (l Logged) Log() spine.Log { return l.Spine }

// FilteredLog is a [spine.Log] that hides events of the given types from Read,
// the observable effect of a state write that bypassed the log.
type FilteredLog struct {
	spine.Log
	Hide map[string]bool
}

// Read returns the embedded log's events without those whose type is hidden.
func (f FilteredLog) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	all, err := f.Log.Read(ctx, q)
	var out []spine.Event
	for _, e := range all {
		if !f.Hide[e.Type] {
			out = append(out, e)
		}
	}
	return out, err
}
