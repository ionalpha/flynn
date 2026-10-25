package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// cliReviewer is the reviewer a promotion from the command line is recorded under.
// It carries the guard's user scheme because the person at the terminal is the
// operator: the trusted reviewer [guard.PushOnPromotion] waits for, and the only one a
// standalone install has.
const cliReviewer = guard.SchemeUser + "cli"

// memoryReview runs the subcommands that read and decide about individual items:
// `ls`, `promote`, `revoke` and `forget`.
//
// They exist because the push gate has a reviewer slot and standalone Flynn had
// nobody in it. Everything the agent writes for itself waits on a promotion before
// the wake digest will carry it, which is the right default (an LLM reviewer is
// itself attack surface) and, with no way to give one, meant an install's own
// lessons reached a later run only when its objective happened to share their
// words. None of these needs a model, so none resolves one.
func memoryReview(sub string, args []string, dataDir string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// The raw store, as consolidation uses it: a decision about one stored item is
	// not a write the curated path has any say in, and the hybrid ranking in front
	// of recall would only reorder a listing that is sorted here anyway.
	return runMemoryReview(ctx, sub, args, store.Memory(), out)
}

// runMemoryReview is memoryReview without the database file, so the decisions can
// be tested over an in-memory store.
func runMemoryReview(ctx context.Context, sub string, args []string, memories state.MemoryStore, out io.Writer) error {
	switch sub {
	case "ls":
		pending, err := parseLsArgs(args)
		if err != nil {
			return err
		}
		return listMemory(ctx, out, memories, pending)
	case "promote", "revoke":
		id, reason, err := parseDecisionArgs(args)
		if err != nil {
			return err
		}
		return decidePromotion(ctx, out, memories, id, reason, sub == "promote")
	case "forget":
		if len(args) != 1 {
			return errMemoryUsage
		}
		return forgetMemory(ctx, out, memories, args[0])
	default:
		return errMemoryUsage
	}
}

// parseLsArgs reads `ls`'s one flag.
func parseLsArgs(args []string) (pending bool, err error) {
	for _, a := range args {
		if a != "--pending" {
			return false, fmt.Errorf("unknown flag %q", a)
		}
		pending = true
	}
	return pending, nil
}

// parseDecisionArgs reads an item id and an optional --reason for the audit trail.
func parseDecisionArgs(args []string) (id, reason string, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--reason":
			if i+1 >= len(args) {
				return "", "", errors.New("--reason needs a note")
			}
			reason, i = args[i+1], i+1
		case strings.HasPrefix(args[i], "-"):
			return "", "", fmt.Errorf("unknown flag %q", args[i])
		case id != "":
			return "", "", errMemoryUsage
		default:
			id = args[i]
		}
	}
	if id == "" {
		return "", "", errMemoryUsage
	}
	return id, reason, nil
}

// pushStatus is one item's standing with the wake digest, worded for the person
// deciding about it.
type pushStatus struct {
	label string
	// pending is an item a promotion would change: the agent's own untainted note,
	// not yet promoted.
	pending bool
}

// statusOf reads an item's push standing from the same predicate the digest filters
// on, so the listing cannot call an item pushed that the digest leaves out.
func statusOf(it state.MemoryItem, promoted bool) pushStatus {
	switch guard.PushEligibility(it) {
	case guard.PushAllowed:
		return pushStatus{label: "pushed: you wrote it"}
	case guard.PushDenied:
		// The two denials are told apart because they are different answers to "why
		// not": one is where the item came from, the other is what was in the context
		// when it was written, and only the second can be true of a note that cites
		// nothing but the agent.
		if it.Tainted {
			return pushStatus{label: "never pushed: untrusted input was in its context"}
		}
		return pushStatus{label: "never pushed: untrusted source"}
	default:
		if promoted {
			return pushStatus{label: "pushed: promoted"}
		}
		return pushStatus{label: "awaiting review", pending: true}
	}
}

// listMemory writes every live item with its id, its push standing and where it came
// from, which are the three things a promotion decision is made on. With pending it
// writes only the items a promotion would change.
func listMemory(ctx context.Context, out io.Writer, memories state.MemoryStore, pending bool) error {
	items, err := memories.Recall(ctx, state.RecallQuery{})
	if err != nil {
		return err
	}
	promoted, err := promotedSet(ctx, memories, items)
	if err != nil {
		return err
	}

	waiting := 0
	shown := make([]state.MemoryItem, 0, len(items))
	for _, it := range items {
		st := statusOf(it, promoted[it.ID])
		if st.pending {
			waiting++
		}
		if !pending || st.pending {
			shown = append(shown, it)
		}
	}

	switch {
	case len(items) == 0:
		_, _ = fmt.Fprintln(out, "no memory yet")
		return nil
	case pending && waiting == 0:
		_, _ = fmt.Fprintln(out, "nothing is awaiting review")
		return nil
	case pending:
		_, _ = fmt.Fprintf(out, "%d item(s) awaiting review:\n", waiting)
	default:
		_, _ = fmt.Fprintf(out, "%d memory item(s), %d awaiting review:\n", len(items), waiting)
	}
	for _, it := range shown {
		kind := it.Kind
		if kind == "" {
			kind = "fact"
		}
		subject := ""
		if it.Subject != "" {
			subject = " " + it.Subject
		}
		_, _ = fmt.Fprintf(out, "  %s [%s]%s (%s)\n", it.ID, kind, subject, statusOf(it, promoted[it.ID]).label)
		_, _ = fmt.Fprintf(out, "    from: %s\n", sourcesLine(it.Sources))
		_, _ = fmt.Fprintf(out, "    %s\n", oneLine(it.Content, 160))
	}
	if waiting > 0 {
		_, _ = fmt.Fprintln(out, "flynn memory promote <id> puts one in every later run's wake digest.")
	}
	return nil
}

// promotedSet reads the current promotion decisions for items.
func promotedSet(ctx context.Context, memories state.MemoryStore, items []state.MemoryItem) (map[string]bool, error) {
	if len(items) == 0 {
		return map[string]bool{}, nil
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	rows, err := memories.Promotions(ctx, ids)
	if err != nil {
		return nil, err
	}
	return state.PromotedSet(rows), nil
}

// sourcesLine renders an item's provenance. An item with none says so, because the
// guard treats a missing record as the least-trusted answer it can defend and a
// reviewer should see that it is missing rather than a blank.
func sourcesLine(sources []string) string {
	if len(sources) == 0 {
		return "no recorded source"
	}
	return strings.Join(sources, ", ")
}

// resolveMemory finds the live item with id ref. The id is matched whole: ids are
// minted in time order, so items written in one run share their leading characters
// and a prefix would mostly be ambiguous, and the commands this feeds push an item
// at every run or delete it.
func resolveMemory(ctx context.Context, memories state.MemoryStore, ref string) (state.MemoryItem, error) {
	items, err := memories.Recall(ctx, state.RecallQuery{})
	if err != nil {
		return state.MemoryItem{}, err
	}
	for _, it := range items {
		if it.ID == ref {
			return it, nil
		}
	}
	return state.MemoryItem{}, fmt.Errorf("no memory item %q (flynn memory ls lists them)", ref)
}

// decidePromotion records the operator's decision about one item.
//
// Only the agent's own untainted notes take a decision. An item the operator wrote
// is pushed already, so recording a promotion on it would put an approval in the
// audit trail that changed nothing. A denied item is refused with its reason: the
// guard keeps denial terminal because a reviewer reads the finished sentence, which
// is what an attacker gets to write, and a command that appeared to accept the
// promotion would tell the operator the item was now in the digest when it is not.
func decidePromotion(ctx context.Context, out io.Writer, memories state.MemoryStore, ref, reason string, promote bool) error {
	it, err := resolveMemory(ctx, memories, ref)
	if err != nil {
		return err
	}
	promoted, err := promotedSet(ctx, memories, []state.MemoryItem{it})
	if err != nil {
		return err
	}
	st := statusOf(it, promoted[it.ID])

	switch guard.PushEligibility(it) {
	case guard.PushAllowed:
		_, _ = fmt.Fprintf(out, "%s is already pushed: you wrote it, so it needs no review\n", it.ID)
		return nil
	case guard.PushDenied:
		if promote {
			return fmt.Errorf("%s cannot be promoted (%s); it stays recallable, and forget removes it", it.ID, st.label)
		}
		_, _ = fmt.Fprintf(out, "%s is not pushed (%s)\n", it.ID, st.label)
		return nil
	case guard.PushOnPromotion:
		// The one standing a decision changes; handled below.
	}

	if promoted[it.ID] == promote {
		if promote {
			_, _ = fmt.Fprintf(out, "%s is already promoted\n", it.ID)
		} else {
			_, _ = fmt.Fprintf(out, "%s is not promoted\n", it.ID)
		}
		return nil
	}
	if _, err := memories.Promote(ctx, state.PromotionDecision{
		MemoryID: it.ID, Promoted: promote, By: cliReviewer, Reason: reason,
	}); err != nil {
		return err
	}
	if promote {
		_, _ = fmt.Fprintf(out, "promoted %s: later runs wake with it\n  %s\n", it.ID, oneLine(it.Content, 160))
	} else {
		_, _ = fmt.Fprintf(out, "revoked %s: later runs recall it but no longer wake with it\n", it.ID)
	}
	return nil
}

// forgetMemory tombstones one item. It is the only way to take back something the
// operator wrote with /remember: those carry no subject, so no later fact
// supersedes them, and they are pushed at every wake for as long as they live.
// The push and use record and any promotion are kept, so `memory usage` can still
// show what the item was doing before it went.
func forgetMemory(ctx context.Context, out io.Writer, memories state.MemoryStore, ref string) error {
	it, err := resolveMemory(ctx, memories, ref)
	if err != nil {
		return err
	}
	if err := memories.Delete(ctx, it.ID); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "forgot %s: %s\n", it.ID, oneLine(it.Content, 160))
	return nil
}
