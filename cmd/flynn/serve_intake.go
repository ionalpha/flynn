package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

// A served process is reachable by anyone who can message its bot, and a message becomes a
// goal's objective, so nothing that a sender wrote is acted on until the sender is named by
// the operator. This file is that rule and the intake it guards.

const (
	// refusedSenderStream is the spine stream on which a message from a sender outside the
	// allow-list is recorded, and refusedSenderEvent its event type.
	refusedSenderStream = "serve.intake"
	refusedSenderEvent  = "serve.sender.refused"

	// servedBudgetTokens is the token ceiling each served goal runs under when the operator
	// sets neither --max-tokens nor --max-cost. A run started by a chat message has nobody
	// watching it, so the unlimited default a terminal run has is not carried over; an
	// operator who wants more raises the ceiling and one who wants none sets it very high.
	servedBudgetTokens int64 = 1_000_000

	// servedPoolPrefix names the budget pool a served goal charges, after the inbox entry
	// that started it.
	servedPoolPrefix = "served-"
)

var signalNumberPattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// senderAllowList maps an inbox source name to the sender ids the operator allowed on it. A
// source with no entry, or an empty one, allows nobody.
type senderAllowList map[string]map[string]struct{}

// add allows ids on source.
func (l senderAllowList) add(source string, ids []string) {
	set := l[source]
	if set == nil {
		set = make(map[string]struct{}, len(ids))
		l[source] = set
	}
	for _, id := range ids {
		set[id] = struct{}{}
	}
}

// allows reports whether sender is named for source. The comparison is exact on the
// platform's stable id: a display name or username is chosen by the sender and can be taken
// over by someone else, so no source reports one as the sender.
func (l senderAllowList) allows(source, sender string) bool {
	if sender == "" {
		return false
	}
	_, ok := l[source][sender]
	return ok
}

// parseTelegramAllow checks and canonicalizes Telegram user ids. They are numeric: a
// username is the sender's to change, so one is refused with the reason rather than
// accepted and never matched.
func parseTelegramAllow(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("serve: --telegram-allow %q: want a numeric Telegram user id (a username can be changed or reassigned, so it is not accepted)", v)
		}
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out, nil
}

// parseSignalAllow checks Signal senders, which are phone numbers in international form.
func parseSignalAllow(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		n := strings.TrimSpace(v)
		if !signalNumberPattern.MatchString(n) {
			return nil, fmt.Errorf("serve: --signal-allow %q: want a phone number in international form, e.g. +15551234567", v)
		}
		out = append(out, n)
	}
	return out, nil
}

// senderGate is an inbox source that passes on only the messages whose sender the operator
// allowed. Anything else is dropped here, before the inbox records it, so a stranger's text
// is never stored as an entry, never triaged and never becomes a goal, and the refusal is
// written to the spine with the sender and not the message.
type senderGate struct {
	inbox.Source
	allow senderAllowList
	log   spine.Log
	warn  func(error)
}

// Receive filters the wrapped source's stream.
func (g senderGate) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	in, err := g.Source.Receive(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan inbox.Spec)
	go func() {
		defer close(out)
		for spec := range in {
			if !g.allow.allows(g.Name(), spec.Sender) {
				g.refuse(ctx, spec)
				continue
			}
			select {
			case out <- spec:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// refuse records that a message from spec.Sender was dropped. The body is left out on
// purpose: it is text an unauthorized party chose, and the record is for the operator to
// see who tried. A failed write is reported and does not let the message through.
func (g senderGate) refuse(ctx context.Context, spec inbox.Spec) {
	_, err := g.log.Append(ctx, spine.AppendInput{
		Stream:  refusedSenderStream,
		Type:    refusedSenderEvent,
		Actor:   spine.ActorSystem,
		Payload: map[string]any{"source": g.Name(), "sender": spec.Sender},
	})
	if err != nil && g.warn != nil {
		g.warn(fmt.Errorf("serve: record refused sender: %w", err))
	}
}

// serveGovernance is what the operator's run flags say about the goals a served process
// drives, the same flags a terminal run takes. Without it `flynn --require-approval shell
// serve` would start a process that quietly ignored the requirement.
type serveGovernance struct {
	approve   []string
	outside   []string
	allowed   []string
	budget    budgetpkg.Limits
	resLimits sandbox.ResourceLimits
}

// goalBudget is the ceiling each served goal runs under: the operator's, or the default
// token ceiling when they set none.
func (g serveGovernance) goalBudget() budgetpkg.Limits {
	if g.budget.Tokens <= 0 && g.budget.Cost <= 0 {
		return budgetpkg.Limits{Tokens: servedBudgetTokens}
	}
	return g.budget
}

// serveIntake is a served process with its model resolved: the governed run its goals are
// driven by, and the channels and allow-list that feed it.
type serveIntake struct {
	model   llm.Model
	plan    harness.Plan
	workdir string
	rstore  resource.Store
	jq      jobs.Queue
	log     spine.Log
	sources []inbox.Source
	sinks   []inbox.Sink
	allow   senderAllowList
	gov     serveGovernance
	notice  io.Writer
}

// run answers messages until ctx is cancelled. Every goal goes through the same gates a
// terminal run's do, with no prompter because nobody at a terminal can be asked: an action
// the operator listed for approval is refused, as it is for `flynn goal`.
func (s serveIntake) run(ctx context.Context) error {
	run, err := assembly{
		model: s.model, plan: s.plan, workdir: s.workdir, rstore: s.rstore, jq: s.jq, log: s.log,
		resLimits: s.gov.resLimits,
		gates:     gateSetup{approve: s.gov.approve, outside: s.gov.outside},
	}.assemble(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = run.Close() }()
	rt := run.Runtime()

	worker := &goalWorker{
		rt: rt, store: s.rstore,
		budgets: budgetpkg.NewLedger(s.rstore), limits: s.gov.goalBudget(),
		allowances: declaredAllowances(s.gov.allowed),
	}
	triage := inbox.NewTriage(s.rstore, worker, inbox.NewSinks(s.sinks...), clock.System{})
	mgr := reconcile.NewManager(s.rstore)
	mgr.Register(inbox.Kind, triage)

	gated := make([]inbox.Source, 0, len(s.sources))
	for _, src := range s.sources {
		gated = append(gated, senderGate{
			Source: src, allow: s.allow, log: s.log,
			warn: func(e error) { fmt.Fprintln(os.Stderr, "serve:", e) },
		})
	}
	ingest := inbox.NewIngest(s.rstore, mgr, clock.System{}, gated,
		inbox.WithIngestErrorHandler(func(e error) { fmt.Fprintln(os.Stderr, "serve:", e) }))

	go func() { _ = rt.Start(ctx) }()
	go func() { mgr.Start(ctx) }()

	s.announce()
	if err := ingest.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// announce states, before the first message is read, who may start a run and what a run is
// held to, so a process that is more open than the operator meant to leave it says so. A
// notice that cannot be written does not stop the process: the posture it describes is
// enforced whether or not anyone is told.
func (s serveIntake) announce() {
	if s.notice == nil {
		return
	}
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(s.notice, "flynn serve: "+format+"\n", args...) }
	for _, src := range s.sources {
		say("%s: %d allowed sender(s); every other sender is refused and recorded", src.Name(), len(s.allow[src.Name()]))
	}
	if gated := governed.GatedActions(governed.ApprovalPolicy(s.gov.approve)); len(gated) > 0 {
		say("approval required for %s; nothing here can prompt, so these actions are refused", strings.Join(gated, ", "))
	} else {
		say("no action requires approval (the run's grant and sandbox are its controls); set --require-approval to gate more")
	}
	lim := s.gov.goalBudget()
	say("each goal is capped at %d tokens, cost %g (0 is no cap on that axis)", lim.Tokens, lim.Cost)
	say("answering messages; press Ctrl-C to stop")
}
