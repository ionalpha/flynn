package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ggufMagicLE is "GGUF" as a little-endian uint32, the first four bytes of a GGUF file.
const ggufMagicLE = 0x46554747

// writeMinimalGGUF writes a syntactically valid GGUF header to a temp file: the magic,
// version 3, zero tensors, and the given key/value metadata. Only string values are
// needed here (the chat template), so the encoder handles just that type. It returns the
// file path.
func writeMinimalGGUF(t *testing.T, kvs map[string]string) string {
	t.Helper()
	var b bytes.Buffer
	w32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w64 := func(v uint64) { _ = binary.Write(&b, binary.LittleEndian, v) }
	wstr := func(s string) {
		w64(uint64(len(s)))
		b.WriteString(s)
	}
	w32(ggufMagicLE)
	w32(3)                  // version
	w64(0)                  // tensor count
	w64(uint64(len(kvs)))   // metadata kv count
	const gilTypeString = 8 // GGUF metadata value type for a string
	for k, v := range kvs {
		wstr(k)
		w32(gilTypeString)
		wstr(v)
	}
	path := filepath.Join(t.TempDir(), "weights.gguf")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatalf("write gguf: %v", err)
	}
	return path
}

func TestInspectWeightsTrustedTemplate(t *testing.T) {
	path := writeMinimalGGUF(t, map[string]string{"general.architecture": "qwen2"})
	got := inspectWeights(path, "chatml")
	if !strings.Contains(got, "trusted \"chatml\" template") || strings.Contains(got, "overridden") {
		t.Fatalf("unexpected line for a model with no embedded template: %q", got)
	}
}

func TestInspectWeightsOverridesEmbeddedTemplate(t *testing.T) {
	// A model that ships its own chat template must be reported as overridden, never
	// trusted as the prompt contract.
	path := writeMinimalGGUF(t, map[string]string{"tokenizer.chat_template": "{{ POISONED }}"})
	got := inspectWeights(path, "chatml")
	if !strings.Contains(got, "overridden") || !strings.Contains(got, "chatml") {
		t.Fatalf("an embedded template must be reported as overridden, got %q", got)
	}
}

func TestInspectWeightsNoTrustedTemplate(t *testing.T) {
	path := writeMinimalGGUF(t, nil)
	got := inspectWeights(path, "")
	if !strings.Contains(got, "no trusted chat template") {
		t.Fatalf("a model without a trusted template must be flagged, got %q", got)
	}
}

func TestInspectWeightsUnparsableFileIsCaution(t *testing.T) {
	// A file the hardened reader cannot parse must be a caution, not a crash, and must
	// make clear the runtime is not handed the file.
	path := filepath.Join(t.TempDir(), "junk.gguf")
	if err := os.WriteFile(path, []byte("this is not a gguf file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := inspectWeights(path, "chatml")
	if !strings.Contains(got, "caution") {
		t.Fatalf("an unparsable file must be reported as a caution, got %q", got)
	}
}
