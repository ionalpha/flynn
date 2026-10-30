package gguf

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// intKV is a metadata entry whose value is the little-endian encoding of raw, at the
// width the type tag implies.
type intKV struct {
	key string
	typ uint32
	raw uint64
}

func buildIntGGUF(t *testing.T, arch string, kvs []intKV) []byte {
	t.Helper()
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	wstr := func(s string) { w(uint64(len(s))); b.WriteString(s) }
	w(uint32(ggufMagic))
	w(uint32(3))
	w(uint64(0))
	w(uint64(len(kvs) + 1))
	wstr("general.architecture")
	w(typeString)
	wstr(arch)
	for _, kv := range kvs {
		wstr(kv.key)
		w(kv.typ)
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], kv.raw)
		size, _ := fixedSize(kv.typ)
		b.Write(raw[:size])
	}
	return b.Bytes()
}

func TestReadMetadataKeepsIntegers(t *testing.T) {
	data := buildIntGGUF(t, "gptoss", []intKV{
		{"gptoss.context_length", typeUint32, 131072},
		{"a.u8", typeUint8, 200},
		{"a.u16", typeUint16, 60000},
		{"a.u64", typeUint64, 1 << 40},
		{"a.i8", typeInt8, 100},
		{"a.i16", typeInt16, 30000},
		{"a.i32", typeInt32, 70000},
		{"a.i64", typeInt64, 1 << 40},
		{"a.float", typeFloat32, 0x3f800000},
		{"a.bool", typeBool, 1},
	})
	meta, err := ReadMetadata(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := meta.ContextLength(); got != 131072 {
		t.Errorf("ContextLength = %d", got)
	}
	for key, want := range map[string]uint64{
		"a.u8": 200, "a.u16": 60000, "a.u64": 1 << 40,
		"a.i8": 100, "a.i16": 30000, "a.i32": 70000, "a.i64": 1 << 40,
	} {
		if got, ok := meta.Uint(key); !ok || got != want {
			t.Errorf("Uint(%s) = %d, %v; want %d", key, got, ok, want)
		}
	}
	for _, key := range []string{"a.float", "a.bool", "missing"} {
		if _, ok := meta.Uint(key); ok {
			t.Errorf("Uint(%s) should be absent", key)
		}
	}
}

// A negative signed value must read as absent: wrapped to uint64 it would pass for a
// context length of billions of tokens.
func TestReadMetadataDropsNegativeIntegers(t *testing.T) {
	data := buildIntGGUF(t, "llama", []intKV{
		{"llama.context_length", typeInt32, uint64(0xFFFFFFFF)},
		{"a.i8", typeInt8, 0x80},
		{"a.i64", typeInt64, ^uint64(0)},
	})
	meta, err := ReadMetadata(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := meta.ContextLength(); got != 0 {
		t.Errorf("ContextLength = %d, want 0 for a negative value", got)
	}
	for _, key := range []string{"a.i8", "a.i64"} {
		if _, ok := meta.Uint(key); ok {
			t.Errorf("Uint(%s) kept a negative value", key)
		}
	}
}

func TestContextLengthNeedsArchitecture(t *testing.T) {
	var m Metadata
	if got := m.ContextLength(); got != 0 {
		t.Errorf("ContextLength without architecture = %d", got)
	}
}

func TestReadMetadataTruncatedInteger(t *testing.T) {
	data := buildIntGGUF(t, "llama", []intKV{{"llama.context_length", typeUint64, 4096}})
	if _, err := ReadMetadata(bytes.NewReader(data[:len(data)-3])); err == nil {
		t.Fatal("want an error for a truncated integer")
	}
}
