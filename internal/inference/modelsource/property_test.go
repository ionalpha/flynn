package modelsource

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/sandbox"
)

// TestClassifyNeverBelowFloor asserts the core safety invariant: only a catalog entry is
// ever trusted, and a hub model is trusted no higher than semi and only when its
// publisher is recognized. No generated source can be classified more trusting than it
// has earned, so the containment a run requires never silently drops.
func TestClassifyNeverBelowFloor(t *testing.T) {
	owners := []string{"Qwen", "rando", "meta-llama", "", "x/y"}
	rapid.Check(t, func(rt *rapid.T) {
		kind := Kind(rapid.IntRange(0, 3).Draw(rt, "kind"))
		owner := rapid.SampledFrom(owners).Draw(rt, "owner")
		s := Source{Kind: kind, Owner: owner, Repo: "r", CatalogID: "c", URL: "https://x/y.gguf", Path: "/p.gguf"}
		known := func(o string) bool { return o == "Qwen" || o == "meta-llama" }

		got := Classify(s, known)
		switch kind {
		case KindCatalog:
			if got.Trust != sandbox.TrustTrusted {
				rt.Fatalf("catalog must be trusted, got %v", got.Trust)
			}
		case KindHuggingFace:
			// A hub model is semi only for a recognized publisher, untrusted otherwise.
			// It may never be classified trusted.
			if got.Trust == sandbox.TrustTrusted {
				rt.Fatalf("a hub model must never be fully trusted")
			}
			if known(owner) && got.Trust != sandbox.TrustSemi {
				rt.Fatalf("recognized publisher must be semi-trusted, got %v", got.Trust)
			}
			if !known(owner) && got.Trust != sandbox.TrustUntrusted {
				rt.Fatalf("unrecognized publisher must be untrusted, got %v", got.Trust)
			}
		default:
			// A raw URL or a local file is always untrusted.
			if got.Trust != sandbox.TrustUntrusted {
				rt.Fatalf("kind %d must be untrusted, got %v", kind, got.Trust)
			}
		}
	})
}

// TestParseNeverPanics asserts Parse handles any input string without panicking and that
// a successful parse round-trips through a non-empty Key.
func TestParseNeverPanics(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ref := rapid.String().Draw(rt, "ref")
		s, err := Parse(ref, func(string) bool { return false })
		if err != nil {
			return
		}
		if s.Key() == "" {
			rt.Fatalf("a parsed source must have a non-empty key: %+v", s)
		}
	})
}
