package main

import (
	"context"
	"fmt"
	"io"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// runLearningMission runs one objective end to end over a durable store: it recalls
// what past runs learned into the prompt, drives the goal to a result through the
// sandboxed toolset, and (when a distiller is supplied) distills the converged run
// back into skills and memory so the next run starts ahead. Progress is written to
// out; the model's final summary is returned.
func runLearningMission(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, distiller learn.Distiller, workdir, objective, verify string, store *sqlite.Store, signer chain.RootSigner, verbose bool, fanout *fanoutConfig, opts ...driveOption) (string, error) {
	reg, err := missionRegistry()
	if err != nil {
		return "", err
	}
	skills, memories := store.Skills(), store.Memory()

	// Recall first: fold what was learned before into the standing instructions, and
	// remember which skills were surfaced so the run can be told what it was shown.
	system := defaultSystemPrompt
	block, recalled, _ := recallContext(ctx, skills, memories, objective)
	if block != "" {
		system += "\n\n" + block
	}

	// Record the run's events into a verifiable chain as they are produced, when an
	// instance signer is available. The recorder wraps the log and changes nothing
	// about how events are written.
	log := store.Log()
	var rec *chain.RecordingLog
	if signer != nil {
		rec = chain.NewRecordingLog(log, nil)
		log = rec
	}

	resources := store.Resources(reg)
	// The run reads skills from the store recall just offered from, through a toolset
	// that keeps the list of what it served. Prepended rather than appended so a caller
	// could override it, and passed unconditionally: the offer in the prompt names
	// skill_read, so the tool has to be there.
	skillset := skilltool.New(skills)
	opts = append([]driveOption{withSkills(skillset)}, opts...)
	result, source, transcript, err := drive(ctx, out, model, plan, workdir, objective, system, resources, store.Jobs(), log, verbose, "", fanout, opts...)

	// Record what the run was shown and what it took up as two separate facts. Every
	// recalled skill was offered; only the ones the model loaded through skill_read
	// are credited with the run's outcome, because being in the prompt establishes
	// nothing about whether a skill was used and a win handed to all five makes the
	// number a property of the run. Both are gated with capture, so a read-only
	// --no-learn run records neither.
	if distiller != nil {
		_ = learn.Offer(ctx, skills, recalled)
		_ = learn.Reinforce(ctx, skills, skillset.Reads(), err == nil)
	}
	if err != nil {
		return "", err
	}

	// Ground the run's success in an independent check before sealing, when one is
	// given. The check runs after the agent has stopped and is never seen by the
	// model, so a sealed run that claims success is backed by a verdict the model
	// could not have produced. This must run before the seal so the check and outcome
	// events are part of the verifiable record.
	if verify != "" && rec != nil {
		recordGroundTruth(ctx, out, rec, source, workdir, verify)
	}

	// Seal the run into a signed, verifiable record stored on its own stream, so it
	// can be checked later from the durable store alone. Best effort: a sealing
	// failure is reported but never fails the run.
	if rec != nil {
		if serr := sealRun(ctx, store, rec, source, signer); serr != nil {
			_, _ = fmt.Fprintf(out, "  (run not sealed: %v)\n", serr)
		} else {
			_, _ = fmt.Fprintf(out, "  run sealed; verify with: flynn spine verify %s\n", source)
		}
		// Checkpoint the resource projection alongside the sealed run, so a later
		// rebuild resumes from a verified snapshot instead of folding the whole
		// stream. Best effort, like the seal: a snapshot is a derived cache, and a
		// missing one is only slower, never wrong.
		if serr := resources.Snapshot(ctx); serr != nil {
			_, _ = fmt.Fprintf(out, "  (resources not snapshotted: %v)\n", serr)
		}
	}

	// Capture: distill the converged run back into durable, provenance-stamped
	// knowledge. A captured skill's check is run in a sandbox at the working
	// directory before it is crystallized, so a broken procedure is dropped rather
	// than learned. Capture failures never fail the run; learning is best effort.
	if distiller != nil {
		distillOutcome(ctx, out, distiller, skills, memories, workdir, learn.Outcome{
			Objective:  objective,
			Result:     result,
			Transcript: transcript,
			Converged:  true,
			Source:     source,
		})
	}
	return result, nil
}

// recordGroundTruth runs the run's verification command independently and records the
// result on the run's stream: a check event carrying the real exit-code verdict, and
// an outcome event that binds the run's success to it. The verdict is the system's,
// produced after the agent stopped and never seen by the model, so a sealed run that
// claims success is grounded in a check the agent could not have graded itself. A
// failing or unrunnable check is recorded honestly, which makes the run's own record
// fail the ground-truth check rather than overstate the outcome.
func recordGroundTruth(ctx context.Context, out io.Writer, log spine.Log, stream, workdir, verify string) {
	passed := runVerification(ctx, workdir, verify)
	if err := appendGroundTruth(ctx, log, stream, passed); err != nil {
		_, _ = fmt.Fprintf(out, "  (ground-truth not recorded: %v)\n", err)
		return
	}
	if passed {
		_, _ = fmt.Fprintln(out, "  ground-truth check passed; the run's success is independently verifiable")
	} else {
		_, _ = fmt.Fprintln(out, "  ground-truth check did not pass; the run's success is not grounded")
	}
}

// runVerification runs command in a confined sandbox at workdir and reports whether it
// succeeded (exit 0). The command is operator-supplied and run after the agent stops,
// so its verdict is independent of anything the model produced.
func runVerification(ctx context.Context, workdir, command string) bool {
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		return false
	}
	// Closing releases whatever the confinement registered with the operating system for
	// this check. Without it, every verified run leaves a container profile behind.
	defer func() { _ = sb.Close() }()
	res, err := sb.Exec(ctx, sandbox.Command{Line: command})
	return err == nil && res.ExitCode == 0
}

// appendGroundTruth records the independent check's verdict and binds the run's
// success to it on the run's stream, using the chain's ground-truth vocabulary.
func appendGroundTruth(ctx context.Context, log spine.Log, stream string, passed bool) error {
	if _, err := log.Append(ctx, spine.AppendInput{
		Stream:  stream,
		Type:    chain.CheckRecorded,
		Actor:   spine.ActorSystem,
		Payload: map[string]any{chain.CheckRefKey: int64(1), chain.CheckPassedKey: passed},
	}); err != nil {
		return err
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream:  stream,
		Type:    chain.OutcomeRecorded,
		Actor:   spine.ActorSystem,
		Payload: map[string]any{chain.OutcomeResultKey: chain.ResultSuccess, chain.CheckRefKey: int64(1)},
	})
	return err
}

// appendProvenance records a run's provenance declaration on its stream using the
// chain's provenance vocabulary: an external agent harness drove the loop, so the
// sealed record vouches for enforced effects (every effect crossed the dispatch waist)
// but names the harness's inner reasoning as an unobserved gap, and the run is
// non-replayable (the run does not drive the harness's inner loop). `flynn spine
// verify` reports this tier mix from the same record. A native run records none of it.
func appendProvenance(ctx context.Context, log spine.Log, stream string, d externalProvenance) error {
	payload := map[string]any{
		chain.ProvenanceHarnessKey:    d.harness,
		chain.ProvenanceEffectsKey:    chain.TierEnforced,
		chain.ProvenanceReasoningKey:  chain.TierUnobserved,
		chain.ProvenanceReplayableKey: false,
		chain.ProvenanceAttestedKey:   d.attested,
		chain.ProvenanceNativeRateKey: d.nativeRate,
		chain.ProvenanceDriftKey:      d.drift,
	}
	if len(d.drift) == 0 {
		// An empty map and an absent key both mean the harness honored the contract. Omit
		// it, so a clean run's record carries no key inviting the reader to wonder.
		delete(payload, chain.ProvenanceDriftKey)
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream:  stream,
		Type:    chain.ProvenanceDeclared,
		Actor:   spine.ActorSystem,
		Payload: payload,
	})
	return err
}

// externalProvenance is what the host observed of an external-harness run: which harness
// drove it, how many events the harness reported about itself, and how far it drifted
// from the session contract it was given.
type externalProvenance struct {
	harness    string
	attested   int
	nativeRate float64
	drift      map[string]int
}

// distillOutcome distills a converged run into durable skills and memory and retires
// skills that enough runs have proven unhelpful, reporting the tally to out. It is
// best effort: a capture or decay failure never fails the run. A captured skill's
// check is verified in a sandbox at workdir before it is crystallized, so a broken
// procedure is dropped rather than learned. Shared by the one-shot runner and the
// interactive session so both capture identically.
func distillOutcome(ctx context.Context, out io.Writer, distiller learn.Distiller, skills state.SkillStore, memories state.MemoryStore, workdir string, outcome learn.Outcome) {
	curator := learn.NewCurator(distiller, skills, memories, learn.WithVerifier(governedVerifier(workdir)))
	if captured, err := curator.Curate(ctx, outcome); err == nil {
		if n := len(captured.Skills) + len(captured.Memories); n > 0 {
			_, _ = fmt.Fprintf(out, "  (learned %d skill(s), %d memory item(s))\n", len(captured.Skills), len(captured.Memories))
		}
		if d := len(captured.Dropped); d > 0 {
			_, _ = fmt.Fprintf(out, "  (dropped %d unverified skill(s))\n", d)
		}
	}

	// Retire skills that enough runs have proven unhelpful, so the index stays
	// high-signal rather than growing without bound.
	if archived, derr := learn.Decay(ctx, skills, state.Scope{}, learn.DefaultDecay()); derr == nil && len(archived) > 0 {
		_, _ = fmt.Fprintf(out, "  (retired %d unhelpful skill(s))\n", len(archived))
	}
}

// governedVerifier builds the skill-check verifier the CLI uses: a sandbox verifier
// that runs each check at dir, wrapped so the check is dispatched through the waist.
// Routing it through dispatch means a verification is admitted against the run's
// grant and traced like every tool call, rather than executing a model-proposed
// command on a side channel that bypasses governance. With no grant bound the
// admitter is permissive, so a standalone run still verifies, just ungoverned.
func governedVerifier(dir string) learn.Verifier {
	inner := learn.NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(dir, sandbox.WithDefaultConfinement())
	})
	return learn.NewGovernedVerifier(inner, dispatch.WithAdmitter(capability.Admitter{}))
}

// governedDistiller wraps the model distiller so its model call runs through the
// dispatch waist, like the agent's own model calls and the governed verifier. With
// no grant bound the admitter is permissive, so a standalone run still distills,
// just ungoverned.
func governedDistiller(model llm.Model) learn.Distiller {
	return learn.NewGovernedDistiller(learn.NewModelDistiller(model), dispatch.WithAdmitter(capability.Admitter{}))
}

// childModelResolver builds the model resolver the Router consults for a delegated
// child goal that names a model other than the run's default. A local catalog model
// is provisioned and served on demand; a hosted model resolves through the same
// credential chain (vault then environment) as the root model, so a child runs on
// whatever its archetype pins without any extra setup. The child's scaffolding plan
// is not threaded through (the Router shares one base plan across loops), so a local
// child runs on the shared defaults.
func childModelResolver(ctx context.Context, dataDir string) driver.ModelResolver {
	return func(id string) (llm.Model, error) {
		if isLocalModelID(id) {
			m, _, err := resolveLocalModel(ctx, id, dataDir)
			return m, err
		}
		return provider.ResolveWith(ctx, id, credentialSource(dataDir))
	}
}
