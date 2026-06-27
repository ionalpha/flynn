package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inference/modelsource"
	"github.com/ionalpha/flynn/inference/orchestrate"
)

// runModelPool implements `flynn models pool [--vram GB] [--pin id,...] <model-id>...`: keep a
// set of local models resident together within a memory budget, launching what is missing and
// evicting the lowest-priority idle models when the set does not fit. It runs the orchestrator
// loop until interrupted, so a model that crashes is brought back and a switch in what should
// be resident is reconciled without a manual stop and start.
func runModelPool(args []string, dataDir string, out io.Writer) error {
	fs := flag.NewFlagSet("models pool", flag.ContinueOnError)
	fs.SetOutput(out)
	vramGB := fs.Float64("vram", 0, "memory budget in GB (default: auto-detect GPU or system RAM)")
	pinList := fs.String("pin", "", "comma-separated model ids to keep resident regardless of budget")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(out, "usage: flynn models pool [--vram GB] [--pin id,...] <model-id>...")
		_, _ = fmt.Fprintln(out, "Keep a set of local models resident, evicting the lowest-priority idle ones under the memory budget.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	ids := fs.Args()
	if len(ids) == 0 {
		return errors.New("models pool: at least one model id is required (see `flynn models`)")
	}

	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	pp, err := buildPool(cat, ids, commaSet(*pinList))
	if err != nil {
		return fmt.Errorf("models pool: %w", err)
	}
	b := resolveBudget(*vramGB)
	if b.bytes <= 0 {
		return errors.New("models pool: could not detect GPU or system memory; pass --vram <GB>")
	}

	// Show up front which models will not fit, rather than letting them silently never start.
	preview := orchestrate.Schedule(pp.desired, nil, b.bytes)
	for _, id := range preview.Unschedulable {
		_, _ = fmt.Fprintf(out, "warning: %s does not fit the %s budget and will not be served\n", id, humanBytes(b.bytes))
	}
	_, _ = fmt.Fprintf(out, "keeping %d model(s) resident within %s; Ctrl-C to stop\n", len(pp.desired), b.source)

	runner := newLocalRunner(dataDir, out)
	adapter := orchestrate.NewServeAdapter(runner.manager, poolLauncher(runner, pp.specs), pp.footprint)
	provider := staticPoolProvider{ds: orchestrate.DesiredState{Models: pp.desired, Budget: b.bytes}}
	ctrl := orchestrate.NewController(provider, adapter, clock.System{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctrl.Run(ctx)
	_, _ = fmt.Fprintln(out, "stopped")
	return nil
}

// poolPlan is the validated, resolved set of models a pool should keep resident: the desired
// entries the scheduler consumes, plus the per-id footprint and catalog spec the serve adapter
// needs to budget and launch each one.
type poolPlan struct {
	desired []orchestrate.Desired
	sizes   map[string]int64
	specs   map[string]catalog.ModelSpec
}

// footprint reports a model's resident size in bytes for the serve adapter, or zero for an id
// the pool does not manage.
func (p poolPlan) footprint(id string) int64 { return p.sizes[id] }

// buildPool resolves the requested ids against the catalog, rejecting anything that is not a
// known local model, and builds the desired set with each model's smallest-quant size as its
// footprint. A pinned id (matched by its raw or canonical form) is kept resident regardless of
// budget. Duplicate ids collapse to one entry.
func buildPool(cat catalog.Catalog, ids []string, pinned map[string]bool) (poolPlan, error) {
	pp := poolPlan{sizes: map[string]int64{}, specs: map[string]catalog.ModelSpec{}}
	seen := map[string]bool{}
	for _, raw := range ids {
		raw = strings.TrimSpace(raw)
		m, ok := findModel(cat, raw)
		if !ok {
			return poolPlan{}, fmt.Errorf("%q is not in the catalog (see `flynn models`)", raw)
		}
		if !m.Local() {
			return poolPlan{}, fmt.Errorf("%q is a hosted API model, not a local one", m.ID)
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		var size int64
		if q, ok := m.SmallestQuant(); ok {
			size = q.SizeBytes
		}
		pp.desired = append(pp.desired, orchestrate.Desired{
			ModelID:   m.ID,
			Footprint: size,
			Pinned:    pinned[raw] || pinned[m.ID],
		})
		pp.sizes[m.ID] = size
		pp.specs[m.ID] = m
	}
	return pp, nil
}

// poolLauncher returns a launch function that serves a managed model by id, taking it through
// the same source-trust and containment gate as `models run` before it is started. A launch is
// idempotent: serving an already-running model reuses it.
func poolLauncher(runner *localRunner, specs map[string]catalog.ModelSpec) orchestrate.LaunchFunc {
	return func(ctx context.Context, id string) error {
		m, ok := specs[id]
		if !ok {
			return fmt.Errorf("model %q is not in the pool", id)
		}
		src, err := modelsource.Parse(m.ID, isLocalModelID)
		if err != nil {
			return err
		}
		if _, err := runner.admitSource(src); err != nil {
			return err
		}
		_, err = runner.serveModel(ctx, m)
		return err
	}
}

// staticPoolProvider serves a fixed desired state, the explicit set of models named on the
// command line. A later provider can derive the set from the router's recent selections.
type staticPoolProvider struct {
	ds orchestrate.DesiredState
}

// Desired returns the fixed desired state.
func (p staticPoolProvider) Desired(context.Context) (orchestrate.DesiredState, error) {
	return p.ds, nil
}

// commaSet parses a comma-separated flag value into a set, dropping blanks and surrounding
// whitespace. An empty value yields an empty set.
func commaSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}
