package chain

import (
	"bytes"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// FuzzVerifyNoPanic feeds arbitrary bytes to every decode and verify entry point.
// The verifier is attacker-facing, so the only acceptable behaviour on hostile
// input is a clean error: it must never panic.
func FuzzVerifyNoPanic(f *testing.F) {
	seed, _ := CanonicalBytes(sampleEvent())
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{0xff})
	f.Add([]byte{0xa1, 0x61, 0x61, 0x61, 0x62}) // a small CBOR map
	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = VerifyCanonical(data)
		_, _ = DecodeCanonical(data)
		_, _ = NewVerifier().VerifyStream([][]byte{data})
		_, _ = LeafInput(data)
	})
}

// FuzzCanonicalStable checks that for any event the encoder can produce, encoding
// is stable and the result verifies as canonical. This is the determinism
// guarantee the whole chain rests on.
func FuzzCanonicalStable(f *testing.F) {
	f.Add("run/x", int64(1), "action", int64(1))
	f.Add("", int64(0), "", int64(-5))
	f.Fuzz(func(t *testing.T, stream string, seq int64, typ string, nanos int64) {
		e := spine.Event{
			Stream:        stream,
			Seq:           seq,
			Time:          time.Unix(0, nanos).UTC(),
			Type:          typ,
			Actor:         spine.ActorAgent,
			SchemaVersion: 1,
			Payload:       map[string]any{},
		}
		if e.Time.IsZero() {
			t.Skip()
		}
		a, err := CanonicalBytes(e)
		if err != nil {
			t.Skip()
		}
		b, err := CanonicalBytes(e)
		if err != nil {
			t.Fatalf("second encode failed: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("encoding is not stable")
		}
		if err := VerifyCanonical(a); err != nil {
			t.Fatalf("self-produced bytes are not canonical: %v", err)
		}
	})
}
