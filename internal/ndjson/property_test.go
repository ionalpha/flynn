package ndjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestWrittenFramesReadBackIntact is the framing round trip: whatever strings a record
// carries, including U+2028, U+2029, controls and a LF inside a value, the writer emits one
// line per record and the reader returns the same records however the bytes are chunked.
func TestWrittenFramesReadBackIntact(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		records := rapid.SliceOfN(rapid.MapOfN(rapid.String(), rapid.String(), 0, 6), 0, 8).Draw(rt, "records")
		chunk := rapid.IntRange(1, 64).Draw(rt, "chunk")

		var buf bytes.Buffer
		w := NewWriter(&buf)
		for _, rec := range records {
			if err := w.WriteFrame(rec); err != nil {
				rt.Fatalf("write: %v", err)
			}
		}
		if got := strings.Count(buf.String(), "\n"); got != len(records) {
			rt.Fatalf("%d records produced %d line feeds", len(records), got)
		}

		rd := NewReader(&chunked{b: buf.Bytes(), n: chunk})
		for i, want := range records {
			line, err := rd.Next()
			if err != nil {
				rt.Fatalf("record %d: %v", i, err)
			}
			var got map[string]string
			if err := json.Unmarshal(line, &got); err != nil {
				rt.Fatalf("record %d does not decode: %v", i, err)
			}
			if len(got) == 0 && len(want) == 0 {
				continue
			}
			if !reflect.DeepEqual(got, want) {
				rt.Fatalf("record %d: got %v, want %v", i, got, want)
			}
		}
		if _, err := rd.Next(); err == nil {
			rt.Fatal("reader returned a record after the last one")
		}
	})
}
