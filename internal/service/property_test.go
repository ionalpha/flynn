package service

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/resource"
)

// TestPropServiceRoundTrip asserts that any valid service written to the store is
// retrievable with its spec and status preserved, and appears exactly once in the
// listing. This is the invariant the deploy path relies on: a registered workload is
// faithfully recorded and addressable by name.
func TestPropServiceRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		reg := resource.NewRegistry()
		if err := RegisterKind(reg); err != nil {
			rt.Fatalf("register kind: %v", err)
		}
		s := NewStore(resource.NewMemory(reg))
		ctx := context.Background()

		name := rapid.StringMatching(`[a-z][a-z0-9-]{0,20}`).Draw(rt, "name")
		provider := rapid.SampledFrom([]string{"cloudflare", "vercel", "render", "hetzner"}).Draw(rt, "provider")
		target := rapid.SampledFrom([]Target{"", TargetStaticSite, TargetContainer, TargetVPS}).Draw(rt, "target")
		url := rapid.SampledFrom([]string{"", "https://x.example", "https://y.example/app"}).Draw(rt, "url")
		desired := rapid.SampledFrom([]DesiredState{"", StateRunning, StateStopped}).Draw(rt, "desired")

		spec := Spec{Provider: provider, Target: target, URL: url, DesiredState: desired}
		status := Status{Phase: "deployed", ObservedURL: url}
		if _, err := s.Put(ctx, name, spec, status); err != nil {
			rt.Fatalf("put: %v", err)
		}

		got, err := s.Get(ctx, name)
		if err != nil {
			rt.Fatalf("get: %v", err)
		}
		if got.Spec.Provider != provider || got.Spec.Target != target || got.Spec.URL != url || got.Spec.DesiredState != desired {
			rt.Fatalf("spec not preserved: put %+v got %+v", spec, got.Spec)
		}
		if got.Status.Phase != "deployed" || got.Status.ObservedURL != url {
			rt.Fatalf("status not preserved: %+v", got.Status)
		}

		list, err := s.List(ctx)
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		count := 0
		for _, svc := range list {
			if svc.Name == name {
				count++
			}
		}
		if count != 1 {
			rt.Fatalf("expected exactly one service named %q, got %d", name, count)
		}
	})
}
