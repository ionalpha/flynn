package resource_test

import (
	"context"
	"encoding/json"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

const (
	propAPIVersion = "test.ionagent.io/v1"
	propKind       = "Doc"
)

// propRegistry registers an unconstrained "Doc" kind, so the property tests focus
// on store, hash, and replay invariants (admission is covered by the conformance
// suite) and any generated spec is admitted.
func propRegistry(t rapid.TB) *resource.Registry {
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{APIVersion: propAPIVersion, Name: propKind}); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return reg
}

func nameGen() *rapid.Generator[string] { return rapid.SampledFrom([]string{"n0", "n1", "n2", "n3"}) }
func valueGen() *rapid.Generator[int]   { return rapid.IntRange(0, 9) }
func labelsGen() *rapid.Generator[map[string]string] {
	return rapid.MapOfN(
		rapid.SampledFrom([]string{"tier", "env"}),
		rapid.SampledFrom([]string{"x", "y", "z"}),
		0, 2,
	)
}

func docResource(rt *rapid.T) resource.Resource {
	spec, _ := json.Marshal(map[string]any{"v": valueGen().Draw(rt, "v")})
	return resource.Resource{
		APIVersion: propAPIVersion,
		Kind:       propKind,
		Name:       nameGen().Draw(rt, "name"),
		Labels:     labelsGen().Draw(rt, "labels"),
		Spec:       spec,
	}
}

// A put resource is retrievable by name and by id, with version 1, a stamped
// envelope, and a non-empty content hash.
func TestProp_PutRoundtrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		s := resource.NewMemory(propRegistry(rt))
		r := docResource(rt)

		saved, err := s.Put(ctx, r)
		if err != nil {
			rt.Fatalf("put: %v", err)
		}
		if saved.ID == "" || saved.Version != 1 || saved.SyncVersion != 1 || saved.ContentHash == "" {
			rt.Fatalf("create envelope wrong: %+v", saved.Envelope)
		}
		byName, err := s.Get(ctx, propKind, resource.Scope{}, r.Name)
		if err != nil {
			rt.Fatalf("get by name: %v", err)
		}
		byID, err := s.GetByID(ctx, saved.ID)
		if err != nil {
			rt.Fatalf("get by id: %v", err)
		}
		if byName.ID != saved.ID || byID.ID != saved.ID {
			rt.Fatalf("roundtrip id mismatch: name=%q id=%q saved=%q", byName.ID, byID.ID, saved.ID)
		}
		if byName.ContentHash != saved.ContentHash {
			rt.Fatalf("roundtrip hash mismatch")
		}
	})
}

// The content hash is deterministic, ignores volatile envelope fields, and tracks
// content changes.
func TestProp_HashContentAddressed(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		r := docResource(rt)
		h1, err := resource.Hash(r)
		if err != nil {
			rt.Fatalf("hash: %v", err)
		}
		if h2, _ := resource.Hash(r); h1 != h2 {
			rt.Fatal("hash is not deterministic")
		}
		// Volatile envelope churn must not change the hash.
		churned := r
		churned.SyncVersion = int64(rapid.IntRange(1, 1000).Draw(rt, "sv"))
		churned.Version = int64(rapid.IntRange(1, 1000).Draw(rt, "v"))
		churned.LastWriterID = "someone-else"
		if h3, _ := resource.Hash(churned); h1 != h3 {
			rt.Fatal("envelope churn changed the content hash")
		}
		// A different spec must change the hash.
		changed := r
		changed.Spec = json.RawMessage(`{"v":99,"extra":true}`)
		if h4, _ := resource.Hash(changed); h1 == h4 {
			rt.Fatal("different content did not change the hash")
		}
	})
}

type docOp struct {
	name string
	del  bool
	val  int
}

// For any sequence of puts and deletes, a store folded purely from the event log
// reproduces the live store exactly: the no-bypass invariant holds under random
// histories, and the log is authoritative.
func TestProp_ReplayEquivalence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		reg := propRegistry(rt)
		s := resource.NewMemory(reg)

		ops := rapid.SliceOfN(rapid.Custom(func(rt *rapid.T) docOp {
			return docOp{
				name: nameGen().Draw(rt, "name"),
				del:  rapid.Bool().Draw(rt, "del"),
				val:  valueGen().Draw(rt, "val"),
			}
		}), 1, 30).Draw(rt, "ops")

		for _, op := range ops {
			if op.del {
				_ = s.Delete(ctx, propKind, resource.Scope{}, op.name)
				continue
			}
			spec, _ := json.Marshal(map[string]any{"v": op.val})
			_, _ = s.Put(ctx, resource.Resource{APIVersion: propAPIVersion, Kind: propKind, Name: op.name, Spec: spec})
		}

		el, ok := s.(interface{ Log() spine.Log })
		if !ok {
			rt.Fatal("memory store should expose Log()")
		}
		replayed, err := resource.Replay(ctx, el.Log(), reg)
		if err != nil {
			rt.Fatalf("replay: %v", err)
		}

		live, _ := s.List(ctx, propKind, resource.Scope{}, nil)
		rep, _ := replayed.List(ctx, propKind, resource.Scope{}, nil)
		if len(live) != len(rep) {
			rt.Fatalf("replayed list len %d != live %d", len(rep), len(live))
		}
		for i := range live {
			if live[i].ID != rep[i].ID || live[i].Name != rep[i].Name ||
				live[i].ContentHash != rep[i].ContentHash || live[i].Version != rep[i].Version {
				rt.Fatalf("replayed resource %d differs: live=%+v rep=%+v", i, live[i], rep[i])
			}
		}
	})
}
