package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
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
func runLearningMission(ctx context.Context, out io.Writer, model llm.Model, distiller learn.Distiller, workdir, objective string, store *sqlite.Store) (string, error) {
	reg, err := missionRegistry()
	if err != nil {
		return "", err
	}
	skills, memories := store.Skills(), store.Memory()

	// Recall first: fold what was learned before into the standing instructions.
	system := defaultSystemPrompt
	if block := recallContext(ctx, skills, memories, objective); block != "" {
		system += "\n\n" + block
	}

	result, source, err := drive(ctx, out, model, workdir, objective, system, store.Resources(reg), store.Jobs())
	if err != nil {
		return "", err
	}

	// Capture: distill the converged run back into durable, provenance-stamped
	// knowledge. A captured skill's check is run in a sandbox at the working
	// directory before it is crystallized, so a broken procedure is dropped rather
	// than learned. Capture failures never fail the run; learning is best effort.
	if distiller != nil {
		verifier := learn.NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
			return sandbox.NewLocal(workdir)
		})
		curator := learn.NewCurator(distiller, skills, memories, learn.WithVerifier(verifier))
		captured, err := curator.Curate(ctx, learn.Outcome{
			Objective: objective,
			Result:    result,
			Converged: true,
			Source:    source,
		})
		if err == nil {
			if n := len(captured.Skills) + len(captured.Memories); n > 0 {
				_, _ = fmt.Fprintf(out, "  (learned %d skill(s), %d memory item(s))\n", len(captured.Skills), len(captured.Memories))
			}
			if d := len(captured.Dropped); d > 0 {
				_, _ = fmt.Fprintf(out, "  (dropped %d unverified skill(s))\n", d)
			}
		}
	}
	return result, nil
}

// recallContext queries the durable skills and memory for what is relevant to the
// objective and renders a compact, bounded block to prepend to the system prompt.
// It returns "" when nothing is on file, so a fresh agent's prompt is unchanged.
//
// The store's full-text search matches a query as a single phrase, so recall runs
// one query per keyword of the objective and unions the hits (deduped, capped).
// This is intentionally a lexical first cut; ranked and vector recall come later.
func recallContext(ctx context.Context, skills state.SkillStore, memories state.MemoryStore, objective string) string {
	terms := keywords(objective)
	if len(terms) == 0 {
		return ""
	}

	var sk []state.Skill
	seenSk := map[string]bool{}
	var mem []state.MemoryItem
	seenMem := map[string]bool{}
	for _, term := range terms {
		if len(sk) < recallLimit {
			if found, err := skills.Search(ctx, term, recallLimit); err == nil {
				for _, s := range found {
					if !seenSk[s.ID] && len(sk) < recallLimit {
						seenSk[s.ID] = true
						sk = append(sk, s)
					}
				}
			}
		}
		if len(mem) < recallLimit {
			if found, err := memories.Recall(ctx, state.RecallQuery{Query: term, Limit: recallLimit}); err == nil {
				for _, m := range found {
					if !seenMem[m.ID] && len(mem) < recallLimit {
						seenMem[m.ID] = true
						mem = append(mem, m)
					}
				}
			}
		}
	}
	if len(sk) == 0 && len(mem) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("From earlier runs you have learned the following. Use anything relevant; ignore the rest.")
	if len(sk) > 0 {
		b.WriteString("\nSkills:")
		for _, s := range sk {
			fmt.Fprintf(&b, "\n- %s: %s", s.Name, truncate(s.Body, 240))
		}
	}
	if len(mem) > 0 {
		b.WriteString("\nMemory:")
		for _, m := range mem {
			fmt.Fprintf(&b, "\n- %s", truncate(m.Content, 240))
		}
	}
	return b.String()
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
// streams the session live to out, and returns the converged result and the
// session id (used as learning provenance). The system prompt is supplied so the
// caller can fold recalled knowledge into it.
func drive(ctx context.Context, out io.Writer, model llm.Model, workdir, objective, system string, rstore resource.Store, jq jobs.Queue) (result, source string, err error) {
	sb, err := sandbox.NewLocal(workdir)
	if err != nil {
		return "", "", err
	}
	w := &syncWriter{w: out}

	sess := session.New(spine.NewMemoryLog(), bus.NewMemory())

	toolset := tools.New(sb).Tools()
	names := make([]string, len(toolset))
	for i, t := range toolset {
		names[i] = t.Def().Name
	}

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
	})
	if err != nil {
		return "", "", err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(runCtx); close(done) }()

	events, err := sess.Subscribe(runCtx, 0)
	if err != nil {
		return "", "", err
	}
	if _, err := sess.Submit(runCtx, rt, goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
	}); err != nil {
		return "", "", err
	}

	result, runErr := renderStream(w, events)
	cancel()
	<-done
	return result, sess.ID(), runErr
}

// renderStream prints the session's events as they arrive and returns once the
// session reaches a terminal event: the model's summary on convergence, or an
// error on stall. A closed channel before any terminal event means the run was
// cancelled.
func renderStream(out io.Writer, events <-chan session.Event) (string, error) {
	for ev := range events {
		switch ev.Kind {
		case session.KindTurnStarted:
			_, _ = fmt.Fprintf(out, "  ... turn %d\n", ev.Turn)
		case session.KindToolCall:
			_, _ = fmt.Fprintf(out, "  -> %s\n", ev.Tool)
		case session.KindConverged:
			return ev.Text, nil
		case session.KindStalled:
			return "", fmt.Errorf("goal stalled: %s", ev.Err)
		}
	}
	return "", context.Canceled
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
