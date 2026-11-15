package chain

import (
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// Structural failure codes, matching the published Provetrail conformance registry.
const (
	CodeEmptyStream     = "chain.empty"
	CodeStreamMismatch  = "chain.stream_mismatch"
	CodeNonMonotonicSeq = "chain.non_monotonic_seq"
)

// Verifier performs structural verification of a canonical event stream: each event
// is well formed and canonical, all events belong to one stream, and Seq strictly
// increases. It does not prove tamper-evidence: [VerifyRun] and [VerifyEventProof]
// pair it with checkpoint-signature and Merkle-root checks. A Verifier is
// stateless and safe for concurrent use.
type Verifier struct{}

// NewVerifier returns a structural verifier.
func NewVerifier() *Verifier { return &Verifier{} }

// VerifyStream fails closed on the first structural fault with a coded error,
// so a partially valid stream is never reported valid. An empty stream is an
// error.
func (v *Verifier) VerifyStream(canonicalEvents [][]byte) ([]spine.Event, error) {
	if len(canonicalEvents) == 0 {
		return nil, fault.New(fault.Terminal, CodeEmptyStream, "chain: empty event stream")
	}
	events := make([]spine.Event, 0, len(canonicalEvents))
	var prev spine.Event
	for i, b := range canonicalEvents {
		// The canonical-form check already decodes; reuse its result.
		e, err := verifyCanonical(b)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			if e.Stream != prev.Stream {
				return nil, fault.New(fault.Terminal, CodeStreamMismatch,
					"chain: stream changed within a single stream verification")
			}
			if e.Seq <= prev.Seq {
				return nil, fault.New(fault.Terminal, CodeNonMonotonicSeq,
					"chain: event Seq is not strictly increasing")
			}
		}

		if err := v.verifyCrypto(b, e); err != nil {
			return nil, err
		}
		events = append(events, e)
		prev = e
	}
	return events, nil
}

// verifyCrypto is a no-op hook: [VerifyRun] and [VerifyEventProof] provide
// tamper-evidence by checking the checkpoint and rebuilding the root.
func (v *Verifier) verifyCrypto(canonical []byte, e spine.Event) error {
	_ = canonical
	_ = e
	return nil
}
