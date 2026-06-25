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

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
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
const defaultSystemPrompt = `You are Flynn, an autonomous software agent working inside a sandboxed working directory.
You have tools to run shell commands and to read, write, edit, glob, and grep files; every command and file path is confined to the working directory.
Work toward the objective directly: inspect what you need, make the changes, and verify them with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// recallLimit caps how many learned skills and memory items are injected into a
// run's prompt. It is deliberately small: recall is precision-first, since a long,
// loosely-relevant context degrades the model's use of it more than it helps.
const recallLimit = 5

// openStore opens the durable SQLite store at dsn, or an ephemeral in-memory one
// when dsn is empty (used by tests and one-off runs). The same store backs the
// runtime's resources and job queue and the learning loop's skills and memory.
func openStore(ctx context.Context, dsn string) (*sqlite.Store, error) {
	if dsn == "" {
		dsn = ":memory:"
	}
	return sqlite.Open(ctx, dsn)
}

// openDataStore opens the durable store under a data directory, creating the
// directory and resolving the database file inside it. An empty or ":memory:"
// dataDir opens an ephemeral store.
func openDataStore(ctx context.Context, dataDir string) (*sqlite.Store, error) {
	if dataDir != "" && dataDir != ":memory:" {
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			return nil, err
		}
		dataDir = filepath.Join(dataDir, "flynn.db")
	}
	return openStore(ctx, dataDir)
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
	for _, ev := range events {
		renderEvent(os.Stdout, ev, verbose)
	}
	return nil
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
// the core kinds plus the Goal kind the runtime drives.
func missionRegistry() (*resource.Registry, error) {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		return nil, err
	}
	if err := goal.RegisterKind(reg); err != nil {
		return nil, err
	}
	return reg, nil
}

// runLearningMission runs one objective end to end over a durable store: it recalls
// what past runs learned into the prompt, drives the goal to a result through the
// sandboxed toolset, and (when a distiller is supplied) distills the converged run
// back into skills and memory so the next run starts ahead. Progress is written to
// out; the model's final summary is returned.
func runLearningMission(ctx context.Context, out io.Writer, model llm.Model, distiller learn.Distiller, workdir, objective string, store *sqlite.Store, verbose bool) (string, error) {
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

	result, source, transcript, err := drive(ctx, out, model, workdir, objective, system, store.Resources(reg), store.Jobs(), store.Log(), verbose, "")

	// Reinforce the recalled skills by the run's outcome: a skill present in a run
	// that converged earns a win; one in a run that failed earns only a use. This is
	// gated with capture (a read-only --no-learn run records nothing).
	if distiller != nil && len(recalled) > 0 {
		_ = learn.Reinforce(ctx, skills, recalled, err == nil)
	}
	if err != nil {
		return "", err
	}

	// Capture: distill the converged run back into durable, provenance-stamped
	// knowledge. A captured skill's check is run in a sandbox at the working
	// directory before it is crystallized, so a broken procedure is dropped rather
	// than learned. Capture failures never fail the run; learning is best effort.
	if distiller != nil {
		curator := learn.NewCurator(distiller, skills, memories, learn.WithVerifier(governedVerifier(workdir)))
		captured, err := curator.Curate(ctx, learn.Outcome{
			Objective:  objective,
			Result:     result,
			Transcript: transcript,
			Converged:  true,
			Source:     source,
		})
		if err == nil {
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
	return result, nil
}

// governedVerifier builds the skill-check verifier the CLI uses: a sandbox verifier
// that runs each check at dir, wrapped so the check is dispatched through the waist.
// Routing it through dispatch means a verification is admitted against the run's
// grant and traced like every tool call, rather than executing a model-proposed
// command on a side channel that bypasses governance. With no grant bound the
// admitter is permissive, so a standalone run still verifies, just ungoverned.
func governedVerifier(dir string) learn.Verifier {
	inner := learn.NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(dir)
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
func drive(ctx context.Context, out io.Writer, model llm.Model, workdir, objective, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, verbose bool, resumeID string) (result, source string, transcript []llm.Message, err error) {
	sb, err := sandbox.NewLocal(workdir)
	if err != nil {
		return "", "", nil, err
	}
	w := &syncWriter{w: out}

	// The session records the run on the durable spine, keyed by a stable run id
	// that also names the goal resource (see session.Submit), so the run survives
	// the process and is addressable for replay and audit. Resuming binds the
	// session to the existing run's id, so the continuation lands on the same stream.
	var sopts []session.Option
	if resumeID != "" {
		sopts = append(sopts, session.WithID(resumeID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)
	_, _ = fmt.Fprintf(w, "  run %s\n", sess.ID())

	toolset := tools.New(sb).Tools()
	// The grant lists every action the run may take: the tools, plus the model call
	// and the distillation, so each is admitted and the grant stays the complete
	// record of what this run can do.
	names := make([]string, 0, len(toolset)+2)
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	names = append(names, mission.ActionModelGenerate, learn.DistillAction)

	exec := mission.NewExecutor(
		model,
		mission.WithTools(toolset...),
		mission.WithSystem(system),
		mission.WithObserver(sess.Reporter()),
		mission.WithGrant(capability.NewGrant(names...)),
	)
	rt, err := runtime.New(runtime.Config{
		Executor:     exec,
		Stop:         mission.Convergence{},
		Store:        rstore,
		Jobs:         jq,
		PollInterval: 200 * time.Millisecond,
		WorkerPoll:   50 * time.Millisecond,
		// A one-shot CLI run drives only its own goal; it must not adopt a goal an
		// earlier run left non-terminal (which would contaminate this run's stream
		// and silently resume unrelated work). Resuming a parked run is explicit.
		DriveSubmittedOnly: true,
	})
	if err != nil {
		return "", "", nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(runCtx); close(done) }()

	events, err := sess.Subscribe(runCtx, 0)
	if err != nil {
		return "", "", nil, err
	}
	if resumeID != "" {
		// Continue an existing run: re-drive its goal (preserving its recorded
		// progress) rather than opening a new one. Subscribe above replays the prior
		// conversation first, then tails the rest live.
		g, err := rt.Resume(runCtx, resumeID)
		if err != nil {
			return "", "", nil, err
		}
		sess.Resume(runCtx, rt, g.Key())
	} else if _, err := sess.Submit(runCtx, rt, goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
	}); err != nil {
		return "", "", nil, err
	}

	result, transcript, runErr := renderStream(w, events, verbose)
	cancel()
	<-done
	return result, sess.ID(), transcript, runErr
}

// renderStream prints the session's events as they arrive and accumulates the
// conversation transcript (the model's text and the tools it called), returning
// once the session reaches a terminal event: the model's summary on convergence,
// or an error on stall. A closed channel before any terminal event means the run
// was cancelled.
func renderStream(out io.Writer, events <-chan session.Event, verbose bool) (string, []llm.Message, error) {
	var transcript []llm.Message
	for ev := range events {
		renderEvent(out, ev, verbose)
		switch ev.Kind {
		case session.KindAssistant:
			transcript = append(transcript, llm.Text(llm.RoleAssistant, ev.Text))
		case session.KindToolCall:
			transcript = append(transcript, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: ev.ToolUseID, Name: ev.Tool, Input: ev.Input}},
			}})
		case session.KindConverged:
			return ev.Text, transcript, nil
		case session.KindStalled:
			return "", transcript, fmt.Errorf("goal stalled: %s", ev.Err)
		default:
			// Already drawn by renderEvent above; only the kinds that build the
			// transcript or end the stream need handling here.
		}
	}
	return "", transcript, context.Canceled
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
