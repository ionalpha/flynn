package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// maxPickerSessions caps how many recent sessions the picker lists, so a long
// history stays a short, scannable menu rather than a wall of runs.
const maxPickerSessions = 10

// pickSession offers a front door to the interactive session: when prior runs
// exist it lists the most recent ones and lets the user resume one or start fresh.
// A resumed session is the same durable run the `runs` and `resume` commands act
// on, so this only chooses an id, it does not introduce a parallel concept. It
// returns the chosen run id (empty to start new), that run's already-rendered
// history to seed the transcript, and the sequence to tail new events after. With
// no prior runs it returns empty immediately, so a first run is never interrupted.
func pickSession(ctx context.Context, store *sqlite.Store, reg *resource.Registry, verbose bool) (runID, history string, lastSeq int64, err error) {
	goals, err := store.Resources(reg).ListAll(ctx, goal.Kind, nil)
	if err != nil {
		return "", "", 0, err
	}
	if len(goals) == 0 {
		return "", "", 0, nil // nothing to resume; go straight into a new session
	}
	sort.Slice(goals, func(i, j int) bool { return goals[i].UpdatedHLC.Wall > goals[j].UpdatedHLC.Wall })
	if len(goals) > maxPickerSessions {
		goals = goals[:maxPickerSessions]
	}

	fmt.Fprintln(os.Stderr, "Resume a session, or start a new one:")
	fmt.Fprintln(os.Stderr, "  0) new session")
	for i, g := range goals {
		spec, _ := goal.DecodeSpec(g)
		st, _ := goal.DecodeStatus(g)
		phase := st.Phase
		if phase == "" {
			phase = goal.PhasePending
		}
		fmt.Fprintf(os.Stderr, "  %d) %-9s %s\n", i+1, phase, oneLine(spec.Objective, 60))
	}

	in := bufio.NewReader(os.Stdin)
	choice, err := promptVisible(in, fmt.Sprintf("Select [0-%d, default 0]: ", len(goals)))
	if err != nil {
		return "", "", 0, err
	}
	n := parseChoice(choice, len(goals))
	if n == 0 {
		if t := strings.TrimSpace(choice); t != "" && t != "0" {
			fmt.Fprintln(os.Stderr, "  not a listed choice; starting a new session.")
		}
		return "", "", 0, nil // start a new session
	}

	id := goals[n-1].Name
	hist, last, herr := renderHistory(ctx, store, id, verbose)
	if herr != nil {
		return "", "", 0, herr
	}
	return id, hist, last, nil
}

// parseChoice maps a menu entry to a 1-based selection in [1, n], or 0 for "new
// session": an empty line, "0", or anything that is not one of the listed numbers.
// It never returns an out-of-range index, so a hostile or fat-fingered entry can
// only start a new session, never select a run that is not on the menu.
func parseChoice(input string, n int) int {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0
	}
	v, err := strconv.Atoi(input)
	if err != nil || v < 1 || v > n {
		return 0
	}
	return v
}

// renderHistory replays a run's recorded events through the same renderer a live
// turn uses, returning the rendered transcript and the last event's sequence so the
// resumed session can tail only new events. Reusing renderEvent keeps a resumed
// transcript identical to how the run first appeared and to `flynn inspect`.
func renderHistory(ctx context.Context, store *sqlite.Store, id string, verbose bool) (string, int64, error) {
	events, err := session.History(ctx, store.Log(), id)
	if err != nil {
		return "", 0, err
	}
	var buf bytes.Buffer
	var last int64
	for _, ev := range events {
		renderEvent(&buf, ev, verbose)
		last = ev.Seq
	}
	return buf.String(), last, nil
}
