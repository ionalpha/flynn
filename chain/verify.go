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
// strictly increases. It deliberately does NOT prove tamper-evidence; the
// cryptographic checks are a separate slot the tamper-evident spine fills (see
// verifyCrypto). A Verifier is stateless and safe for concurrent use.
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
		if err := VerifyCanonical(b); err != nil {
			return nil, err
		}
		e, err := DecodeCanonical(b)
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
		// Cryptographic verification slot. The tamper-evident spine fills this with
		// hash-chain, Merkle inclusion, and signed-root checks. It is a no-op here:
		// this Verifier proves STRUCTURE only and must not be relied on for
		// tamper-evidence until that layer lands.
		if err := v.verifyCrypto(b, e); err != nil {
			return nil, err
		}
		events = append(events, e)
		prev = e
	}
	return events, nil
}

// verifyCrypto is the intentionally empty cryptographic-verification slot. It
// returns nil so structural verification stands alone today, and becomes the entry
// point for signature, hash-chain, and Merkle-inclusion checks when the
// tamper-evident spine is built. It is a method so that future state (a keyring, a
// signed root) has a home without changing VerifyStream's shape.
func (v *Verifier) verifyCrypto(canonical []byte, e spine.Event) error {
	_ = canonical
	_ = e
	return nil
}
