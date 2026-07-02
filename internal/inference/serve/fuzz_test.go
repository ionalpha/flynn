package serve

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzRegistryLoad feeds arbitrary bytes as the on-disk registry file and asserts the
// registry never panics and always presents a usable view: it either parses the records
// or, for anything malformed, reports an empty registry rather than failing. A corrupt
// registry must never wedge the model runtime, and a Put after a corrupt read must
// succeed and leave the file valid.
func FuzzRegistryLoad(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("[]"))
	f.Add([]byte("not json"))
	f.Add([]byte(`[{"modelID":"m","pid":5,"port":9000,"baseURL":"http://127.0.0.1:9000/v1"}]`))
	f.Add([]byte(`[{"modelID":`))
	f.Add([]byte(`{"modelID":"m"}`)) // an object, not the expected array

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "servers.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skip()
		}
		reg := NewRegistry(dir)

		// List must never panic or error on arbitrary content.
		recs, err := reg.List()
		if err != nil {
			t.Fatalf("List on arbitrary content errored: %v", err)
		}
		// Get reuses List and must be equally safe.
		if _, _, err := reg.Get("m"); err != nil {
			t.Fatalf("Get on arbitrary content errored: %v", err)
		}
		_ = recs

		// A Put after any prior content must succeed and yield a registry that reads back
		// with the new record present, proving a corrupt file cannot wedge writes.
		rec := Record{ModelID: "fresh", PID: 7, Port: 9100, BaseURL: "http://127.0.0.1:9100/v1"}
		if err := reg.Put(rec); err != nil {
			t.Fatalf("Put after arbitrary content errored: %v", err)
		}
		got, ok, err := NewRegistry(dir).Get("fresh")
		if err != nil {
			t.Fatalf("Get after Put errored: %v", err)
		}
		if !ok || got.PID != 7 {
			t.Fatalf("record not persisted after Put over arbitrary content: ok=%v rec=%+v", ok, got)
		}
	})
}
