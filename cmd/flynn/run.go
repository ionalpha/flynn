package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/brakes"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/internal/archetype"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/internal/instance"
	"github.com/ionalpha/flynn/internal/migrate"
	"github.com/ionalpha/flynn/internal/playbook"
	"github.com/ionalpha/flynn/internal/profilestore"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
	"github.com/ionalpha/flynn/tools"
)

// defaultSystemPrompt frames the agent for a coding/automation task. It is kept
// short on purpose: a capable model works better from a clear goal than from a long
// list of rules.
const defaultSystemPrompt = `You are Flynn, an autonomous agent. You take on whatever objective the user gives you, from writing, research, and analysis to planning and software work.
You have tools to run shell commands and to read, write, edit, glob, and grep files in a sandboxed working directory; every command and file path is confined to it. Use the tools when the task calls for them, and answer directly when it does not.
Work toward the objective directly: gather what you need, do the work, and verify it with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// recallLimit caps how many learned skills and memory items are injected into a
// run's prompt. It is deliberately small: recall is precision-first, since a long,
// loosely-relevant context degrades the model's use of it more than it helps.
const recallLimit = 5

// defaultCompactionBudget is the input-token budget at which the CLI starts eliding
// the oldest middle turns from a long session. It is a conservative floor for the
// large hosted models the CLI targets by default (roughly half a 200k window);
// per-model, window-aware triggering arrives with the model registry.
const defaultCompactionBudget = 100_000

// openStore opens the durable SQLite store at dsn, or an ephemeral in-memory one
// when dsn is empty (used by tests and one-off runs). The same store backs the
// runtime's resources and job queue and the learning loop's skills and memory.
func openStore(ctx context.Context, dsn string, opts ...sqlite.Option) (*sqlite.Store, error) {
	if dsn == "" {
		dsn = ":memory:"
	}
	return sqlite.Open(ctx, dsn, opts...)
}

// dataStoreFile is the path of the durable database file under a data directory, or empty
// for an ephemeral ("" or ":memory:") data dir that has no file on disk. It is the single
// definition of where the store lives, so opening it and checking whether it exists agree.
func dataStoreFile(dataDir string) string {
	if dataDir == "" || dataDir == ":memory:" {
		return ""
	}
	return filepath.Join(dataDir, "flynn.db")
}

// openDataStore opens the durable store under a data directory, creating the
// directory and resolving the database file inside it. An empty or ":memory:"
// dataDir opens an ephemeral store.
func openDataStore(ctx context.Context, dataDir string, opts ...sqlite.Option) (*sqlite.Store, error) {
	dsn := dataStoreFile(dataDir)
	if dsn != "" {
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			return nil, err
		}
	}
	store, err := openStore(ctx, dsn, opts...)
	if err != nil {
		return nil, explainStoreOpenError(err, dataDir)
	}
	return store, nil
}

// explainStoreOpenError turns a store-open failure the user can act on into a clear
// message with the recovery step, and passes anything else through unchanged. Today it
// recognises an incompatible on-disk schema (a database created by a different build):
// rather than a raw migrate error, it names the recovery, so a run never dead-ends on an
// internal message.
func explainStoreOpenError(err error, dataDir string) error {
	var schema *migrate.IncompatibleSchemaError
	if errors.As(err, &schema) && dataDir != "" && dataDir != ":memory:" {
		return fmt.Errorf("the state database in %s was created by an incompatible build (%s %s).\n"+
			"Recover with `flynn db reset` (it backs up the old database first), or run against a fresh `--data-dir`",
			dataDir, schema.Migration, schema.Reason)
	}
	return err
}

// listRuns prints the runs recorded in the durable store: their id, phase, step
// count, and objective, newest first, so a run can be found and then inspected or
// resumed by its id.
func listRuns(dataDir string) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	goals, err := store.Resources(reg).ListAll(ctx, goal.Kind, nil)
	if err != nil {
		return err
	}
	if len(goals) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no runs yet")
		return nil
	}
	sort.Slice(goals, func(i, j int) bool { return goals[i].UpdatedHLC.Wall > goals[j].UpdatedHLC.Wall })
	for _, g := range goals {
		spec, _ := goal.DecodeSpec(g)
		st, _ := goal.DecodeStatus(g)
		phase := st.Phase
		if phase == "" {
			phase = goal.PhasePending
		}
		_, _ = fmt.Fprintf(os.Stdout, "  %s  %-9s  step %d  %s\n", g.Name, phase, st.Steps, oneLine(spec.Objective, 60))
	}
	return nil
}

// inspectRun replays a past run's recorded events from the durable spine through
// the same renderer a live run uses, so any run is auditable after the fact by its
// id (printed when the run starts). verbose shows the tool arguments, outputs, and
// per-turn detail; the default view shows the shape of the run.
func inspectRun(dataDir, runID string, verbose bool) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	events, err := session.History(ctx, store.Log(), runID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no run found with id %q under %s", runID, dataDir)
	}
	var meter usageMeter
	for _, ev := range events {
		renderEvent(os.Stdout, ev, verbose)
		if ev.Usage != nil {
			meter.add(*ev.Usage)
		}
	}
	renderUsageSummary(os.Stdout, meter)
	return nil
}

// renderUsageSummary writes the run's running token total as a final line, when any
// turn reported usage. It is the cumulative companion to the per-turn lines, so a
// run ends with one glance at what it cost and how much the prompt cache saved.
func renderUsageSummary(out io.Writer, meter usageMeter) {
	if s := meter.summary(); s != "" {
		_, _ = fmt.Fprintf(out, "%s\n", s)
	}
}

// regradeSkills re-runs every stored skill's check in a sandbox at the working
// directory, re-confirming the ones that still pass and retiring the ones that no
// longer do, then reports the tally.
func regradeSkills(dataDir string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	verifier := governedVerifier(cwd)
	res, err := learn.Regrade(ctx, store.Skills(), state.Scope{}, verifier)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "regrade: %d checked, %d reconfirmed, %d retired\n",
		res.Checked, len(res.Reconfirmed), len(res.Retired))
	return nil
}

// missionRegistry builds the resource registry the durable store admits against:
// the core kinds plus the Goal kind the runtime drives and the ModelProfile kind a
// reliability measurement is recorded under.
func missionRegistry() (*resource.Registry, error) {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		return nil, err
	}
	if err := goal.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := budgetpkg.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := inbox.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := profilestore.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := archetype.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := instance.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := credential.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := extension.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := service.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := dependency.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := playbook.RegisterKind(reg); err != nil {
		return nil, err
	}
	return reg, nil
}

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
	// remember which skills were surfaced so the run's outcome can reinforce them.
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
	result, source, transcript, err := drive(ctx, out, model, plan, workdir, objective, system, resources, store.Jobs(), log, verbose, "", fanout, opts...)

	// Reinforce the recalled skills by the run's outcome: a skill present in a run
	// that converged earns a win; one in a run that failed earns only a use. This is
	// gated with capture (a read-only --no-learn run records nothing).
	if distiller != nil && len(recalled) > 0 {
		_ = learn.Reinforce(ctx, skills, recalled, err == nil)
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

// recallContext queries the durable skills and memory for what is relevant to the
// objective and renders a compact, bounded block to prepend to the system prompt.
// It returns "" when nothing is on file, so a fresh agent's prompt is unchanged.
//
// The store's full-text search matches a query as a single phrase, so recall runs
// one query per keyword of the objective, unions the hits, then ranks them by how
// many of the objective's keywords each one carries, with verified skills boosted
// above unverified ones. Only the top few survive, since a long, loosely-relevant
// context hurts the model's use of it more than it helps. This is a lexical first
// cut; vector recall is a later refinement.
// recallContext returns the prompt block, the slugs of the skills surfaced (for
// outcome reinforcement), and a compact human-readable line per recalled item (a
// skill name or a memory snippet) so the session can show the user what it pulled in.
func recallContext(ctx context.Context, skills state.SkillStore, memories state.MemoryStore, objective string) (block string, recalled []string, items []string) {
	terms := keywords(objective)
	if len(terms) == 0 {
		return "", nil, nil
	}
	sk := rankSkills(terms, gatherSkills(ctx, skills, terms))
	mem := rankMemory(terms, gatherMemory(ctx, memories, terms))
	if len(sk) == 0 && len(mem) == 0 {
		return "", nil, nil
	}

	var b strings.Builder
	b.WriteString("From earlier runs you have learned the following. Use anything relevant; ignore the rest.")
	if len(sk) > 0 {
		b.WriteString("\nSkills:")
		for _, s := range sk {
			fmt.Fprintf(&b, "\n- %s: %s", s.Name, truncate(s.Body, 240))
			recalled = append(recalled, s.Slug)
			items = append(items, "skill: "+s.Name)
		}
	}
	if len(mem) > 0 {
		b.WriteString("\nMemory:")
		for _, m := range mem {
			fmt.Fprintf(&b, "\n- %s", truncate(m.Content, 240))
			items = append(items, "memory: "+oneLine(m.Content, 100))
		}
	}
	return b.String(), recalled, items
}

// gatherSkills unions the per-keyword full-text hits into a deduped candidate set
// for ranking.
func gatherSkills(ctx context.Context, skills state.SkillStore, terms []string) []state.Skill {
	seen := map[string]bool{}
	var out []state.Skill
	for _, term := range terms {
		found, err := skills.Search(ctx, term, recallLimit)
		if err != nil {
			continue
		}
		for _, s := range found {
			if !seen[s.ID] {
				seen[s.ID] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// gatherMemory unions the per-keyword recall hits into a deduped candidate set.
func gatherMemory(ctx context.Context, memories state.MemoryStore, terms []string) []state.MemoryItem {
	seen := map[string]bool{}
	var out []state.MemoryItem
	for _, term := range terms {
		found, err := memories.Recall(ctx, state.RecallQuery{Query: term, Limit: recallLimit})
		if err != nil {
			continue
		}
		for _, m := range found {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// rankSkills orders candidate skills by relevance (how many of the objective's
// keywords each carries), boosted for verified skills and for those with a strong
// confirmed track record, then caps the result. Relevance dominates; verification
// and confidence break ties between similarly relevant skills.
func rankSkills(terms []string, cands []state.Skill) []state.Skill {
	type scored struct {
		s     state.Skill
		score float64
	}
	ss := make([]scored, len(cands))
	for i, s := range cands {
		text := strings.ToLower(s.Name + " " + s.Body + " " + strings.Join(s.Tags, " "))
		score := float64(matchScore(terms, text)+verifiedBoost(s.Tags)) + learn.Confidence(s.Uses, s.Wins)
		ss[i] = scored{s, score}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].s.Slug < ss[j].s.Slug
	})
	out := make([]state.Skill, 0, recallLimit)
	for _, x := range ss {
		if len(out) >= recallLimit {
			break
		}
		out = append(out, x.s)
	}
	return out
}

// rankMemory orders candidate memory items by relevance, most-recent first on a
// tie, then caps the result.
func rankMemory(terms []string, cands []state.MemoryItem) []state.MemoryItem {
	type scored struct {
		m     state.MemoryItem
		score int
	}
	ss := make([]scored, len(cands))
	for i, m := range cands {
		ss[i] = scored{m, matchScore(terms, strings.ToLower(m.Content))}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].m.CreatedAt.After(ss[j].m.CreatedAt)
	})
	out := make([]state.MemoryItem, 0, recallLimit)
	for _, x := range ss {
		if len(out) >= recallLimit {
			break
		}
		out = append(out, x.m)
	}
	return out
}

// matchScore counts how many distinct terms appear in text, the lexical relevance
// signal recall ranks on.
func matchScore(terms []string, text string) int {
	n := 0
	for _, t := range terms {
		if strings.Contains(text, t) {
			n++
		}
	}
	return n
}

// verifiedBoost nudges a skill whose check passed (tagged verified) above an
// otherwise equally relevant unverified one, so evidence breaks ties.
func verifiedBoost(tags []string) int {
	for _, t := range tags {
		if t == "verified" {
			return 1
		}
	}
	return 0
}

// recallStopwords are common words dropped from an objective before recall, so a
// query term carries signal rather than matching nearly everything.
var recallStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"into": true, "from": true, "your": true, "you": true, "use": true, "run": true,
	"add": true, "all": true, "are": true, "its": true, "out": true, "via": true,
}

// keywords reduces an objective to up to eight distinct, lowercased content words
// (alphanumeric, 3+ chars, not a stopword) used as recall query terms.
func keywords(s string) []string {
	seen := map[string]bool{}
	var out []string
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, f := range fields {
		if len(f) < 3 || recallStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// truncate shortens s to at most n runes, appending an ellipsis when it cut.
func truncate(s string, n int) string { return text.Clip(strings.TrimSpace(s), n) }

// driveConfig collects the optional levers a run is driven with. Its zero value (no
// budget, no resource caps) drives a run exactly as before, so a caller that passes
// no option is unaffected.
type driveConfig struct {
	budget    budgetpkg.Limits
	resLimits sandbox.ResourceLimits
	extAgent  *externAgent
	toolset   *boundToolset
	observe   func(session.Event)
}

// driveOption configures a run driven by drive.
type driveOption func(*driveConfig)

// withBudget caps a run's total spend: every model and tool call the run makes (and,
// in a fan-out, its children, which share one pool) is charged against the ceiling,
// and an action is refused once it is reached. A zero-limit budget is unlimited, so
// passing it leaves the run uncapped.
func withBudget(l budgetpkg.Limits) driveOption {
	return func(c *driveConfig) { c.budget = l }
}

// withResourceLimits caps the host memory and process count of the commands a run's
// tools execute, on top of the always-on wall-clock and process-tree containment. The
// zero value applies no cap, so passing it leaves a run's commands unconstrained. See
// sandbox.ResourceLimits for the per-platform enforcement.
func withResourceLimits(r sandbox.ResourceLimits) driveOption {
	return func(c *driveConfig) { c.resLimits = r }
}

// withExternalAgent drives the run through an external agent CLI backend (its own
// harness runs the loop) instead of a native model conversation. The model llm.Model
// passed to drive is unused in this mode: the external CLI drives its own model,
// selected by the agent's model string. Every tool call the CLI makes still comes back
// through the same dispatch waist, so governance is unchanged.
func withExternalAgent(ea *externAgent) driveOption {
	return func(c *driveConfig) { c.extAgent = ea }
}

// withToolset drives the run over a caller-supplied toolset and grant instead of
// the sandboxed working-tree tools. This is how a specialised run (a pull-request
// review) holds exactly the authority its archetype declares: the toolset carries
// no shell and no filesystem, and the grant it comes with is the complete list of
// what the waist admits. The working directory is untouched; a toolset run never
// reads or writes the tree.
func withToolset(t *boundToolset) driveOption {
	return func(c *driveConfig) { c.toolset = t }
}

// withEventObserver invokes fn on every session event as the run streams, in
// addition to rendering. A caller uses it to read an outcome off the run's own
// recorded events (a submitted review verdict, say) rather than re-deriving it
// out of band, so what the caller acts on and what the record says cannot differ.
func withEventObserver(fn func(session.Event)) driveOption {
	return func(c *driveConfig) { c.observe = fn }
}

// boundToolset is a toolset paired with the grant that bounds it. They travel
// together so a caller cannot hand drive a toolset while forgetting the authority
// that is supposed to confine it: the default-permissive trap this pairing exists
// to avoid.
type boundToolset struct {
	tools []mission.Tool
	grant capability.Grant
}

// budgeted reports whether a ceiling is set on any axis.
func (c driveConfig) budgeted() bool { return c.budget.Tokens > 0 || c.budget.Cost > 0 }

// drive assembles the runtime over the given store and the sandboxed toolset,
// streams the session live to out, and returns the converged result, the session
// id (used as learning provenance), and the conversation transcript (so the
// distiller can learn from how the goal was reached, not just the final summary).
// The system prompt is supplied so the caller can fold recalled knowledge into it.
func drive(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, workdir, objective, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, verbose bool, resumeID string, fanout *fanoutConfig, opts ...driveOption) (result, source string, transcript []llm.Message, err error) {
	w := &syncWriter{w: out}
	var cfg driveConfig
	for _, o := range opts {
		o(&cfg)
	}
	// A run with fan-out enabled drives the full goals engine (the Router plus a
	// delegation spawner); otherwise it is a single governed conversation. Both seal
	// into the same verifiable record, so fan-out adds delegation without changing how
	// a run is recorded or checked.
	var run *missionRun
	switch {
	case cfg.toolset != nil:
		// A caller-supplied toolset and grant: no sandbox, no working-tree tools, the
		// same session recording and governance as every other path.
		run, err = assembleToolsetMission(model, plan, cfg.toolset, system, rstore, jq, log, resumeID)
	case cfg.extAgent != nil:
		// An external agent CLI drives the loop: the same sandbox, session, toolset,
		// grant, and governance recording as a native run, but the run loop is the CLI's
		// episode driver rather than a model conversation.
		run, err = assembleExternalMission(cfg.extAgent, workdir, system, rstore, jq, log, resumeID, cfg.resLimits)
	case fanout != nil:
		run, err = assembleFanoutMission(model, plan, workdir, system, rstore, jq, log, resumeID, fanout.resolveModel, cfg.resLimits)
	default:
		run, err = assembleMission(model, plan, workdir, system, rstore, jq, log, resumeID, cfg.resLimits)
	}
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = run.Close() }()
	_, _ = fmt.Fprintf(w, "  run %s\n", run.sess.ID())

	// Open the run's spend pool before the goal is submitted, so the ceiling is in
	// force from the first action rather than after a race. The pool is keyed by the
	// run id (the root goal's name, which equals the session id), and every fan-out
	// child inherits it, so one budget bounds the whole run. Without a ceiling nothing
	// is opened and the always-wired budget hook is inert (an absent pool is unlimited).
	if cfg.budgeted() {
		if _, oerr := budgetpkg.NewLedger(rstore).Open(ctx, run.sess.ID(), resource.Scope{}, cfg.budget); oerr != nil {
			return "", "", nil, fmt.Errorf("open run budget: %w", oerr)
		}
	}

	// Record the run's own outbound-network decisions onto its stream: seed the driving
	// context with an egress observer bound to the run's stream, so every dial netguard
	// makes on the run's behalf (a hosted model's API call, say) reports its allow/block
	// verdict into the same recorded history as the run's governed actions. A run whose
	// model is local makes no netguard-gated dial, so nothing is recorded.
	egress := spinesink.NewEgress(log, run.sess.ID())
	runCtx, cancel := context.WithCancel(netguard.WithObserver(ctx, egress.Observe))
	defer cancel()
	done := make(chan struct{})
	go func() { _ = run.rt.Start(runCtx); close(done) }()

	events, err := run.sess.Subscribe(runCtx, 0)
	if err != nil {
		return "", "", nil, err
	}
	if resumeID != "" {
		// Continue an existing run: re-drive its goal (preserving its recorded
		// progress) rather than opening a new one. Subscribe above replays the prior
		// conversation first, then tails the rest live.
		g, err := run.rt.Resume(runCtx, resumeID)
		if err != nil {
			return "", "", nil, err
		}
		run.sess.Resume(runCtx, run.rt, g.Key())
	} else if _, err := run.sess.Submit(runCtx, run.rt, goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
		// The model the loop drives. Empty for a native run (the host default model
		// applies); for an external agent it is the CLI's model string, which the episode
		// driver hands the CLI so `flynn --model codex:<model>` pins that model.
		Model: externalModel(cfg.extAgent),
		// A fan-out parent spends steps it would not as a single conversation: a step
		// dispatching each delegation, and a step per poll while it waits for the
		// children to finish (a wait makes no model call, but the reconciler still
		// counts it). Give it a larger budget so a legitimate delegation that waits on
		// a few children is not cut off mid-fold; a single conversation keeps the
		// default. The safety brake and fan-out width still bound a runaway.
		MaxSteps: fanoutMaxSteps(fanout),
	}); err != nil {
		return "", "", nil, err
	}

	result, transcript, _, runErr := renderStream(w, events, verbose, cfg.observe)

	// Declare the run's provenance onto its stream before it is sealed, when an external
	// agent harness drove the loop. The record then vouches for enforced effects (every
	// tool call crossed the dispatch waist) while naming the harness's inner reasoning as
	// an unobserved gap, so an external run never claims the integrity of a native one.
	// It is appended before cancel() so the run context is still live; a native run
	// records nothing. Best effort: a failure to record it is reported, not fatal.
	//
	// A failed run declares its provenance too. The declaration's absence is what marks a
	// record as natively driven, so omitting it on failure would seal a broken external
	// episode as though Flynn's own loop had run it: the exact overclaim this declaration
	// exists to prevent.
	if cfg.extAgent != nil {
		if perr := appendProvenance(runCtx, log, run.sess.ID(), observedProvenance(cfg.extAgent)); perr != nil {
			_, _ = fmt.Fprintf(w, "  (provenance not recorded: %v)\n", perr)
		}
		// A harness event the record could not hold is a hole in the harness's account. The
		// declared count still names every event the harness reported, so verify reports the
		// gap from the record alone; saying it here tells the operator why.
		if lost, lerr := unrecordedAttested(cfg.extAgent); lost > 0 {
			_, _ = fmt.Fprintf(w, "  (%d attested event(s) not recorded: %v)\n", lost, lerr)
		}
	}

	cancel()
	<-done
	return result, run.sess.ID(), transcript, runErr
}

// missionRun is an assembled goal runtime paired with the session that records it
// onto the spine. The caller starts rt (rt.Start in a goroutine), then submits or
// resumes a goal through sess; cancelling the context stops the control plane.
type missionRun struct {
	rt   *runtime.Runtime
	sess *session.Session
	// parts holds the ingredients whose lifetime is the run's, so closing the run closes
	// them. The sandbox among them registers operating-system objects that outlive the
	// process unless they are released.
	parts *missionParts
}

// Close releases the run's sandbox. A caller owns a missionRun for the length of one run
// (a goal, a resumed run, an interactive session, a served conversation) and closes it
// when that run ends.
func (m *missionRun) Close() error {
	if m == nil {
		return nil
	}
	return m.parts.Close()
}

// missionParts is the set of ingredients every one-shot runtime assembly shares:
// the confined sandbox, the session that records the run onto the spine, the
// sandboxed toolset, the capability grant that bounds every action the run may take,
// and the governance event sink. Both the single-conversation runner and the fan-out
// runner build from one of these, so the sandbox confinement, tool surface,
// governance recording, and (the reason this is one function) the grant that decides
// what a run is allowed to do are defined once and cannot drift between the two paths.
type missionParts struct {
	sandbox *sandbox.Local
	sess    *session.Session
	toolset []mission.Tool
	grant   capability.Grant
	sink    *spinesink.Sink
}

// Close releases the run's sandbox, which is what hands back the operating-system objects
// its confinement registered (on Windows, the container profile whose identity outlives
// the process that made it). Every caller of newMissionParts owns the parts for the length
// of a run and closes them when it ends.
func (p *missionParts) Close() error {
	if p == nil || p.sandbox == nil {
		return nil
	}
	return p.sandbox.Close()
}

// newMissionParts wires the shared ingredients for a run at workdir recording onto
// log under runID (empty gets a fresh one). withSpawn adds the delegation action to
// the grant, so a fan-out run may spawn sub-goals and a single conversation cannot;
// everything else is identical across the two paths. resLimits caps the host memory
// and process count of the commands the run's tools execute (its zero value applies
// no cap).
func newMissionParts(workdir string, log spine.Log, runID string, withSpawn bool, resLimits sandbox.ResourceLimits) (*missionParts, error) {
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement(), sandbox.WithResourceLimits(resLimits))
	if err != nil {
		return nil, err
	}

	var sopts []session.Option
	if runID != "" {
		sopts = append(sopts, session.WithID(runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)

	toolset := tools.New(sb).Tools()
	// Optional capabilities mount here through the registry each one self-registers
	// into under its own build tag (none in the default build), so their actions flow
	// into the grant below exactly like a built-in tool and adding a capability never
	// touches this assembly. A capability that is configured but cannot be built fails
	// the run rather than silently vanishing.
	extraTools, err := optionalTools()
	if err != nil {
		_ = sb.Close()
		return nil, err
	}
	toolset = append(toolset, extraTools...)
	// The grant lists every action the run may take: the tools, plus the model call
	// and the distillation, and (for a fan-out) the spawn that delegates a sub-goal. A
	// child narrows from this set, so a delegation can never widen authority; a run
	// whose grant omitted spawn could not fan out at all. Assembling the action set in
	// one place is the point: the single and fan-out paths cannot grant different
	// authority by drift.
	names := make([]string, 0, len(toolset)+3)
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	names = append(names, mission.ActionModelGenerate, learn.DistillAction)
	if withSpawn {
		names = append(names, mission.ActionSpawn)
	}

	return &missionParts{
		sandbox: sb,
		sess:    sess,
		toolset: toolset,
		grant:   capability.NewGrant(names...),
		sink:    spinesink.New(log, sess.ID()),
	}, nil
}

// runtimeConfig returns the runtime.Config both assemblies share, with the caller's
// executor and stop condition dropped in. Every one-shot CLI run polls at the same
// cadence and drives only its own submitted goal (and, for a fan-out, the children
// it spawns) - never a parked goal an earlier run left non-terminal, which would
// contaminate this run's stream and silently resume unrelated work. Resuming a parked
// run, or continuing a session turn, is always explicit.
func (p *missionParts) runtimeConfig(exec goal.StepExecutor, stop goal.StopEvaluator, rstore resource.Store, jq jobs.Queue) runtime.Config {
	return runtime.Config{
		Executor:           exec,
		Stop:               stop,
		Store:              rstore,
		Jobs:               jq,
		PollInterval:       200 * time.Millisecond,
		WorkerPoll:         50 * time.Millisecond,
		DriveSubmittedOnly: true,
	}
}

// assembleMission wires one goal runtime over the durable store ports and the
// sandboxed toolset at workdir, with a session recording the run onto the spine.
// runID names both the session's event stream and (via Submit/Resume) its goal
// resource, so a single id addresses the whole run for replay, audit, and resume; an
// empty runID gets a fresh one. The system prompt is supplied so the caller can fold
// recalled knowledge into it. It is the shared assembly behind the one-shot runner,
// resume, and the interactive session, so none of them reassembles the runtime by
// hand.
func assembleMission(model llm.Model, plan harness.Plan, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string, resLimits sandbox.ResourceLimits) (*missionRun, error) {
	parts, err := newMissionParts(workdir, log, runID, false, resLimits)
	if err != nil {
		return nil, err
	}

	opts := []mission.Option{
		mission.WithTools(parts.toolset...),
		mission.WithSystem(system),
		mission.WithObserver(parts.sess.Reporter()),
		mission.WithGrant(parts.grant),
		// Charge every action against the run's spend pool, so a ceiling set for the run
		// (flynn run --max-cost/--max-tokens) halts it once reached. It is inert until a
		// budget is opened for the run: a pool with no budget resource is unlimited, so a
		// run without a ceiling is unchanged, and a resumed run honours the durable budget
		// its first run opened.
		mission.WithBudget(budgetpkg.NewHook(rstore)),
		// Halt a runaway from outside the model loop: the same circuit breaker the
		// fan-out runs under, so a single conversation cannot spin unbounded (a jailbroken
		// or looping model hammering a tool) any more than a delegating one can. The rate
		// is a generous backstop that never trips on legitimate use.
		mission.WithBrakes(defaultBrakes()),
		// Record every governed action's lifecycle (admitted, completed, or rejected)
		// onto the run's own stream, so the admission decisions are part of the run's
		// recorded and sealed history rather than only the live trace. The stream is the
		// session's, so governance events interleave with the run's other events in one
		// ordered log.
		mission.WithEventSink(parts.sink),
		// Compact the transcript when it grows past this budget so a long session
		// stays affordable and clear of the context limit. It is a conservative floor
		// for a model whose window is unknown; the plan below tightens it to a model
		// with a measured, narrower effective context.
		mission.WithCompactionBudget(defaultCompactionBudget),
	}
	// Apply the model's scaffolding plan last so a present field (a tighter context
	// budget, simplified schemas, verify passes) overrides the lean defaults, while an
	// absent one (the zero plan of a strong model) leaves them in place.
	opts = append(opts, mission.PlanOptions(plan)...)
	exec := mission.NewExecutor(model, opts...)
	rt, err := runtime.New(parts.runtimeConfig(exec, mission.Convergence{}, rstore, jq))
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

// assembleToolsetMission wires one goal runtime over a caller-supplied toolset and
// the grant bound to it, recording onto the spine exactly as assembleMission does.
// There is no sandbox: the toolset holds every action the run may take, and the
// grant it arrived with is the complete authority the waist consults. Budget,
// brakes, governance recording, and compaction are identical to the sandboxed
// path, so a specialised run is not a less-governed run.
func assembleToolsetMission(model llm.Model, plan harness.Plan, ts *boundToolset, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string) (*missionRun, error) {
	var sopts []session.Option
	if runID != "" {
		sopts = append(sopts, session.WithID(runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)
	parts := &missionParts{
		sess:    sess,
		toolset: ts.tools,
		grant:   ts.grant,
		sink:    spinesink.New(log, sess.ID()),
	}

	opts := []mission.Option{
		mission.WithTools(parts.toolset...),
		mission.WithSystem(system),
		mission.WithObserver(parts.sess.Reporter()),
		mission.WithGrant(parts.grant),
		mission.WithBudget(budgetpkg.NewHook(rstore)),
		mission.WithBrakes(defaultBrakes()),
		mission.WithEventSink(parts.sink),
		mission.WithCompactionBudget(defaultCompactionBudget),
	}
	opts = append(opts, mission.PlanOptions(plan)...)
	exec := mission.NewExecutor(model, opts...)
	rt, err := runtime.New(parts.runtimeConfig(exec, mission.Convergence{}, rstore, jq))
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

// externalModel returns the model string an external agent CLI should drive, or empty
// when the run is native. It is set on the submitted goal so the episode driver hands
// it to the CLI.
func externalModel(ea *externAgent) string {
	if ea == nil {
		return ""
	}
	return ea.model
}

// assembleExternalMission wires a one-shot run whose loop is an external agent CLI: it
// shares the same sandbox, session, toolset, capability grant, and governance
// recording as assembleMission (via newMissionParts), but builds the run loop from the
// external agent's driver instead of a native model executor. The driver's episode
// loop routes every tool call the CLI makes back through the same dispatch waist, so
// the grant, containment gate, safety brake, and spend ceiling bound the external
// harness exactly as they bound a native loop; the CLI's own inner model calls stay
// unobserved-but-contained and are recorded as a declared provenance gap. A run driven
// this way does not fan out (the external harness owns its own loop), so the spawn
// action is withheld from the grant.
func assembleExternalMission(ea *externAgent, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string, resLimits sandbox.ResourceLimits) (*missionRun, error) {
	parts, err := newMissionParts(workdir, log, runID, false, resLimits)
	if err != nil {
		return nil, err
	}

	// Record the harness's own account of its episode onto the same stream the waist
	// records the run's enforced effects on. The stream exists only now (the driver was
	// built during detection, before a run was assembled), which is why the sink is bound
	// here rather than at construction.
	recordAttestedEvents(ea, log, parts.sess.ID())

	// The loop-agnostic Spec the driver builds from: the same governance ingredients a
	// native run assembles, carried to the external episode loop. Model is intentionally
	// unset (an llm.Model), since the external CLI drives its own model, selected by the
	// goal's model string; the compaction budget and scaffolding plan are native-loop
	// concerns the external CLI manages itself, so they are left unset.
	exec, stop, err := ea.driver.Build(driver.Spec{
		Tools:     parts.toolset,
		System:    system,
		Grant:     parts.grant,
		HasGrant:  true,
		Sandbox:   parts.sandbox,
		Reporter:  parts.sess.Reporter(),
		EventSink: parts.sink,
		Brakes:    defaultBrakes(),
		Budget:    budgetpkg.NewHook(rstore),
	})
	if err != nil {
		return nil, err
	}
	rt, err := runtime.New(parts.runtimeConfig(exec, stop, rstore, jq))
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

// defaultFanoutWidth caps how many child runs a fan-out may have outstanding at
// once, bounding the blast radius of delegation alongside the depth guard.
const defaultFanoutWidth = 8

// fanoutRootMaxSteps is the step budget a fan-out parent runs under. A fan-out adds
// orchestration steps a single conversation never takes (a dispatch per delegation,
// and one poll per reconcile while it waits for the children), so the default budget
// that suits a single loop would cut a legitimate delegation off mid-fold. Zero
// (single conversation) keeps the reconciler's default.
const fanoutRootMaxSteps = 200

// fanoutMaxSteps returns the step budget for a run's root goal: the larger fan-out
// budget when delegation is enabled, or zero (the reconciler's default) otherwise.
func fanoutMaxSteps(fanout *fanoutConfig) int {
	if fanout != nil {
		return fanoutRootMaxSteps
	}
	return 0
}

// defaultMaxActionsPerMinute is the rate the default safety brake halts a run at.
// It is set well above any real run's pace so it catches only a degenerate tight
// loop, not legitimate tool use.
const defaultMaxActionsPerMinute = 600

// defaultBrakes builds the run's safety governor: the circuit breaker that halts a
// runaway from outside the model loop. It is a generous rate backstop (a real run
// dispatches far fewer than this per minute), so it fires only on a degenerate tight
// loop, never on legitimate tool use. Every run gets one, the single conversation as
// much as the fan-out, so no run can spin unbounded even when the model is jailbroken
// or looping. Each call returns a fresh Hook (with its own in-memory kill-switch), so
// a run's breaker state and halt are its own.
func defaultBrakes() *brakes.Hook {
	return brakes.NewHook(brakes.Limits{MaxActions: defaultMaxActionsPerMinute, Window: time.Minute}, nil)
}

// fanoutConfig enables the goals engine on a one-shot run: the model may delegate
// self-contained sub-goals to concurrent, governed child agents, and each child is
// routed to the model and loop its bound Agent archetype pins (resolveModel turns a
// named model into a client). Every child runs under a grant narrowed from the
// parent's, shares the run's budget, and folds back into the parent's single sealed
// record, so a multi-goal, multi-model fan-out stays one verifiable run. A nil
// *fanoutConfig leaves a run as a single conversation, which is the n=1 case of the
// same mechanism.
type fanoutConfig struct {
	resolveModel driver.ModelResolver
}

// assembleFanoutMission wires a one-shot run that drives the full goals engine over
// the durable store: a Router that builds one loop per (driver, model) a goal
// selects, and a fan-out spawner that creates governed child goals. It mirrors
// assembleMission (same sandbox, session, toolset, grant, governance recording, and
// compaction) but adds the spawn action to the grant and routes each goal through
// the Router, so a delegated child runs as the agent its archetype names, on that
// agent's model, while the root and every child fold into one recorded, sealable
// stream. The shared store backs the child goals a fan-out spawns, so they land
// where the runtime reconciles them.
func assembleFanoutMission(model llm.Model, plan harness.Plan, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string, resolveModel driver.ModelResolver, resLimits sandbox.ResourceLimits) (*missionRun, error) {
	parts, err := newMissionParts(workdir, log, runID, true, resLimits)
	if err != nil {
		return nil, err
	}

	// The spawner is the run's fan-out: it creates governed child goals (owned by the
	// parent, grant narrowed, depth- and concurrency-bounded) and hands them to the
	// runtime. Its enqueue hook is bound once the runtime exists (below).
	spawner := orchestration.NewSpawner(rstore, nil, orchestration.WithConcurrency(defaultFanoutWidth))

	// The Router drives each goal through the loop and model its spec selects: the
	// default loop and host model for the root, and the bound Agent's loop and model
	// for a delegated child. The shared ingredients (tools, default prompt and grant,
	// sandbox gate, governance recording, compaction, brake, fan-out) apply to every
	// loop; the per-goal prompt, grant, and model are applied from the goal.
	router := driver.NewRouter(driver.RouterConfig{
		Registry:     driver.Default(),
		DefaultModel: model,
		ResolveModel: resolveModel,
		Base: driver.Spec{
			Tools:    parts.toolset,
			System:   system,
			Grant:    parts.grant,
			HasGrant: true,
			Sandbox:  parts.sandbox,
			Reporter: parts.sess.Reporter(),
			Fanout:   spawner,
			// Record every governed action's lifecycle onto the run's own stream, so the
			// admission decisions (including each delegation) are part of the sealed record.
			EventSink:        parts.sink,
			CompactionBudget: defaultCompactionBudget,
			// Halt a runaway from outside the model loop: the same circuit breaker the
			// single conversation runs under, shared by every child (which run under this
			// pool), so the whole fan-out is braked as one.
			Brakes: defaultBrakes(),
			// Charge every action (root and every child, which share one pool) against the
			// run's spend pool, so a ceiling set for the run halts the whole fan-out. Inert
			// until a budget is opened: an absent pool is unlimited.
			Budget: budgetpkg.NewHook(rstore),
			// Apply the model's scaffolding plan so a weaker model is driven with the
			// support it needs; the zero plan of a strong model adds nothing.
			Plan: plan,
		},
	})

	rt, err := runtime.New(parts.runtimeConfig(router, router, rstore, jq))
	if err != nil {
		return nil, err
	}
	// Bind the spawner to the runtime so a spawned child is enqueued for
	// reconciliation. Binding here (rather than at construction) breaks the cycle: the
	// executor holds the spawner, and the runtime holds the executor.
	spawner.SetEnqueue(func(ctx context.Context, key resource.Key) error {
		_, rerr := rt.Resume(ctx, key.Name)
		return rerr
	})
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

// renderStream prints the session's events as they arrive and accumulates the
// conversation transcript (the model's text and the tools it called), returning
// once the session reaches a terminal event: the model's summary on convergence,
// or an error on stall. lastSeq is the sequence of the last event consumed, so a
// caller tailing the same stream across turns can resume after it. A closed channel
// before any terminal event means the run was cancelled.
//
// observe, when non-nil, is called with every event before it is rendered to out.
// It is the tap an interactive client uses to render the typed stream itself (a
// themed transcript, a status badge) rather than the flat text this writes; the
// text path still runs, so a caller that wants only the typed events points out at
// io.Discard.
func renderStream(out io.Writer, events <-chan session.Event, verbose bool, observe func(session.Event)) (result string, transcript []llm.Message, lastSeq int64, err error) {
	var meter usageMeter
	for ev := range events {
		lastSeq = ev.Seq
		if observe != nil {
			observe(ev)
		}
		renderEvent(out, ev, verbose)
		if ev.Usage != nil {
			meter.add(*ev.Usage)
		}
		switch ev.Kind {
		case session.KindAssistant:
			transcript = append(transcript, llm.Text(llm.RoleAssistant, ev.Text))
		case session.KindToolCall:
			transcript = append(transcript, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: ev.ToolUseID, Name: ev.Tool, Input: ev.Input}},
			}})
		case session.KindConverged:
			renderUsageSummary(out, meter)
			return ev.Text, transcript, lastSeq, nil
		case session.KindStalled:
			// Show the spend even on failure: a run that stalled still cost tokens,
			// and that is exactly when the number is worth seeing.
			renderUsageSummary(out, meter)
			return "", transcript, lastSeq, fmt.Errorf("goal stalled: %s", ev.Err)
		default:
			// Already drawn by renderEvent above; only the kinds that build the
			// transcript or end the stream need handling here.
		}
	}
	return "", transcript, lastSeq, context.Canceled
}

// syncWriter serializes writes, so the stream-rendering goroutine and any other
// writer never interleave or race on the underlying writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
