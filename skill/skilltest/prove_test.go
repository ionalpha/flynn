package skilltest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/conformtest"
	"github.com/ionalpha/flynn/internal/conformtest/faultstate"
	"github.com/ionalpha/flynn/skill/skilltest"
	"github.com/ionalpha/flynn/state"
)

func TestSuiteFailsOnBrokenSkillStore(t *testing.T) {
	c := func(name, want string, brk func(f *faultstate.Skills)) conformtest.Case {
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) {
			skilltest.RunSuite(t, func() state.SkillStore {
				f := &faultstate.Skills{SkillStore: state.NewMemory().Skills()}
				brk(f)
				return f
			})
		}}
	}
	conformtest.Prove(t, []conformtest.Case{
		c("drops the description on write", "Description", func(f *faultstate.Skills) {
			f.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				sk.Description = ""
				return in.Upsert(ctx, sk)
			}
		}),
		c("search skips the description", "Description", func(f *faultstate.Skills) {
			f.OnSearch = func(ctx context.Context, in state.SkillStore, q string, limit int) ([]state.Skill, error) {
				all, err := in.Search(ctx, "", 0)
				var out []state.Skill
				for _, sk := range all {
					hay := sk.Name + " " + sk.Body + " " + strings.Join(sk.Tags, " ")
					if strings.Contains(hay, q) {
						out = append(out, sk)
					}
				}
				return out, err
			}
		}),
		c("update patches instead of replacing", "Description", func(f *faultstate.Skills) {
			f.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				if old, err := in.Get(ctx, sk.Slug); err == nil && sk.Description == "" {
					sk.Description = old.Description
				}
				return in.Upsert(ctx, sk)
			}
		}),
		c("update does not bump the version", "UpsertGetListSearch", func(f *faultstate.Skills) {
			f.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				out, err := in.Upsert(ctx, sk)
				out.Version = 1
				return out, err
			}
		}),
		c("list ignores the scope", "UpsertGetListSearch", func(f *faultstate.Skills) {
			f.OnList = func(ctx context.Context, in state.SkillStore, _ state.Scope) ([]state.Skill, error) {
				return in.Search(ctx, "", 0)
			}
		}),
		c("search ignores the limit", "UpsertGetListSearch", func(f *faultstate.Skills) {
			f.OnSearch = func(ctx context.Context, in state.SkillStore, q string, _ int) ([]state.Skill, error) {
				return in.Search(ctx, q, 0)
			}
		}),
		c("stale writes are accepted", "OptimisticConcurrency", func(f *faultstate.Skills) {
			f.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				sk.SyncVersion = 0
				return in.Upsert(ctx, sk)
			}
		}),
		c("delete does nothing", "Tombstone", func(f *faultstate.Skills) {
			f.OnDelete = func(context.Context, state.SkillStore, string) error { return nil }
		}),
		c("deleting twice succeeds", "Tombstone", func(f *faultstate.Skills) {
			f.OnDelete = func(ctx context.Context, in state.SkillStore, id string) error {
				if err := in.Delete(ctx, id); !errors.Is(err, state.ErrNotFound) {
					return err
				}
				return nil
			}
		}),
		c("deleted skills stay searchable", "Tombstone", func(f *faultstate.Skills) {
			// Remember every skill ever written, so Search can return it after Delete.
			seen := map[string]state.Skill{}
			f.OnUpsert = func(ctx context.Context, in state.SkillStore, sk state.Skill) (state.Skill, error) {
				out, err := in.Upsert(ctx, sk)
				if err == nil {
					seen[out.Slug] = out
				}
				return out, err
			}
			f.OnSearch = func(ctx context.Context, in state.SkillStore, q string, _ int) ([]state.Skill, error) {
				var out []state.Skill
				for _, sk := range seen {
					if strings.Contains(sk.Body, q) {
						out = append(out, sk)
					}
				}
				return out, nil
			}
		}),
	})
}
