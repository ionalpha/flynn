package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"

	"github.com/ionalpha/flynn/memory/digest"
	"github.com/ionalpha/flynn/state"
)

// memoryUsageReport runs `flynn memory usage`: what this install has put in front
// of its readers unasked, what they did with it, and how alike the pushed sets of a
// fleet's instances have become.
//
// The counters are written as the wake digest pushes and the pull side records a
// use, and the digest reads them back to rank an ignored item down. Nothing else
// reads them, which leaves the two questions a curator actually has unanswerable
// from inside a session: which items are being pushed at readers who never act on
// them, and whether the policy doing the pushing is feeding every instance the same
// short list.
func memoryUsageReport(args []string, dataDir string, out io.Writer) error {
	if len(args) > 0 {
		return errMemoryUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	return renderMemoryUsage(ctx, out, store.Memory())
}

// usageLine is one item's fleet-wide record, ready to print: the per-instance rows
// summed, with whatever the store still holds about the item itself.
type usageLine struct {
	total   state.MemoryUsage
	kind    string
	subject string
	preview string
	// demoted is the verdict the wake digest reaches on these same counters, at the
	// threshold the digest ships with. It is read from there rather than restated,
	// because a report that disagreed with the selection policy about which items
	// are demoted would be worse than no report.
	demoted bool
}

// renderMemoryUsage writes the usage record to out.
func renderMemoryUsage(ctx context.Context, out io.Writer, memories state.MemoryStore) error {
	rows, err := memories.Usage(ctx, nil)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "nothing has been pushed at a reader or used yet")
		return nil
	}
	items, err := memories.Recall(ctx, state.RecallQuery{})
	if err != nil {
		return err
	}

	lines := usageLines(rows, items)
	demoted := 0
	for _, l := range lines {
		if l.demoted {
			demoted++
		}
	}
	_, _ = fmt.Fprintf(out, "%d item(s) with a record, %d demoted:\n", len(lines), demoted)
	for _, l := range lines {
		_, _ = fmt.Fprintf(out, "  [%s] %s: %s, %s%s\n", l.kind, l.subject, pushSummary(l.total), useSummary(l.total), demotedMark(l))
		if l.preview != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", l.preview)
		}
	}
	_, _ = fmt.Fprintln(out, monocultureLine(rows))
	return nil
}

// usageLines sums the per-instance rows per item, joins them to the items the store
// still holds, and orders the result the way a curator reads it: the items being
// pushed hardest for the least return first, because those are the ones a policy
// change would be about.
func usageLines(rows []state.MemoryUsage, items []state.MemoryItem) []usageLine {
	live := make(map[string]state.MemoryItem, len(items))
	for _, it := range items {
		live[it.ID] = it
	}

	byItem := map[string][]state.MemoryUsage{}
	order := []string{}
	for _, r := range rows {
		if _, seen := byItem[r.MemoryID]; !seen {
			order = append(order, r.MemoryID)
		}
		byItem[r.MemoryID] = append(byItem[r.MemoryID], r)
	}

	lines := make([]usageLine, 0, len(order))
	for _, id := range order {
		total := state.TotalUsage(byItem[id])
		l := usageLine{
			total:   total,
			demoted: total.Ignored() && total.PushCount >= int64(digest.DefaultDemoteAfter),
		}
		it, ok := live[id]
		if !ok {
			// Usage outlives a tombstone on purpose, so a curator can still see what
			// was pushed at readers before somebody retired the item. Dropping those
			// rows here would hide exactly the review the record was kept for.
			l.kind, l.subject = "retired", id
			lines = append(lines, l)
			continue
		}
		l.kind, l.subject, l.preview = it.Kind, it.Subject, oneLine(it.Content, 160)
		if l.kind == "" {
			l.kind = "fact"
		}
		if l.subject == "" {
			l.subject = id
		}
		lines = append(lines, l)
	}

	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if a.demoted != b.demoted {
			return a.demoted
		}
		if a.total.Ignored() != b.total.Ignored() {
			return a.total.Ignored()
		}
		if a.total.PushCount != b.total.PushCount {
			return a.total.PushCount > b.total.PushCount
		}
		if a.total.UseCount() != b.total.UseCount() {
			return a.total.UseCount() < b.total.UseCount()
		}
		return a.total.MemoryID < b.total.MemoryID
	})
	return lines
}

// pushSummary says how often the item was put in front of a reader unasked.
func pushSummary(u state.MemoryUsage) string {
	if u.PushCount == 0 {
		return "never pushed"
	}
	return fmt.Sprintf("pushed %d", u.PushCount)
}

// useSummary says what readers did with it, split by origin. The split is the
// measurement: an organic use says the item was worth finding, where a primed use
// mostly says the push landed in front of somebody who could act on it.
func useSummary(u state.MemoryUsage) string {
	if u.UseCount() == 0 {
		return "never used"
	}
	return fmt.Sprintf("used %d (%d organic, %d primed)", u.UseCount(), u.OrganicUses, u.PrimedUses)
}

// demotedMark flags the items the wake digest is already ranking behind everything
// still earning its place. It is a ranking there and a mark here: neither one
// deletes anything, and one use lifts it.
func demotedMark(l usageLine) string {
	if !l.demoted {
		return ""
	}
	return "  [demoted]"
}

// monocultureLine reports how alike the instances' pushed sets are, or says why the
// number is not available yet.
//
// A single-instance install gets the sentence rather than the 0 state.Monoculture
// returns for it. The metric is about a fleet converging, and printing 0.00 on a
// setup that cannot converge would read as a measured all-clear where nothing was
// measured at all.
func monocultureLine(rows []state.MemoryUsage) string {
	pushers := map[string]bool{}
	for _, r := range rows {
		if r.PushCount > 0 && r.InstanceID != "" {
			pushers[r.InstanceID] = true
		}
	}
	if len(pushers) < 2 {
		return "monoculture: not measurable until a second instance has pushed"
	}
	return fmt.Sprintf("monoculture: %.2f across %d instances (0 is no shared item, 1 is one shared list)",
		state.Monoculture(rows), len(pushers))
}
