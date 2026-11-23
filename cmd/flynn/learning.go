package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// errGroundTruthFailed is returned when the --verify check, run after the agent stopped,
// did not pass. The run is still sealed and distilled, since its record states the
// failure; the error keeps a goal its own check disagrees with from exiting 0.
var errGroundTruthFailed = errors.New("the --verify check did not pass: the run's success is not grounded")

// errSkillNotLoaded is returned before any model call when a skill named by
// [withLoadedSkills] cannot be loaded. It is the caller's mistake, not the run's
// outcome, so a measurement stops on it instead of scoring a failed attempt.
var errSkillNotLoaded = errors.New("a skill named for the run could not be loaded")

// runLearningMission drives one objective over a durable store: it recalls past
// learning into the prompt, runs the goal through the sandboxed toolset and, given a
// distiller, distills a converged run back into skills and memory. It returns the
// model's final summary.
func runLearningMission(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, distiller learn.Distiller, workdir, objective, verify string, store *sqlite.Store, signer chain.RootSigner, verbose bool, fanout *fanoutConfig, opts ...driveOption) (string, error) {
	reg, err := governed.Registry()
	if err != nil {
		return "", err
	}
	skills := store.Skills()
	// Every write goes through the curated store, so a fact supersedes the standing answer
	// on its subject and an episode joins the series consolidation reads.
	// Memory is assembled before the conversation, so the embedder option is applied to a
	// local config here and again by drive; both are plain setters.
	var cfg driveConfig
	for _, o := range opts {
		o(&cfg)
	}
	mem := governed.NewMemoryStack(store.Memory(), governed.NoticeWriter(out), governed.WithMemoryEmbedder(cfg.emb))
	memories := mem.Store()

	// With a prime scope, a memory the digest pushed is attributed as primed when used.
	ctx = governed.WakeContext(ctx)

	// The digest pushes what this install knows whether or not the objective mentions it,
	// which a pull-only store cannot do. Recall pulls what the objective's words match,
	// plus skills to offer, and records what was surfaced so the run is told what it saw.
	system := governed.WithWake(ctx, mem, defaultSystemPrompt, out)
	block, recalled, _ := recallContext(ctx, skills, memories, objective)
	if block != "" {
		system += "\n\n" + block
	}
	// Skills are read from the store recall offered from, through a toolset that lists
	// what it served. It is prepended so a caller can override it, and always present
	// because the prompt's offer names skill_read.
	//
	// Loading a skill also surfaces what past runs learned from it and counts that as a
	// use of those memories: the pull side of the digest's push.
	skillset := skilltool.New(skills, skilltool.WithNotes(mem.SkillNotes()))
	// A loaded skill comes from the toolset the run will use, so a caller's override
	// (a skill version with its own resources) is what the prompt carries.
	loader := skillset
	if cfg.skills != nil {
		loader = cfg.skills
	}
	var loaded strings.Builder
	for _, slug := range cfg.loaded {
		text, lerr := loader.Load(ctx, slug)
		if lerr != nil {
			return "", fmt.Errorf("%w: %s: %w", errSkillNotLoaded, slug, lerr)
		}
		fmt.Fprintf(&loaded, "\n\nThe skill %s is loaded for this run. Its full procedure follows:\n\n%s", slug, text)
	}
	system += loaded.String()
	// Last, so no operator or store text follows text the repository wrote.
	if p := cfg.project.Prompt(); p != "" {
		system += "\n\n" + p
	}

	// With a signer, events are recorded into a verifiable chain as they are written. The
	// recorder does not change how they are written.
	log := store.Log()
	var rec *chain.RecordingLog
	if signer != nil {
		rec = chain.NewRecordingLog(log, nil)
		log = rec
	}

	resources := store.Resources(reg)
	opts = append([]driveOption{withSkills(skillset)}, opts...)
	result, source, transcript, err := driveRequest{
		out:       out,
		model:     model,
		plan:      plan,
		workdir:   workdir,
		objective: objective,
		system:    system,
		rstore:    resources,
		jq:        store.Jobs(),
		log:       log,
		verbose:   verbose,
		fanout:    fanout,
	}.withOptions(opts...).execute(ctx)

	// Offered and used are recorded separately. Being in the prompt says nothing about use,
	// and crediting all recalled skills would make the number a property of the run, so
	// only skills loaded through skill_read get the outcome. A --no-learn run records
	// neither.
	if distiller != nil {
		_ = learn.Offer(ctx, skills, recalled)
		_ = learn.Reinforce(ctx, skills, skillset.Reads(), err == nil)
	}
	if err != nil {
		// A stopped run is sealed with its ending recorded first. Without a run id nothing
		// started, so there is no stream to seal.
		if rec != nil && source != "" {
			sealStopped(ctx, out, store, rec, source, signer, err)
		}
		return "", err
	}

	// The check runs after the agent stopped and is never shown to the model, so a sealed
	// success is backed by a verdict the model could not produce. It runs before the seal
	// so its events are in the record.
	groundTruthFailed := false
	if verify != "" && rec != nil {
		groundTruthFailed = !recordGroundTruth(ctx, out, rec, source, workdir, verify)
	}

	// Best effort: a sealing failure is reported and never fails the run.
	if rec != nil {
		sealReported(ctx, out, store, rec, source, signer)
		// A snapshot lets a later rebuild start from a verified checkpoint. It is a derived
		// cache, so a missing one is slower and never wrong.
		if serr := resources.Snapshot(ctx); serr != nil {
			_, _ = fmt.Fprintf(out, "  (resources not snapshotted: %v)\n", serr)
		}
	}

	// A captured skill's check runs in a sandbox at the working directory before it is
	// crystallized, so a broken procedure is dropped. Capture failures never fail the run.
	if distiller != nil {
		distillOutcome(ctx, out, distiller, skills, memories, workdir, learn.Outcome{
			Objective:  objective,
			Result:     result,
			Transcript: transcript,
			Converged:  true,
			Source:     source,
			SkillsRead: skillset.Reads(),
		}, cfg.project.Taint()...)
	}
	if groundTruthFailed {
		return result, errGroundTruthFailed
	}
	return result, nil
}

// recordGroundTruth runs the verification command and records a check event with the
// exit-code verdict and an outcome event binding the run's success to it. The verdict
// comes after the agent stopped and is never shown to the model. A failing or
// unrunnable check is recorded as such, so the record fails the ground-truth tier
// instead of overstating the outcome.
//
// It returns the verdict even if the log refused it, because the exit status follows
// the check.
func recordGroundTruth(ctx context.Context, out io.Writer, log spine.Log, stream, workdir, verify string) bool {
	passed := runVerification(ctx, workdir, verify)
	if err := appendGroundTruth(ctx, log, stream, passed); err != nil {
		_, _ = fmt.Fprintf(out, "  (ground-truth not recorded: %v)\n", err)
		return passed
	}
	if passed {
		_, _ = fmt.Fprintln(out, "  ground-truth check passed; the run's success is independently verifiable")
	} else {
		_, _ = fmt.Fprintln(out, "  ground-truth check did not pass; the run's success is not grounded")
	}
	return passed
}

// runVerification runs the operator's command in a confined sandbox at workdir and
// reports exit 0.
func runVerification(ctx context.Context, workdir, command string) bool {
	passed, _ := verifyWithOutput(ctx, workdir, command)
	return passed
}

// verifyWithOutput is [runVerification] that also returns what the command printed, or
// why it could not run, so a failed attempt can be read afterwards.
func verifyWithOutput(ctx context.Context, workdir, command string) (bool, string) {
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		return false, "the sandbox could not start: " + err.Error()
	}
	// Without Close every verified run leaves a container profile behind.
	defer func() { _ = sb.Close() }()
	res, err := sb.Exec(ctx, sandbox.Command{Line: command})
	if err != nil {
		return false, "the command could not run: " + err.Error()
	}
	return res.ExitCode == 0, fmt.Sprintf("exit %d\n%s", res.ExitCode, res.Output)
}

// appendGroundTruth records the verdict and binds the run's success to it, in the
// chain's ground-truth vocabulary.
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

// appendProvenance records that an external harness drove the loop: the record vouches
// for enforced effects (each crossed the dispatch waist), names the harness's reasoning
// as unobserved, and marks the run non-replayable. `flynn spine verify` reports this
// tier mix. Native runs record none of it.
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
		// Empty and absent both mean the contract was honoured; omitting the key keeps a clean
		// record from implying otherwise.
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

// externalProvenance is what the host observed of an external-harness run: the harness,
// the events it reported about itself, and its drift from the session contract.
type externalProvenance struct {
	harness    string
	attested   int
	nativeRate float64
	drift      map[string]int
}

// distillOutcome distills a converged run into skills and memory, retires skills enough
// runs proved unhelpful, and reports the tally. Failures never fail the run. A captured
// skill's check runs in a sandbox at workdir first, so a broken procedure is dropped.
// The one-shot runner and the interactive session share it.
//
// carried are taint reasons from conversation the transcript no longer holds (a
// session's compacted turns), so a summary cannot launder what it summarised.
func distillOutcome(ctx context.Context, out io.Writer, distiller learn.Distiller, skills state.SkillStore, memories state.MemoryStore, workdir string, outcome learn.Outcome, carried ...string) {
	ctx = taintFromTranscript(ctx, outcome.Transcript, carried...)
	curator := learn.NewCurator(distiller, skills, memories, learn.WithVerifier(governed.SkillVerifier(workdir)))
	if captured, err := curator.Curate(ctx, outcome); err == nil {
		if n := len(captured.Skills) + len(captured.Memories); n > 0 {
			_, _ = fmt.Fprintf(out, "  (learned %d skill(s), %d memory item(s))\n", len(captured.Skills), len(captured.Memories))
		}
		if d := len(captured.Dropped); d > 0 {
			_, _ = fmt.Fprintf(out, "  (dropped %d unverified skill(s))\n", d)
		}
	}

	// Decay keeps the skill index from growing without bound.
	if archived, derr := learn.Decay(ctx, skills, state.Scope{}, learn.DefaultDecay()); derr == nil && len(archived) > 0 {
		_, _ = fmt.Fprintf(out, "  (retired %d unhelpful skill(s))\n", len(archived))
	}
}

// untrustedOutputTools are tools whose results this binary cannot vouch for. A note
// distilled from a run that called one is tainted: never pushed at wake, and refused by
// `memory promote`. The shell has open network, and without a governed egress proxy a
// curl looks like a go test. A spawn's child transcript is not in the parent's. File
// tools and skill_read read the working tree and skill store, which the install trusts.
// TestUntrustedOutputToolsCoverEveryUntrustedTool fails when a reduced-trust default
// tool is missing here.
var untrustedOutputTools = map[string]bool{
	"bash":              true,
	mission.ActionSpawn: true,
}

// taintFromTranscript returns ctx with a fresh taint scope, marked for each call to one
// of [untrustedOutputTools] in transcript and each carried reason. The guard stamps
// the distiller's memory writes under it.
//
// The scope opens here because the distiller is the run's only memory writer and the
// transcript is the full record of what the model saw. That gives the same answer as
// marking each tool result on arrival, without threading a scope through every drive
// path.
func taintFromTranscript(ctx context.Context, transcript []llm.Message, carried ...string) context.Context {
	ctx = guard.NewTaintScope(ctx)
	for _, reason := range append(taintReasons(transcript), carried...) {
		guard.MarkTainted(ctx, reason)
	}
	return ctx
}

// taintReasons names each call to one of [untrustedOutputTools] as the guard records
// it ("tool:bash").
func taintReasons(transcript []llm.Message) []string {
	var reasons []string
	for _, m := range transcript {
		for _, b := range m.Blocks {
			if b.ToolUse != nil && untrustedOutputTools[b.ToolUse.Name] {
				reasons = append(reasons, guard.SchemeTool+b.ToolUse.Name)
			}
		}
	}
	return reasons
}

// childModelResolver resolves a delegated child's non-default model. A local catalog
// model is served on demand; a hosted one uses the root's source (the credential chain
// plus the data directory's providers file), so a child runs on its archetype's model
// with no setup. The Router shares one base plan across loops, so a local child gets
// the shared scaffolding defaults.
func childModelResolver(ctx context.Context, dataDir string) driver.ModelResolver {
	return func(id string) (llm.Model, error) {
		if isLocalModelID(id) {
			m, _, err := resolveLocalModel(ctx, id, dataDir)
			return m, err
		}
		src, err := modelSource(dataDir)
		if err != nil {
			return nil, err
		}
		return provider.ResolveWith(ctx, id, src)
	}
}
