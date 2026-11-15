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

// attestRecorder is an optional driver capability, because a native loop has nothing to
// attest and the driver port stays free of the record.
type attestRecorder interface {
	SetRecorder(externagent.Recorder)
	Unrecorded() (int, error)
}

// The binding is a run-time type assertion, so a drifted signature would make recording a
// no-op under a declared attested count; this fails the build instead.
var _ attestRecorder = (*externagent.Driver)(nil)

// AttestedSink records each line the external harness produced, verbatim, so a reader can
// compare what the harness claimed with what the dispatch waist enforced.
type AttestedSink struct {
	log    spine.Log
	stream string
}

// NewAttestedSink records onto stream on log.
func NewAttestedSink(log spine.Log, stream string) *AttestedSink {
	return &AttestedSink{log: log, stream: stream}
}

// Record inlines at most chain.AttestedRawLimit bytes and digests the whole line, so a
// large echo cannot inflate the record and a truncated line still matches the harness log.
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
	// The harness is the actor because this is its account of itself.
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

// RecordAttested binds the run's stream to the driver. A driver that cannot record is left
// alone, and `flynn spine verify` reports its declared count with no events as a mismatch.
func RecordAttested(d driver.Driver, log spine.Log, stream string) {
	if r, ok := d.(attestRecorder); ok {
		r.SetRecorder(NewAttestedSink(log, stream))
	}
}

// UnrecordedAttested reports how many harness events failed to record and why, so the run
// can declare the gap.
func UnrecordedAttested(d driver.Driver) (int, error) {
	if r, ok := d.(attestRecorder); ok {
		return r.Unrecorded()
	}
	return 0, nil
}
