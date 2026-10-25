package memorytest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/conformtest"
	"github.com/ionalpha/flynn/internal/conformtest/faultstate"
	"github.com/ionalpha/flynn/memory/memorytest"
	"github.com/ionalpha/flynn/state"
)

// TestSuiteFailsOnBrokenMemoryStore runs the suite against stores that each
// violate one guarantee, and requires the subtest that owns the guarantee to
// fail. Without it the suite is only known to accept correct stores.
func TestSuiteFailsOnBrokenMemoryStore(t *testing.T) {
	c := func(name, want string, brk func(m *faultstate.Memory)) conformtest.Case {
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) {
			memorytest.RunSuite(t, func() state.MemoryStore {
				m := &faultstate.Memory{MemoryStore: state.NewMemory().Memory()}
				brk(m)
				return m
			})
		}}
	}
	// onWrite and onRecall adapt a plain edit of the input or the result to a hook.
	onWrite := func(edit func(*state.MemoryItem)) func(context.Context, state.MemoryStore, state.MemoryItem) (state.MemoryItem, error) {
		return func(ctx context.Context, in state.MemoryStore, it state.MemoryItem) (state.MemoryItem, error) {
			edit(&it)
			return in.Write(ctx, it)
		}
	}
	onQuery := func(edit func(*state.RecallQuery)) func(context.Context, state.MemoryStore, state.RecallQuery) ([]state.MemoryItem, error) {
		return func(ctx context.Context, in state.MemoryStore, q state.RecallQuery) ([]state.MemoryItem, error) {
			edit(&q)
			return in.Recall(ctx, q)
		}
	}
	swallow := func(target error, err error) error {
		if errors.Is(err, target) {
			return nil
		}
		return err
	}

	conformtest.Prove(t, []conformtest.Case{
		c("recall ignores the limit", "WriteRecall", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.Limit = 0 })
		}),
		c("write does not stamp a sync version", "WriteRecall", func(m *faultstate.Memory) {
			m.OnWrite = func(ctx context.Context, in state.MemoryStore, it state.MemoryItem) (state.MemoryItem, error) {
				out, err := in.Write(ctx, it)
				out.SyncVersion = 0
				return out, err
			}
		}),

		c("recall never widens to ancestor scopes", "ScopeResolution", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.IncludeAncestors = false })
		}),

		c("recall ignores the kind filter", "Selectors", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.Kinds = nil })
		}),
		c("recall ignores the lower time bound", "Selectors", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.Since = time.Time{} })
		}),
		c("recall includes the upper time bound", "Selectors", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) {
				if !q.Until.IsZero() {
					q.Until = q.Until.Add(time.Nanosecond)
				}
			})
		}),

		c("recall ignores the score floor", "Relevance", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.MinScore = 0 })
		}),
		c("write persists a caller's score", "Relevance", func(m *faultstate.Memory) {
			m.OnWrite = func(ctx context.Context, in state.MemoryStore, it state.MemoryItem) (state.MemoryItem, error) {
				out, err := in.Write(ctx, it)
				out.Score = it.Score
				return out, err
			}
		}),
		c("recall reports no score", "Relevance", func(m *faultstate.Memory) {
			m.OnRecall = func(ctx context.Context, in state.MemoryStore, q state.RecallQuery) ([]state.MemoryItem, error) {
				got, err := in.Recall(ctx, q)
				for i := range got {
					got[i].Score = 0
				}
				return got, err
			}
		}),

		c("write keeps only the first source", "Provenance", func(m *faultstate.Memory) {
			m.OnWrite = onWrite(func(it *state.MemoryItem) {
				if len(it.Sources) > 1 {
					it.Sources = it.Sources[:1]
				}
			})
		}),

		c("write drops anchors", "Anchors", func(m *faultstate.Memory) {
			m.OnWrite = onWrite(func(it *state.MemoryItem) { it.Anchors = nil })
		}),
		c("recall ignores the anchor filter", "Anchors", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.Anchors = nil })
		}),
		c("write accepts a half-formed anchor", "Anchors", func(m *faultstate.Memory) {
			m.OnWrite = func(ctx context.Context, in state.MemoryStore, it state.MemoryItem) (state.MemoryItem, error) {
				for _, a := range it.Anchors {
					if a.Kind == "" || a.ID == "" {
						return it, nil
					}
				}
				return in.Write(ctx, it)
			}
		}),

		c("recall ignores the subject filter", "Subjects", func(m *faultstate.Memory) {
			m.OnRecall = onQuery(func(q *state.RecallQuery) { q.Subjects = nil })
		}),
		c("write accepts a subject with nothing to key on", "Subjects", func(m *faultstate.Memory) {
			m.OnWrite = onWrite(func(it *state.MemoryItem) {
				switch it.Subject {
				case "---", ".", " ?! ":
					it.Subject = ""
				}
			})
		}),

		c("write drops supersession links", "Supersedes", func(m *faultstate.Memory) {
			m.OnWrite = onWrite(func(it *state.MemoryItem) { it.Supersedes = nil })
		}),

		c("write clears the taint", "Taint", func(m *faultstate.Memory) {
			m.OnWrite = onWrite(func(it *state.MemoryItem) { it.Tainted = false })
		}),
		c("taint is recomputed from user-scheme sources", "Taint", func(m *faultstate.Memory) {
			m.OnWrite = onWrite(func(it *state.MemoryItem) {
				if len(it.Sources) > 0 && it.Sources[0] == "user:operator" {
					it.Tainted = false
				}
			})
		}),

		c("expired items are recalled", "Expiry", func(m *faultstate.Memory) {
			// Store the item without its expiry but report it as written, so the
			// round trip looks right while recall can no longer tell it has lapsed.
			m.OnWrite = func(ctx context.Context, in state.MemoryStore, it state.MemoryItem) (state.MemoryItem, error) {
				expiry := it.ExpiresAt
				it.ExpiresAt = time.Time{}
				out, err := in.Write(ctx, it)
				out.ExpiresAt = expiry
				return out, err
			}
		}),

		c("delete does nothing", "Tombstone", func(m *faultstate.Memory) {
			m.OnDelete = func(context.Context, state.MemoryStore, string) error { return nil }
		}),
		c("deleting twice succeeds", "Tombstone", func(m *faultstate.Memory) {
			m.OnDelete = func(ctx context.Context, in state.MemoryStore, id string) error {
				return swallow(state.ErrNotFound, in.Delete(ctx, id))
			}
		}),

		c("every use counts as organic", "Usage", func(m *faultstate.Memory) {
			m.OnRecordUse = func(ctx context.Context, in state.MemoryStore, id string, o state.UsageOrigin) error {
				if o == state.UsagePrimed {
					o = state.UsageOrganic
				}
				return in.RecordUse(ctx, id, o)
			}
		}),
		c("an unknown use origin is accepted", "Usage", func(m *faultstate.Memory) {
			m.OnRecordUse = func(ctx context.Context, in state.MemoryStore, id string, o state.UsageOrigin) error {
				return swallow(state.ErrInvalid, in.RecordUse(ctx, id, o))
			}
		}),
		c("a push naming an unknown id is accepted", "Usage", func(m *faultstate.Memory) {
			m.OnRecordPush = func(ctx context.Context, in state.MemoryStore, ids []string) error {
				return swallow(state.ErrNotFound, in.RecordPush(ctx, ids))
			}
		}),
		c("a repeated id in one push counts twice", "Usage", func(m *faultstate.Memory) {
			m.OnRecordPush = func(ctx context.Context, in state.MemoryStore, ids []string) error {
				if err := in.RecordPush(ctx, ids); err != nil {
					return err
				}
				return in.RecordPush(ctx, ids[:1])
			}
		}),

		c("every decision reads as a promotion", "Promotions", func(m *faultstate.Memory) {
			m.OnPromote = func(ctx context.Context, in state.MemoryStore, d state.PromotionDecision) (state.MemoryPromotion, error) {
				d.Promoted = true
				return in.Promote(ctx, d)
			}
		}),
		c("an unattributed decision is accepted", "Promotions", func(m *faultstate.Memory) {
			m.OnPromote = func(ctx context.Context, in state.MemoryStore, d state.PromotionDecision) (state.MemoryPromotion, error) {
				if d.By == "" {
					d.By = "nobody"
				}
				return in.Promote(ctx, d)
			}
		}),
	})
}
