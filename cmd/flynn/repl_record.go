package main

// What the session does to its run's durable record: declaring the provenance of an
// externally driven run, sealing, verifying, exporting, and branching it.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/resource"
)

// declareProvenance writes the run's provenance onto its stream when an external agent
// harness drove the session: the record then vouches for the enforced effects (every
// tool call crossed the dispatch waist) while naming the harness's inner reasoning as an
// unobserved gap, so an external run never claims the integrity of a native one. The
// absence of this declaration is what marks a record as natively driven, so a session
// the CLI drove must not be sealed without it.
//
// It is written once, and late: a verifier reads the first declaration a record carries,
// and the tallies it declares (the attested events, the harness's tool-choice rate) are
// only complete once the session's episodes have all run. A native session declares
// nothing, which is what says its own loop drove it.
func (s *replSession) declareProvenance(ctx context.Context) {
	if s.ext == nil || s.provDeclared || !s.started {
		return
	}
	s.provDeclared = true
	if err := appendProvenance(ctx, s.store.Log(), s.runID, observedProvenance(s.ext)); err != nil {
		_, _ = fmt.Fprintf(s.out, "  (provenance not recorded: %v)\n", err)
		return
	}
	// An event the harness reported that the record could not hold is a hole in the
	// harness's account of itself. The declaration names every event it reported, so a
	// verifier sees the gap from the record alone; saying it here tells the operator why.
	if lost, lerr := unrecordedAttested(s.ext); lost > 0 {
		_, _ = fmt.Fprintf(s.out, "  (%d attested event(s) not recorded: %v)\n", lost, lerr)
	}
}

// seal signs the session's run into a verifiable record stored on its stream. It needs a
// started run and the instance signer; without either it reports why rather than failing
// the session. Sealing reads the run's current durable events, so it captures the whole
// conversation, including history a resumed session continued.
func (s *replSession) seal(ctx context.Context) error {
	if !s.started {
		return errors.New("nothing to seal yet; run a turn first")
	}
	if s.signer == nil {
		return errors.New("cannot seal: no instance signing key is available")
	}
	// An external run's record must carry its provenance declaration before it is sealed,
	// or the sealed record reads as though Flynn's own loop drove it: the exact overclaim
	// the declaration exists to prevent.
	s.declareProvenance(ctx)
	return sealRunFromStore(ctx, s.store, s.runID, s.signer)
}

// verify checks the session's sealed record and writes its per-tier report to out. It
// returns errChecksFailed if a tier fails (the report names which), or a plain error
// when the run has not been sealed yet.
func (s *replSession) verify(ctx context.Context, out io.Writer) error {
	if !s.started {
		return errors.New("nothing to verify yet; run a turn first")
	}
	return verifyStoredRun(ctx, out, s.store, s.runID)
}

// export writes the session's sealed record to path and returns the path written. It
// needs a sealed run: a run not yet sealed carries no record and is reported, so a caller
// seals before exporting. The written file is the portable, independently verifiable
// artifact `flynn spine verify --file` (and any third party) checks.
func (s *replSession) export(ctx context.Context, path string) (string, error) {
	if !s.started {
		return "", errors.New("nothing to export yet; run a turn first")
	}
	if path == "" {
		path = s.runID + ".flynnrecord"
	}
	if err := exportRecord(ctx, s.store, s.runID, path); err != nil {
		return "", err
	}
	return path, nil
}

// fork branches the current run into a new independent run seeded with a verbatim copy
// of the conversation so far, switches the session onto it, and returns its id. The
// original run keeps its id, its recorded history, and its seal, so a branch never
// disturbs the run it came from. The fork opens a fresh event stream under the new id:
// its turns record onto their own hash chain from the branch point on, while the model
// still sees the whole prior conversation carried on the copied checkpoint. It needs a
// started run; without one there is nothing to branch from.
func (s *replSession) fork(ctx context.Context) (string, error) {
	if !s.started {
		return "", errors.New("nothing to fork yet; run a turn first")
	}
	rs := s.store.Resources(s.reg)
	parent, err := rs.Get(ctx, goal.Kind, resource.Scope{}, s.runID)
	if err != nil {
		return "", err
	}
	forkID := ids.New()
	forked := parent
	forked.Name = forkID
	// Clear the parent's identity and sync envelope so the store creates a new record
	// instead of overwriting the run this branched from. Spec and Status (the
	// conversation checkpoint) carry over verbatim, so the fork opens from the exact
	// state the parent is in.
	forked.ID = ""
	forked.Envelope = resource.Envelope{}
	forked.Annotations = withForkParent(parent.Annotations, s.runID)
	if _, err := rs.Put(ctx, forked); err != nil {
		return "", err
	}
	// Switch the session onto the fork and rewind the cursor to the start of its empty
	// stream, so the next turn continues the copied conversation while recording onto
	// the fork's own chain.
	s.runID = forkID
	s.lastSeq = 0
	return forkID, nil
}

// forkParentAnnotation records the id of the run a fork branched from, so the lineage of
// a branched run is auditable from its resource alone.
const forkParentAnnotation = "flynn/forked-from"

// withForkParent returns a copy of the parent run's annotations with the fork-parent id
// set, leaving the parent's own annotation map untouched.
func withForkParent(parent map[string]string, parentID string) map[string]string {
	out := make(map[string]string, len(parent)+1)
	for k, v := range parent {
		out[k] = v
	}
	out[forkParentAnnotation] = parentID
	return out
}
