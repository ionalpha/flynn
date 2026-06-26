package serve

import (
	"testing"

	"pgregory.net/rapid"
)

// TestRegistryMatchesModel asserts the registry behaves as a simple keyed set: after any
// sequence of Put and Delete operations, its contents (read back from disk through a
// fresh Registry, so persistence is exercised too) equal a plain in-memory model of the
// same operations. Put is an upsert keyed by model id, Delete removes by id, and neither
// may corrupt or lose unrelated records.
func TestRegistryMatchesModel(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		reg := NewRegistry(dir)
		model := map[string]Record{}

		ops := rapid.IntRange(0, 40).Draw(rt, "ops")
		for range ops {
			id := rapid.SampledFrom(ids).Draw(rt, "id")
			if rapid.Bool().Draw(rt, "delete") {
				delete(model, id)
				if err := reg.Delete(id); err != nil {
					rt.Fatalf("Delete(%q): %v", id, err)
				}
				continue
			}
			rec := Record{
				ModelID:   id,
				PID:       rapid.IntRange(1, 1<<20).Draw(rt, "pid"),
				Port:      rapid.IntRange(1, 65535).Draw(rt, "port"),
				BaseURL:   "http://127.0.0.1/v1",
				Runtime:   rapid.SampledFrom([]string{"llama.cpp", "ollama"}).Draw(rt, "rt"),
				StartedAt: rapid.Int64Range(0, 1<<40).Draw(rt, "started"),
			}
			model[id] = rec
			if err := reg.Put(rec); err != nil {
				rt.Fatalf("Put(%q): %v", id, err)
			}
		}

		// Read back through a fresh registry on the same path, so the assertion covers
		// what was persisted, not just in-process state.
		got, err := NewRegistry(dir).List()
		if err != nil {
			rt.Fatalf("List: %v", err)
		}
		if len(got) != len(model) {
			rt.Fatalf("registry has %d records, model has %d", len(got), len(model))
		}
		for _, rec := range got {
			want, ok := model[rec.ModelID]
			if !ok {
				rt.Fatalf("registry has unexpected id %q", rec.ModelID)
			}
			if rec != want {
				rt.Fatalf("record for %q = %+v, want %+v", rec.ModelID, rec, want)
			}
		}
		// List is sorted by id; verify the order is stable and ascending.
		for i := 1; i < len(got); i++ {
			if got[i-1].ModelID > got[i].ModelID {
				rt.Fatalf("List not sorted: %q before %q", got[i-1].ModelID, got[i].ModelID)
			}
		}
	})
}
