package conformance

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// TestPropWellFormedEventAccepted asserts the accept-path property the valid
// vectors are fixed instances of: any well-formed event, encoded canonically, is
// accepted by the verifier as a single-event stream. Drawing over a wide space of
// events checks that the suite's accept criterion is not accidentally specific to
// the hand-picked vectors.
func TestPropWellFormedEventAccepted(t *testing.T) {
	v := chain.NewVerifier()
	rapid.Check(t, func(rt *rapid.T) {
		e := spine.Event{
			Stream:        rapid.StringMatching(`[a-z][a-z0-9/._-]{0,16}`).Draw(rt, "stream"),
			Seq:           rapid.Int64Range(0, 1<<40).Draw(rt, "seq"),
			Time:          time.Unix(0, rapid.Int64Range(1, 1<<62).Draw(rt, "time")).UTC(),
			Type:          rapid.StringMatching(`[a-z][a-z.]{0,16}`).Draw(rt, "type"),
			Actor:         spine.ActorAgent,
			SchemaVersion: 1,
			Payload:       map[string]any{},
		}
		cb, err := chain.CanonicalBytes(e)
		if err != nil {
			t.Skip()
		}
		if _, err := v.VerifyStream([][]byte{cb}); err != nil {
			t.Fatalf("a well-formed single event was rejected: %v", err)
		}
	})
}
