package modelformat

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// safetensorsHeader builds the leading bytes of a safetensors file: an 8-byte
// little-endian header length followed by a JSON object.
func safetensorsHeader(jsonLen uint64) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, jsonLen)
	b.WriteString(`{"__metadata__":{}}`)
	return b.Bytes()
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want Format
	}{
		{"gguf", []byte("GGUF\x03\x00\x00\x00"), FormatGGUF},
		{"zip local", []byte("PK\x03\x04rest of zip"), FormatZip},
		{"zip empty", []byte("PK\x05\x06"), FormatZip},
		{"pickle proto2", []byte{0x80, 0x02, 0x7d, 0x71}, FormatPickle},
		{"pickle proto5", []byte{0x80, 0x05, 0x95}, FormatPickle},
		{"safetensors", safetensorsHeader(19), FormatSafetensors},
		{"empty", nil, FormatUnknown},
		{"random text", []byte("not a model file at all"), FormatUnknown},
		{"too short", []byte("GG"), FormatUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.data); got != tc.want {
				t.Fatalf("Detect(%q) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

// TestCheckRefusesCodeExecAndUnknown is the gate: a code-executing or unrecognized
// model file is refused, while the data-only formats pass. This is what stops a
// pickle disguised as a ".bin" from ever reaching a runtime.
func TestCheckRefusesCodeExecAndUnknown(t *testing.T) {
	refused := map[string][]byte{
		"pickle":  {0x80, 0x04, 0x95, 0x00},
		"zip":     []byte("PK\x03\x04............"),
		"unknown": []byte("totally unknown bytes here!"),
	}
	for name, data := range refused {
		f, err := Check(bytes.NewReader(data))
		if err == nil {
			t.Fatalf("%s: a non-data format (%v) must be refused", name, f)
		}
		if f.SafeToParse() {
			t.Fatalf("%s: refused format %v reported safe", name, f)
		}
	}

	allowed := map[string][]byte{
		"gguf":        append([]byte("GGUF"), make([]byte, 16)...),
		"safetensors": safetensorsHeader(19),
	}
	for name, data := range allowed {
		f, err := Check(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: a data-only format must pass, got %v (%v)", name, err, f)
		}
		if !f.SafeToParse() {
			t.Fatalf("%s: passed format %v not reported safe", name, f)
		}
	}
}

// FuzzDetect proves the detector is total: for any leading bytes it returns a format
// without panicking, and a format it calls safe is only ever one of the two data-only
// formats. A code-executing file can never be mislabeled safe.
func FuzzDetect(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("GGUF"), {0x80, 0x02}, []byte("PK\x03\x04"), safetensorsHeader(19), {}, []byte("x"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got := Detect(data)
		if got.SafeToParse() && got != FormatGGUF && got != FormatSafetensors {
			t.Fatalf("Detect(%q) = %v reported safe but is not a data-only format", data, got)
		}
	})
}
