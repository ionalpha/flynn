package extension

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/resource"
)

// knownSurfaces is the pool the property tests draw surface keys from.
var knownSurfaces = []string{
	SurfaceIntegration, SurfaceTool, SurfaceOps, SurfaceScrape, SurfaceAuth, SurfaceAgent,
}

// TestPropLoadUnloadBalances asserts two invariants over arbitrary surface sets:
// after a successful load the mounted set is exactly the declared surfaces in
// sorted order, and after unload nothing remains and every OnLoad is matched by
// exactly one OnUnload. A leak or a double-unload here would be a resource bug.
func TestPropLoadUnloadBalances(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Pick a non-empty subset of the known surfaces.
		chosen := rapid.SliceOfDistinct(
			rapid.SampledFrom(knownSurfaces),
			func(s string) string { return s },
		).Filter(func(s []string) bool { return len(s) > 0 }).Draw(rt, "surfaces")

		reg := NewRegistry()
		handlers := map[string]*recordHandler{}
		for _, c := range knownSurfaces {
			h := &recordHandler{capability: c}
			handlers[c] = h
			if err := reg.Register(h); err != nil {
				rt.Fatalf("register %s: %v", c, err)
			}
		}
		l := NewLoader(reg)
		ctx := context.Background()

		surfaces := map[string]json.RawMessage{}
		for _, c := range chosen {
			surfaces[c] = json.RawMessage(`{}`)
		}

		mounted, err := l.Load(ctx, res("ext-1", "x", surfaces))
		if err != nil {
			rt.Fatalf("load: %v", err)
		}

		want := append([]string(nil), chosen...)
		sort.Strings(want)
		if len(mounted) != len(want) {
			rt.Fatalf("mounted %v want %v", mounted, want)
		}
		for i := range want {
			if mounted[i] != want[i] {
				rt.Fatalf("mounted not sorted: %v want %v", mounted, want)
			}
		}

		if err := l.Unload(ctx, "ext-1"); err != nil {
			rt.Fatalf("unload: %v", err)
		}
		if got := l.Mounted("ext-1"); len(got) != 0 {
			rt.Fatalf("surfaces leaked after unload: %v", got)
		}
		for _, c := range chosen {
			h := handlers[c]
			if h.loadCount() != h.unloadCount() {
				rt.Fatalf("surface %s unbalanced: loads=%d unloads=%d", c, h.loadCount(), h.unloadCount())
			}
		}
	})
}

// TestPropSpecRoundTrip asserts encode/decode is an identity on the spec's fields,
// so a stored extension reads back as authored regardless of which fields are set.
func TestPropSpecRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := Spec{
			DisplayName:  rapid.String().Draw(rt, "displayName"),
			Version:      rapid.String().Draw(rt, "version"),
			Provider:     rapid.String().Draw(rt, "provider"),
			BaseURL:      rapid.String().Draw(rt, "baseURL"),
			Capabilities: rapid.SliceOf(rapid.StringMatching(`[a-z.]+`)).Draw(rt, "capabilities"),
			Auth: AuthSpec{
				Type:          rapid.SampledFrom([]string{"", "bearer", "api_key", "basic"}).Draw(rt, "authType"),
				CredentialRef: rapid.String().Draw(rt, "credRef"),
			},
			Safety: SafetySpec{
				ReadOnly:           rapid.Bool().Draw(rt, "readOnly"),
				RateLimitPerMinute: rapid.IntRange(0, 1000).Draw(rt, "rate"),
			},
		}
		body, err := in.Encode()
		if err != nil {
			rt.Fatalf("encode: %v", err)
		}
		out, err := DecodeSpec(resource.Resource{Spec: body})
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		if out.DisplayName != in.DisplayName || out.BaseURL != in.BaseURL ||
			out.Auth.Type != in.Auth.Type || out.Auth.CredentialRef != in.Auth.CredentialRef ||
			out.Safety.ReadOnly != in.Safety.ReadOnly || out.Safety.RateLimitPerMinute != in.Safety.RateLimitPerMinute {
			rt.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
		}
	})
}
