package modelsource

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseAndClassify feeds arbitrary reference strings through Parse and, on success,
// Classify and the format guard, asserting none panic and that a parsed source is never
// classified above its floor (a non-catalog source is never fully trusted). The trust
// decision is security-critical, so it must hold for any input a user could type or paste.
func FuzzParseAndClassify(f *testing.F) {
	for _, s := range []string{
		"", "hf:Qwen/r", "hf:Qwen/r/model.gguf", "https://huggingface.co/a/b/resolve/main/m.gguf",
		"https://x/y.gguf", "/tmp/x.gguf", `C:\x.bin`, "catalog-id", "hf:onlyowner", "hf:::///",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		s, err := Parse(ref, func(string) bool { return ref == "catalog-id" })
		if err != nil {
			return
		}
		if s.Key() == "" {
			t.Fatalf("parsed %q has empty key", ref)
		}
		c := Classify(s, func(o string) bool { return KnownPublisher(o) })
		if s.Kind != KindCatalog && c.Trust == 0 /* sandbox.TrustTrusted */ {
			t.Fatalf("non-catalog source %q classified as fully trusted", ref)
		}
		// The format guard must never panic on whatever file name the source carries.
		_ = CheckRunnableFormat(s.File)
		_ = CheckRunnableFormat(s.Path)
		_ = DetectFormat(ref)
	})
}

// FuzzLedgerLoad feeds arbitrary bytes as the provenance file and asserts the ledger
// never panics or errors: any malformed content reads as empty, and a record written
// afterward persists, so a corrupt ledger can never wedge a run.
func FuzzLedgerLoad(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("[]"))
	f.Add([]byte("not json"))
	f.Add([]byte(`[{"key":"k","digest":"sha256:a"}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "provenance.json"), data, 0o600); err != nil {
			t.Skip()
		}
		l := NewLedger(dir)
		if _, err := l.List(); err != nil {
			t.Fatalf("List on arbitrary content errored: %v", err)
		}
		if err := l.Record(Provenance{Key: "fresh", Digest: "sha256:z"}); err != nil {
			t.Fatalf("Record after arbitrary content errored: %v", err)
		}
		if _, ok, _ := NewLedger(dir).Get("fresh"); !ok {
			t.Fatal("record not persisted after arbitrary prior content")
		}
	})
}
