package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/dependency"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/playbook"
	"github.com/ionalpha/flynn/profilestore"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/service"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spinesink"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
	"github.com/ionalpha/flynn/tools"
)

// defaultSystemPrompt frames the agent for a coding/automation task. It is kept
// short on purpose: a capable model works better from a clear goal than from a long
// list of rules.
const defaultSystemPrompt = `You are Flynn, an autonomous software agent working inside a sandboxed working directory.
You have tools to run shell commands and to read, write, edit, glob, and grep files; every command and file path is confined to the working directory.
Work toward the objective directly: inspect what you need, make the changes, and verify them with the tools rather than guessing.
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
func openStore(ctx context.Context, dsn string) (*sqlite.Store, error) {
	if dsn == "" {
		dsn = ":memory:"
	}
	return sqlite.Open(ctx, dsn)
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
func openDataStore(ctx context.Context, dataDir string) (*sqlite.Store, error) {
	dsn := dataStoreFile(dataDir)
	if dsn != "" {
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			return nil, err
		}
	}
	return openStore(ctx, dsn)
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
func runLearningMission(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, distiller learn.Distiller, workdir, objective, verify string, store *sqlite.Store, signer chain.RootSigner, verbose bool, fanout *fanoutConfig) (string, error) {
	reg, err := missionRegistry()
	if err != nil {
		return "", err
	}
	skills, memories := store.Skills(), store.Memory()

	// Recall first: fold what was learned before into the standing instructions, and
	// remember which skills were surfaced so the run's outcome can reinforce them.
	system := defaultSystemPrompt
	block, recalled := recallContext(ctx, skills, memories, objective)
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

	result, source, transcript, err := drive(ctx, out, model, plan, workdir, objective, system, store.Resources(reg), store.Jobs(), log, verbose, "", fanout)

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
func recallContext(ctx context.Context, skills state.SkillStore, memories state.MemoryStore, objective string) (block string, recalled []string) {
	terms := keywords(objective)
	if len(terms) == 0 {
		return "", nil
	}
	sk := rankSkills(terms, gatherSkills(ctx, skills, terms))
	mem := rankMemory(terms, gatherMemory(ctx, memories, terms))
	if len(sk) == 0 && len(mem) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("From earlier runs you have learned the following. Use anything relevant; ignore the rest.")
	if len(sk) > 0 {
		b.WriteString("\nSkills:")
		for _, s := range sk {
			fmt.Fprintf(&b, "\n- %s: %s", s.Name, truncate(s.Body, 240))
			recalled = append(recalled, s.Slug)
		}
	}
	if len(mem) > 0 {
		b.WriteString("\nMemory:")
		for _, m := range mem {
			fmt.Fprintf(&b, "\n- %s", truncate(m.Content, 240))
		}
	}
	return b.String(), recalled
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
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "..."
}

// drive assembles the runtime over the given store and the sandboxed toolset,
// streams the session live to out, and returns the converged result, the session
// id (used as learning provenance), and the conversation transcript (so the
// distiller can learn from how the goal was reached, not just the final summary).
// The system prompt is supplied so the caller can fold recalled knowledge into it.
func drive(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, workdir, objective, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, verbose bool, resumeID string, fanout *fanoutConfig) (result, source string, transcript []llm.Message, err error) {
	w := &syncWriter{w: out}
	// A run with fan-out enabled drives the full goals engine (the Router plus a
	// delegation spawner); otherwise it is a single governed conversation. Both seal
	// into the same verifiable record, so fan-out adds delegation without changing how
	// a run is recorded or checked.
	var run *missionRun
	if fanout != nil {
		run, err = assembleFanoutMission(model, plan, workdir, system, rstore, jq, log, resumeID, fanout.resolveModel)
	} else {
		run, err = assembleMission(model, plan, workdir, system, rstore, jq, log, resumeID)
	}
	if err != nil {
		return "", "", nil, err
	}
	_, _ = fmt.Fprintf(w, "  run %s\n", run.sess.ID())

	runCtx, cancel := context.WithCancel(ctx)
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

	result, transcript, _, runErr := renderStream(w, events, verbose)
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
}

// assembleMission wires one goal runtime over the durable store ports and the
// sandboxed toolset at workdir, with a session recording the run onto the spine.
// runID names both the session's event stream and (via Submit/Resume) its goal
// resource, so a single id addresses the whole run for replay, audit, and resume; an
// empty runID gets a fresh one. The system prompt is supplied so the caller can fold
// recalled knowledge into it. It is the shared assembly behind the one-shot runner,
// resume, and the interactive session, so none of them reassembles the runtime by
// hand.
func assembleMission(model llm.Model, plan harness.Plan, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string) (*missionRun, error) {
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		return nil, err
	}

	var sopts []session.Option
	if runID != "" {
		sopts = append(sopts, session.WithID(runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)

	toolset := tools.New(sb).Tools()
	// The grant lists every action the run may take: the tools, plus the model call
	// and the distillation, so each is admitted and the grant stays the complete
	// record of what this run can do.
	names := make([]string, 0, len(toolset)+2)
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	names = append(names, mission.ActionModelGenerate, learn.DistillAction)

	opts := []mission.Option{
		mission.WithTools(toolset...),
		mission.WithSystem(system),
		mission.WithObserver(sess.Reporter()),
		mission.WithGrant(capability.NewGrant(names...)),
		// Record every governed action's lifecycle (admitted, completed, or rejected)
		// onto the run's own stream, so the admission decisions are part of the run's
		// recorded and sealed history rather than only the live trace. The stream is the
		// session's, so governance events interleave with the run's other events in one
		// ordered log.
		mission.WithEventSink(spinesink.New(log, sess.ID())),
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
	rt, err := runtime.New(runtime.Config{
		Executor:     exec,
		Stop:         mission.Convergence{},
		Store:        rstore,
		Jobs:         jq,
		PollInterval: 200 * time.Millisecond,
		WorkerPoll:   50 * time.Millisecond,
		// A CLI run drives only its own goal; it must not adopt a goal an earlier run
		// left non-terminal (which would contaminate this run's stream and silently
		// resume unrelated work). Resuming a parked run, or continuing a session turn,
		// is always explicit.
		DriveSubmittedOnly: true,
	})
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: sess}, nil
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
func assembleFanoutMission(model llm.Model, plan harness.Plan, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string, resolveModel driver.ModelResolver) (*missionRun, error) {
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		return nil, err
	}

	var sopts []session.Option
	if runID != "" {
		sopts = append(sopts, session.WithID(runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)

	toolset := tools.New(sb).Tools()
	// The grant lists every action the run may take: the tools, the model call, the
	// distillation, and the spawn that delegates a sub-goal. A child narrows from this
	// set, so a delegation can never widen authority; a run whose grant omitted spawn
	// could not fan out at all.
	names := make([]string, 0, len(toolset)+3)
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	names = append(names, mission.ActionModelGenerate, learn.DistillAction, mission.ActionSpawn)

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
			Tools:    toolset,
			System:   system,
			Grant:    capability.NewGrant(names...),
			HasGrant: true,
			Sandbox:  sb,
			Reporter: sess.Reporter(),
			Fanout:   spawner,
			// Record every governed action's lifecycle onto the run's own stream, so the
			// admission decisions (including each delegation) are part of the sealed record.
			EventSink:        spinesink.New(log, sess.ID()),
			CompactionBudget: defaultCompactionBudget,
			// Halt a runaway from outside the model loop. The default is a generous rate
			// backstop: a real run dispatches far fewer than this per minute, so the breaker
			// fires only on a degenerate tight loop, never on legitimate tool use.
			Brakes: brakes.NewHook(brakes.Limits{MaxActions: defaultMaxActionsPerMinute, Window: time.Minute}, nil),
			// Apply the model's scaffolding plan so a weaker model is driven with the
			// support it needs; the zero plan of a strong model adds nothing.
			Plan: plan,
		},
	})

	rt, err := runtime.New(runtime.Config{
		Executor:     router,
		Stop:         router,
		Store:        rstore,
		Jobs:         jq,
		PollInterval: 200 * time.Millisecond,
		WorkerPoll:   50 * time.Millisecond,
		// A CLI run drives only its own goal and the children it spawns (enqueued
		// explicitly below), never a parked goal an earlier run left non-terminal.
		DriveSubmittedOnly: true,
	})
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
	return &missionRun{rt: rt, sess: sess}, nil
}

// renderStream prints the session's events as they arrive and accumulates the
// conversation transcript (the model's text and the tools it called), returning
// once the session reaches a terminal event: the model's summary on convergence,
// or an error on stall. lastSeq is the sequence of the last event consumed, so a
// caller tailing the same stream across turns can resume after it. A closed channel
// before any terminal event means the run was cancelled.
func renderStream(out io.Writer, events <-chan session.Event, verbose bool) (result string, transcript []llm.Message, lastSeq int64, err error) {
	var meter usageMeter
	for ev := range events {
		lastSeq = ev.Seq
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
