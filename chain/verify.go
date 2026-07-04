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
// is well formed and in canonical form, all events belong to one stream, and Seq
// strictly increases. It deliberately does NOT prove tamper-evidence: that is
// composed at the record layer, where VerifyRun and VerifyEventProof pair this
// structural check with checkpoint-signature and Merkle-root verification. A Verifier
// is stateless and safe for concurrent use.
type Verifier struct{}

// NewVerifier returns a structural verifier.
func NewVerifier() *Verifier { return &Verifier{} }

// VerifyStream checks a contiguous slice of canonical event byte blobs in Seq order
// and returns the decoded events. It fails closed: the first structural fault stops
// verification and returns a coded error, so a partially valid stream is never
// reported as valid. An empty stream is an error rather than a vacuous pass.
func (v *Verifier) VerifyStream(canonicalEvents [][]byte) ([]spine.Event, error) {
	if len(canonicalEvents) == 0 {
		return nil, fault.New(fault.Terminal, CodeEmptyStream, "chain: empty event stream")
	}
	events := make([]spine.Event, 0, len(canonicalEvents))
	var prev spine.Event
	for i, b := range canonicalEvents {
		// One pass: the canonical-form check decodes the event anyway, so take
		// the decoded event from it instead of decoding the bytes a second time.
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
		// Per-event cryptographic hook. Tamper-evidence for a sealed run is enforced
		// at the record layer: VerifyRun checks the checkpoint signature and rebuilds
		// the signed Merkle root, so this hook is a no-op today. It is the extension
		// point for future per-event checks inside a single stream pass.
		if err := v.verifyCrypto(b, e); err != nil {
			return nil, err
		}
		events = append(events, e)
		prev = e
	}
	return events, nil
}

// verifyCrypto is the per-event cryptographic hook, empty today. Record-level
// verification (VerifyRun, VerifyEventProof) provides tamper-evidence by checking the
// signed checkpoint and rebuilding the Merkle root, so structural verification stands
// alone here. The hook is kept as a method so future per-event state (a keyring, a
// signed root) has a home without changing VerifyStream's shape.
func (v *Verifier) verifyCrypto(canonical []byte, e spine.Event) error {
	_ = canonical
	_ = e
	return nil
}
