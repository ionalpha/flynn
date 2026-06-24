package learn

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

func TestRegrade(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	seed := func(slug, check string, tags ...string) {
		if _, err := skills.Upsert(ctx, state.Skill{Slug: slug, Body: "x", Check: check, Tags: tags}); err != nil {
			t.Fatal(err)
		}
	}
	seed("good", "exit 0", unverifiedTag) // still passes -> re-confirmed and promoted
	seed("bad", "exit 1", verifiedTag)    // now fails -> retired
	seed("nocheck", "")                   // no check -> untouched

	v := NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(t.TempDir())
	})
	res, err := Regrade(ctx, skills, state.Scope{}, v)
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 2 || len(res.Reconfirmed) != 1 || len(res.Retired) != 1 {
		t.Fatalf("regrade result = %+v, want 2 checked / 1 reconfirmed / 1 retired", res)
	}

	good, err := skills.Get(ctx, "good")
	if err != nil {
		t.Fatalf("re-confirmed skill missing: %v", err)
	}
	if !hasTag(good.Tags, verifiedTag) || hasTag(good.Tags, unverifiedTag) {
		t.Fatalf("re-confirmed skill not promoted to verified: %v", good.Tags)
	}
	if _, err := skills.Get(ctx, "bad"); err == nil {
		t.Fatal("a skill whose check now fails should have been retired")
	}
	if _, err := skills.Get(ctx, "nocheck"); err != nil {
		t.Fatalf("a checkless skill should be left untouched: %v", err)
	}
}

func TestRegradeNilVerifierIsNoop(t *testing.T) {
	skills, _ := newStores(t)
	res, err := Regrade(context.Background(), skills, state.Scope{}, nil)
	if err != nil || res.Checked != 0 {
		t.Fatalf("nil verifier = (%+v, %v), want a clean no-op", res, err)
	}
}

func TestRetagVerified(t *testing.T) {
	got := retagVerified([]string{"learned", unverifiedTag})
	if !contains(got, verifiedTag) || contains(got, unverifiedTag) || !contains(got, "learned") {
		t.Fatalf("retag = %v, want learned+verified without unverified", got)
	}
	// Idempotent: already verified stays verified, once.
	twice := retagVerified(retagVerified([]string{verifiedTag}))
	n := 0
	for _, tg := range twice {
		if tg == verifiedTag {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("verified tag duplicated: %v", twice)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
