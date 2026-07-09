package state_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// FuzzUnmarshalSnapshot checks snapshot decode is total over any bytes, and that
// decode really is the inverse of MarshalSnapshot as documented. A snapshot blob is
// read back from a host-provided backend across the persistence boundary, so
// malformed bytes must yield a typed error, never a panic. A blob that decodes must
// re-encode to a stable payload: Rebuild restores from whatever MarshalSnapshot
// wrote, so a decode that loses or reorders records would silently rebuild a
// different projection than the one that was captured.
func FuzzUnmarshalSnapshot(f *testing.F) {
	for _, s := range []string{
		"{}", `{"seq":1}`, "null", "", "[]", "not json", `{"seq":"x"}`,
		`{"lastSeq":1,"sessions":[{"ID":"s1"}],"turns":[{"ID":"t1","SessionID":"s1"}]}`,
		`{"turns":[{"ID":"b","SessionID":"s1","Seq":2},{"ID":"a","SessionID":"s1","Seq":1}]}`,
		`{"lastSeq":-9223372036854775808}`,
		`{"slugToID":{"a":"b","c":"d"}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		snap, err := state.UnmarshalSnapshot(payload)
		if err != nil {
			return // a typed error is the correct outcome for a malformed blob
		}
		first, err := state.MarshalSnapshot(snap)
		if err != nil {
			t.Fatalf("MarshalSnapshot failed on a snapshot that decoded cleanly: %v", err)
		}
		again, err := state.UnmarshalSnapshot(first)
		if err != nil {
			t.Fatalf("UnmarshalSnapshot rejected its own MarshalSnapshot output: %v", err)
		}
		second, err := state.MarshalSnapshot(again)
		if err != nil {
			t.Fatalf("MarshalSnapshot failed on a round-tripped snapshot: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("snapshot round-trip is not stable:\n first: %s\nsecond: %s", first, second)
		}
	})
}

// FuzzDecodeRecords checks the record decoders are total over any event payload,
// and that they refuse a payload whose record is absent. Durable backends hand
// these arbitrary map payloads and project whatever comes back. A decoder that
// reported success for a missing record would hand the backend a zero-valued row
// while the in-memory core, which rejects the same event, kept none: the two
// projections of one event would diverge.
func FuzzDecodeRecords(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"session":{"ID":"s1"}}`, `{"turn":{"ID":"t1"}}`, `{"skill":{"ID":"k1"}}`,
		`{"item":{"ID":"i1"}}`, `{"session":123}`, `{"session":null}`, `null`, `{"turn":[]}`,
		`{"session":{"ID":"s1","CreatedAt":"not-a-time"}}`,
		`{"skill":{"ID":"k1","Tags":"not-a-list"}}`,
		`{"session":{},"turn":{},"skill":{},"item":{}}`,
	} {
		f.Add([]byte(s))
	}

	// Each decoder reads exactly one key. Decode must fail when that key is absent
	// or null, and must never panic on any value stored under it.
	decoders := []struct {
		key    string
		decode func(map[string]any) error
	}{
		{"session", func(m map[string]any) error { _, err := state.DecodeSession(m); return err }},
		{"turn", func(m map[string]any) error { _, err := state.DecodeTurn(m); return err }},
		{"skill", func(m map[string]any) error { _, err := state.DecodeSkill(m); return err }},
		{"item", func(m map[string]any) error { _, err := state.DecodeMemoryItem(m); return err }},
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return // an undecodable payload never reaches the decoders
		}
		for _, d := range decoders {
			v, present := m[d.key]
			err := d.decode(m)
			if (!present || v == nil) && err == nil {
				t.Fatalf("Decode(%q) reported success for a payload with no %[1]q record: %s", d.key, raw)
			}
		}
	})
}
