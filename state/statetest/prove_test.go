package statetest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/internal/conformtest"
	"github.com/ionalpha/flynn/internal/conformtest/faultstate"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
)

// parts is the set of wrappers one broken provider is assembled from. A case
// sets hooks on whichever it needs.
type parts struct {
	provider *faultstate.Provider
	sessions *faultstate.Sessions
	skills   *faultstate.Skills
	memory   *faultstate.Memory
	// log, when set, makes the provider expose an event log built from the inner one.
	log func(inner spine.Log) spine.Log
}

// newParts wraps a fresh in-memory provider.
func newParts() *parts {
	in := state.NewMemory()
	x := &parts{
		sessions: &faultstate.Sessions{SessionStore: in.Sessions()},
		skills:   &faultstate.Skills{SkillStore: in.Skills()},
		memory:   &faultstate.Memory{MemoryStore: in.Memory()},
	}
	x.provider = &faultstate.Provider{
		Provider: in, SessionStore: x.sessions, SkillStore: x.skills, MemoryStore: x.memory,
	}
	return x
}

func (x *parts) build() state.Provider {
	if x.log == nil {
		return x.provider
	}
	inner := x.provider.Provider.(interface{ Log() spine.Log }).Log()
	return faultstate.Logged{Provider: x.provider, Spine: x.log(inner)}
}

func TestSuiteFailsOnBrokenProvider(t *testing.T) {
	c := func(name, want string, brk func(x *parts)) conformtest.Case {
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) {
			statetest.RunSuite(t, func() state.Provider {
				x := newParts()
				brk(x)
				return x.build()
			})
		}}
	}
	hidden := func(types ...string) func(x *parts) {
		return func(x *parts) {
			x.log = func(inner spine.Log) spine.Log {
				h := map[string]bool{}
				for _, ty := range types {
					h[ty] = true
				}
				return faultstate.FilteredLog{Log: inner, Hide: h}
			}
		}
	}
	noName := ""
	conformtest.Prove(t, []conformtest.Case{
		c("has no name", "NameAndClose", func(x *parts) { x.provider.ProviderName = &noName }),
		c("close fails", "NameAndClose", func(x *parts) { x.provider.CloseErr = errors.New("stuck") }),

		c("missing session reads as found", "Sessions", func(x *parts) {
			x.sessions.OnGet = func(ctx context.Context, in state.SessionStore, id string) (state.Session, error) {
				s, err := in.Get(ctx, id)
				if errors.Is(err, state.ErrNotFound) {
					return s, nil
				}
				return s, err
			}
		}),
		c("turns all take sequence 1", "Sessions", func(x *parts) {
			x.sessions.OnAppendTurn = func(ctx context.Context, in state.SessionStore, tn state.Turn) (state.Turn, error) {
				out, err := in.AppendTurn(ctx, tn)
				out.Seq = 1
				return out, err
			}
		}),
		c("transcript comes back reversed", "Sessions", func(x *parts) {
			x.sessions.OnTurns = func(ctx context.Context, in state.SessionStore, id string) ([]state.Turn, error) {
				turns, err := in.Turns(ctx, id)
				slices.Reverse(turns)
				return turns, err
			}
		}),
		c("appending to a missing session succeeds", "Sessions", func(x *parts) {
			x.sessions.OnAppendTurn = func(ctx context.Context, in state.SessionStore, tn state.Turn) (state.Turn, error) {
				out, err := in.AppendTurn(ctx, tn)
				if errors.Is(err, state.ErrNotFound) {
					return out, nil
				}
				return out, err
			}
		}),
		c("sessions list newest first", "Sessions", func(x *parts) {
			x.sessions.OnList = func(ctx context.Context, in state.SessionStore) ([]state.Session, error) {
				all, err := in.List(ctx)
				slices.Reverse(all)
				return all, err
			}
		}),
		c("session delete does nothing", "Sessions", func(x *parts) {
			x.sessions.OnDelete = func(context.Context, state.SessionStore, string) error { return nil }
		}),

		c("skill list ignores the scope", "Skills", func(x *parts) {
			x.skills.OnList = func(ctx context.Context, in state.SkillStore, _ state.Scope) ([]state.Skill, error) {
				return in.Search(ctx, "", 0)
			}
		}),
		c("skill update keeps version 1", "Skills", func(x *parts) {
			x.skills.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				out, err := in.Upsert(ctx, sk)
				out.Version = 1
				return out, err
			}
		}),
		c("skill search ignores the limit", "Skills", func(x *parts) {
			x.skills.OnSearch = func(ctx context.Context, in state.SkillStore, q string, _ int) ([]state.Skill, error) {
				return in.Search(ctx, q, 0)
			}
		}),
		c("skill get by slug is not found", "Skills", func(x *parts) {
			x.skills.OnGet = func(ctx context.Context, in state.SkillStore, key string) (state.Skill, error) {
				sk, err := in.Get(ctx, key)
				if err == nil && sk.ID != key {
					return state.Skill{}, state.ErrNotFound
				}
				return sk, err
			}
		}),

		c("stale skill writes are accepted", "SkillOptimisticConcurrency", func(x *parts) {
			x.skills.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				sk.SyncVersion = 0
				return in.Upsert(ctx, sk)
			}
		}),

		c("skill delete does nothing", "SkillTombstone", func(x *parts) {
			x.skills.OnDelete = func(context.Context, state.SkillStore, string) error { return nil }
		}),
		c("deleting a skill twice succeeds", "SkillTombstone", func(x *parts) {
			x.skills.OnDelete = func(ctx context.Context, in state.SkillStore, id string) error {
				if err := in.Delete(ctx, id); !errors.Is(err, state.ErrNotFound) {
					return err
				}
				return nil
			}
		}),

		c("memory recall ignores the scope", "Memory", func(x *parts) {
			x.memory.OnRecall = func(ctx context.Context, in state.MemoryStore, q state.RecallQuery) ([]state.MemoryItem, error) {
				q.Scope = state.Scope{}
				return in.Recall(ctx, q)
			}
		}),
		c("memory delete does nothing", "Memory", func(x *parts) {
			x.memory.OnDelete = func(context.Context, state.MemoryStore, string) error { return nil }
		}),
		c("deleting a missing memory item succeeds", "Memory", func(x *parts) {
			x.memory.OnDelete = func(ctx context.Context, in state.MemoryStore, id string) error {
				if err := in.Delete(ctx, id); !errors.Is(err, state.ErrNotFound) {
					return err
				}
				return nil
			}
		}),

		c("skill envelope loses its origin", "Envelope", func(x *parts) {
			x.skills.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				out, err := in.Upsert(ctx, sk)
				out.OriginInstanceID = ""
				return out, err
			}
		}),
		c("turns are not stamped", "Envelope", func(x *parts) {
			x.sessions.OnAppendTurn = func(ctx context.Context, in state.SessionStore, tn state.Turn) (state.Turn, error) {
				out, err := in.AppendTurn(ctx, tn)
				out.LastWriterID = ""
				return out, err
			}
		}),
		c("appending a turn leaves the session clock alone", "Envelope", func(x *parts) {
			x.sessions.OnGet = func(ctx context.Context, in state.SessionStore, id string) (state.Session, error) {
				s, err := in.Get(ctx, id)
				s.Envelope = state.Envelope{}
				return s, err
			}
		}),

		c("upserts of one slug create separate records", "Concurrency", func(x *parts) {
			var n atomic.Int64
			x.skills.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				sk.Slug = fmt.Sprintf("%s-%d", sk.Slug, n.Add(1))
				return in.Upsert(ctx, sk)
			}
		}),

		c("a skill delete bypasses the log", "EventSourced", hidden(state.EvSkillDeleted)),
		c("a memory write bypasses the log", "EventSourced", hidden(state.EvMemoryWritten)),
		c("a turn bypasses the log", "EventSourced", hidden(state.EvTurnAppended)),
	})
}
