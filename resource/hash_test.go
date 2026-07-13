package resource

import (
	"encoding/json"
	"testing"
)

// TestHashHandlesEmptyAndOpaqueSpecs covers the two specs canonicalization cannot
// re-encode: an absent spec (a kind whose desired state is empty) and one whose
// bytes are not JSON at all. Both must still hash, deterministically, and must not
// collide with each other or with a real spec, so no record is ever unhashable.
func TestHashHandlesEmptyAndOpaqueSpecs(t *testing.T) {
	base := Resource{APIVersion: "test/v1", Kind: "W", Name: "a"}

	empty, err := Hash(base)
	if err != nil {
		t.Fatalf("hash an empty spec: %v", err)
	}
	if again, _ := Hash(base); again != empty {
		t.Fatal("hashing an empty spec must be deterministic")
	}

	opaque := base
	opaque.Spec = []byte("this is not json")
	oh, err := Hash(opaque)
	if err != nil {
		t.Fatalf("hash an opaque spec: %v", err)
	}
	if again, _ := Hash(opaque); again != oh {
		t.Fatal("hashing an opaque spec must be deterministic")
	}
	if oh == empty {
		t.Fatal("an opaque spec must not hash the same as an absent one")
	}

	// The spec hash follows the same rules and stays independent of status.
	sh, err := SpecHash(opaque)
	if err != nil {
		t.Fatalf("spec hash an opaque spec: %v", err)
	}
	withStatus := opaque
	withStatus.Status = []byte(`{"phase":"ready"}`)
	sh2, err := SpecHash(withStatus)
	if err != nil {
		t.Fatal(err)
	}
	if sh != sh2 {
		t.Fatal("status must not affect the spec hash")
	}
	if h, _ := Hash(withStatus); h == oh {
		t.Fatal("status must affect the content hash")
	}
}

// TestHashesMatchesTheIndividualHashes proves the combined helper (which
// canonicalizes the spec once) returns exactly what the two single-purpose
// functions return, so a stamped record's stored hashes are the ones a reader
// recomputes.
func TestHashesMatchesTheIndividualHashes(t *testing.T) {
	for _, r := range []Resource{
		{APIVersion: "test/v1", Kind: "W", Name: "a"},
		{APIVersion: "test/v1", Kind: "W", Name: "a", Spec: json.RawMessage(`{"b":2,"a":1}`)},
		{APIVersion: "test/v1", Kind: "W", Name: "a", Spec: []byte("opaque"), Status: json.RawMessage(`{"n":1}`)},
	} {
		content, spec, err := Hashes(r)
		if err != nil {
			t.Fatalf("Hashes: %v", err)
		}
		wantContent, err := Hash(r)
		if err != nil {
			t.Fatal(err)
		}
		wantSpec, err := SpecHash(r)
		if err != nil {
			t.Fatal(err)
		}
		if content != wantContent || spec != wantSpec {
			t.Fatalf("Hashes disagreed with Hash/SpecHash for %+v", r)
		}
	}
}

// TestHashExcludesOwnerReferences locks that owner references are lifecycle
// metadata, not content: attaching an owner to a resource must not change its
// content hash, so dedup, provenance, and "which version produced this" stay keyed
// on what the resource is, not who owns it.
func TestHashExcludesOwnerReferences(t *testing.T) {
	base := Resource{
		APIVersion: "test.ionagent.io/v1",
		Kind:       "Widget",
		Name:       "w",
		Spec:       json.RawMessage(`{"size":"m"}`),
	}
	owned := base
	owned.OwnerReferences = []OwnerReference{
		{APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "parent", ID: "parent-id", Controller: true},
	}

	h1, err := Hash(base)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	h2, err := Hash(owned)
	if err != nil {
		t.Fatalf("hash owned: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("owner references must not affect the content hash:\n base  = %s\n owned = %s", h1, h2)
	}

	// Sanity: a real content change still moves the hash, so the test is not
	// passing because Hash ignores everything.
	changed := base
	changed.Spec = json.RawMessage(`{"size":"l"}`)
	if h3, _ := Hash(changed); h3 == h1 {
		t.Fatal("a spec change must change the content hash")
	}
}
