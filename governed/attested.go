package governed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/spine"
)

// attestRecorder is an external-agent driver whose harness's attested events can be
// recorded onto the run's stream, and which reports how many of them were lost. The
// driver port itself stays free of the record (a native loop has nothing to attest), so
// the host asks for the capability rather than requiring it.
type attestRecorder interface {
	SetRecorder(externagent.Recorder)
	Unrecorded() (int, error)
}

// The external-agent driver satisfies it. Asserted at compile time because the binding is
// a type assertion at run time: a signature that drifted would turn the recording into a
// silent no-op, and the run would declare an attested count over an empty account.
var _ attestRecorder = (*externagent.Driver)(nil)

// AttestedSink writes the external harness's own account of its episode onto the run's
// event stream: one event per line the harness produced, carrying the line verbatim so a
// reader can hold the harness's claims next to the effects the dispatch waist enforced
// and see where the two accounts part. It is the counterpart of the governance sink,
// which records what the run enforced; this records what the harness said.
type AttestedSink struct {
	log    spine.Log
	stream string
}

// NewAttestedSink returns a sink that records harness events onto stream on log.
func NewAttestedSink(log spine.Log, stream string) *AttestedSink {
	return &AttestedSink{log: log, stream: stream}
}

// Record appends one attested event. The raw line is inlined up to chain.AttestedRawLimit
// bytes and always digested whole, so a harness that echoes a large tool result into its
// stream cannot inflate the record while a verifier can still match a truncated line
// against the log the harness kept.
func (s *AttestedSink) Record(ctx context.Context, ev externagent.Event) error {
	sum := sha256.Sum256(ev.Raw)
	raw := ev.Raw
	truncated := len(raw) > chain.AttestedRawLimit
	if truncated {
		raw = raw[:chain.AttestedRawLimit]
	}
	payload := map[string]any{
		chain.AttestedKindKey:   ev.Kind.String(),
		chain.AttestedTierKey:   ev.Tier.String(),
		chain.AttestedRawKey:    string(raw),
		chain.AttestedDigestKey: hex.EncodeToString(sum[:]),
		chain.AttestedBytesKey:  len(ev.Raw),
	}
	if truncated {
		payload[chain.AttestedTruncatedKey] = true
	}
	// The harness is the actor: this is its account of itself, not the run's observation,
	// and the record says so in the event's own attribution as well as its tier.
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream:    s.stream,
		Type:      chain.AttestedRecorded,
		Actor:     spine.ActorAgent,
		Payload:   payload,
		Principal: capability.PrincipalFromContext(ctx),
	})
	return err
}

var _ externagent.Recorder = (*AttestedSink)(nil)

// RecordAttested binds the run's stream to the external driver, so each event the
// harness reports lands on the record as it is projected. A driver that cannot record
// (another harness implementation) is left alone; the run then declares its attested
// count and carries no events, which `flynn spine verify` reports as a mismatch rather
// than passing over in silence.
func RecordAttested(d driver.Driver, log spine.Log, stream string) {
	if r, ok := d.(attestRecorder); ok {
		r.SetRecorder(NewAttestedSink(log, stream))
	}
}

// UnrecordedAttested reports how many of the harness's events could not be written to the
// record and why, so the run can say so rather than leaving a silent hole behind a count.
func UnrecordedAttested(d driver.Driver) (int, error) {
	if r, ok := d.(attestRecorder); ok {
		return r.Unrecorded()
	}
	return 0, nil
}
