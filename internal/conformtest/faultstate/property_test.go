package faultstate_test

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/conformtest/faultstate"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// A wrapper with no hook set is transparent: whatever is written through it
// reads back identically from the store it wraps. This is what lets a proving
// case attribute a suite failure to the one hook it installed.
func TestProp_UnhookedWrappersAreTransparent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		inner := state.NewMemory()
		skills := &faultstate.Skills{SkillStore: inner.Skills()}
		memory := &faultstate.Memory{MemoryStore: inner.Memory()}

		sk, err := skills.Upsert(ctx, testkit.SkillGen().Draw(rt, "skill"))
		if err != nil {
			rt.Fatalf("upsert through the wrapper: %v", err)
		}
		direct, err := inner.Skills().Get(ctx, sk.ID)
		if err != nil || direct.Body != sk.Body || direct.Version != sk.Version {
			rt.Fatalf("store holds (%+v, %v), wrapper returned %+v", direct, err, sk)
		}

		item, err := memory.Write(ctx, state.MemoryItem{Kind: "fact", Content: rapid.String().Draw(rt, "content")})
		if err != nil {
			rt.Fatalf("write through the wrapper: %v", err)
		}
		if err := memory.Delete(ctx, item.ID); err != nil {
			rt.Fatalf("delete through the wrapper: %v", err)
		}
		if err := inner.Memory().Delete(ctx, item.ID); !errors.Is(err, state.ErrNotFound) {
			rt.Fatalf("the wrapper's delete did not reach the store: %v", err)
		}
	})
}

// A hook replaces the call it names and nothing else: the call does not reach
// the wrapped store unless the hook forwards it.
func TestHookReplacesOnlyItsCall(t *testing.T) {
	ctx := context.Background()
	inner := state.NewMemory()
	skills := &faultstate.Skills{
		SkillStore: inner.Skills(),
		OnDelete:   func(context.Context, state.SkillStore, string) error { return nil },
	}
	if _, err := skills.Upsert(ctx, state.Skill{Slug: "a", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := skills.Delete(ctx, "a"); err != nil {
		t.Fatalf("hooked Delete = %v, want the hook's nil", err)
	}
	if _, err := inner.Skills().Get(ctx, "a"); err != nil {
		t.Fatalf("the hooked Delete reached the store: %v", err)
	}
}

// Provider serves the stores it is given, falls back to the wrapped provider's
// for the ones it is not, and exposes a log only through [faultstate.Logged].
func TestProviderServesOverridesAndFallsBack(t *testing.T) {
	inner := state.NewMemory()
	override := &faultstate.Skills{SkillStore: inner.Skills()}
	name := "broken"
	p := &faultstate.Provider{Provider: inner, SkillStore: override, ProviderName: &name}

	if p.Skills() != state.SkillStore(override) || p.Name() != "broken" {
		t.Fatal("Provider did not serve its overrides")
	}
	if p.Memory() != inner.Memory() || p.Sessions() != inner.Sessions() {
		t.Fatal("Provider did not fall back to the wrapped provider's stores")
	}
	if _, ok := any(p).(interface{ Log() spine.Log }); ok {
		t.Fatal("a Provider must not claim an event log")
	}
	boom := errors.New("boom")
	p.CloseErr = boom
	if err := p.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want %v", err, boom)
	}
}
