package resource

import (
	"encoding/json"
	"testing"
)

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
