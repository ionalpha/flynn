package modelsource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLedgerRecordGetAndPin(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir)

	if _, ok, err := l.PinnedDigest("hf:Qwen/r"); err != nil || ok {
		t.Fatalf("unpinned source = (%v, %v), want (false, nil)", ok, err)
	}
	rec := Provenance{Key: "hf:Qwen/r", Raw: "hf:Qwen/r", Trust: "semi-trusted", Format: "gguf", Digest: "sha256:abc", FirstSeen: 100, LastVerified: 100}
	if err := l.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Read back through a fresh ledger to cover persistence.
	digest, ok, err := NewLedger(dir).PinnedDigest("hf:Qwen/r")
	if err != nil || !ok || digest != "sha256:abc" {
		t.Fatalf("PinnedDigest = (%q, %v, %v), want sha256:abc", digest, ok, err)
	}
}

func TestLedgerUpdatePreservesFirstSeen(t *testing.T) {
	l := NewLedger(t.TempDir())
	_ = l.Record(Provenance{Key: "k", FirstSeen: 100, LastVerified: 100, Digest: "sha256:a"})
	// A later verification updates LastVerified but the original sighting must remain.
	_ = l.Record(Provenance{Key: "k", LastVerified: 200, Digest: "sha256:a"})
	got, ok, _ := l.Get("k")
	if !ok || got.FirstSeen != 100 || got.LastVerified != 200 {
		t.Fatalf("record = %+v, want FirstSeen 100 LastVerified 200", got)
	}
}

func TestLedgerCorruptFileReadsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewLedger(dir)
	recs, err := l.List()
	if err != nil || len(recs) != 0 {
		t.Fatalf("corrupt ledger = (%v, %v), want empty and no error", recs, err)
	}
	// A write after a corrupt read must succeed and produce a valid file.
	if err := l.Record(Provenance{Key: "k", Digest: "sha256:z"}); err != nil {
		t.Fatalf("Record after corrupt: %v", err)
	}
	if _, ok, _ := NewLedger(dir).Get("k"); !ok {
		t.Fatal("record not persisted after recovering from a corrupt ledger")
	}
}

func TestLedgerListSorted(t *testing.T) {
	l := NewLedger(t.TempDir())
	for _, k := range []string{"c", "a", "b"} {
		_ = l.Record(Provenance{Key: k})
	}
	recs, _ := l.List()
	if len(recs) != 3 || recs[0].Key != "a" || recs[1].Key != "b" || recs[2].Key != "c" {
		t.Fatalf("List not sorted by key: %+v", recs)
	}
}
