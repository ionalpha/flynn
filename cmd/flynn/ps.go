package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

// dispatchPs implements `flynn ps`: the live process view. It lists every known
// instance with its effective run-state, derived from the heartbeat so a crashed
// process reads as Unknown rather than stuck at its last live state. Listing first
// refreshes this process's own Instance record, so the live process always appears.
func dispatchPs(args []string, dataDir string) error {
	if len(args) > 0 {
		return errors.New("usage: flynn ps")
	}
	ctx := context.Background()
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = durable.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	store := durable.Resources(reg)
	registerLocalInstance(ctx, durable.InstanceID(), store)

	rs, err := store.ListAll(ctx, instance.Kind, nil)
	if err != nil {
		return err
	}
	renderTable(os.Stdout, instanceStatusTable(rs, clock.System{}.Now()))
	return nil
}

// dispatchStatus implements `flynn status [<run>]`. With no argument it prints a
// live overview: the instances by effective state and the runs by phase, the one
// screen that answers "what is happening right now". With a run name or id it
// focuses that single run's phase, progress, and objective.
func dispatchStatus(args []string, dataDir string) error {
	ctx := context.Background()
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = durable.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	store := durable.Resources(reg)

	if len(args) >= 1 {
		id, err := resolveID(ctx, store, goal.Kind, args[0])
		if err != nil {
			return err
		}
		r, err := store.GetByID(ctx, id)
		if err != nil {
			return err
		}
		renderTable(os.Stdout, runStatusTable([]resource.Resource{r}))
		return nil
	}

	registerLocalInstance(ctx, durable.InstanceID(), store)
	instances, err := store.ListAll(ctx, instance.Kind, nil)
	if err != nil {
		return err
	}
	runs, err := store.ListAll(ctx, goal.Kind, nil)
	if err != nil {
		return err
	}
	out := os.Stdout
	_, _ = fmt.Fprintln(out, "INSTANCES")
	renderTable(out, instanceStatusTable(instances, clock.System{}.Now()))
	_, _ = fmt.Fprintln(out, "\nRUNS")
	renderTable(out, runStatusTable(runs))
	return nil
}

// instanceStatusTable projects instances to the live process view. STATE is the
// effective state (heartbeat-aware), RUNS is how many runs the instance drives, and
// HEARTBEAT is the age of its last write with a "(stale)" marker once it crosses the
// staleness threshold. now is the single time the whole table is measured against,
// so every row is consistent within one render.
func instanceStatusTable(rs []resource.Resource, now time.Time) controlplane.Table {
	t := controlplane.Table{Columns: []string{"NAME", "HOST", "VERSION", "STATE", "RUNS", "HEARTBEAT"}}
	for _, r := range rs {
		st, _ := instance.DecodeStatus(r)
		spec, _ := instance.DecodeSpec(r)
		state := instance.EffectiveState(r, now, instance.DefaultStaleAfter)
		hb := heartbeatCell(r, now)
		t.Rows = append(t.Rows, controlplane.Row{
			ID:   r.ID,
			Name: r.Name,
			Cells: []string{
				r.Name, spec.Host, spec.Version, string(state),
				strconv.Itoa(len(st.Runs)), hb,
			},
		})
	}
	return t
}

// heartbeatCell renders an instance's heartbeat age, flagging it stale once it
// crosses the threshold the effective-state rule uses, so the marker and the
// reported Unknown state always agree.
func heartbeatCell(r resource.Resource, now time.Time) string {
	if r.UpdatedAt.IsZero() {
		return "never"
	}
	age := compactAge(now.Sub(r.UpdatedAt))
	if instance.IsStale(r, now, instance.DefaultStaleAfter) {
		return age + " (stale)"
	}
	return age
}

// runStatusTable projects runs (goals) to a compact live view: name, phase, steps
// taken, and the objective, trimmed to one line.
func runStatusTable(rs []resource.Resource) controlplane.Table {
	t := controlplane.Table{Columns: []string{"NAME", "PHASE", "STEPS", "OBJECTIVE"}}
	for _, r := range rs {
		t.Rows = append(t.Rows, controlplane.Row{
			ID:   r.ID,
			Name: r.Name,
			Cells: []string{
				r.Name,
				controlplane.StatusField("phase")(r),
				controlplane.StatusField("steps")(r),
				oneLine(controlplane.SpecField("objective")(r), 50),
			},
		})
	}
	return t
}

// compactAge renders a non-negative duration as a short, single-unit age (seconds,
// minutes, hours, or days). A negative duration (a heartbeat clock-skewed into the
// future) clamps to zero, so the cell never shows a confusing negative age.
func compactAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
