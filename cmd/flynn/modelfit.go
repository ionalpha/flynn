package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/hardware"
)

// detectBox and probeFreeVRAM are the two reads of the real machine behind `flynn models
// --fit`. They are variables so the fit view can be driven from a described box, which is
// how its ranking is tested on a machine with a different GPU.
var (
	detectBox     = hardware.Detect
	probeFreeVRAM = hardware.FreeVRAM
)

// fitMachine is the machine a fit is judged against and how it was determined.
type fitMachine struct {
	catalog.Machine
	// description is the human line that opens the fit view: what was detected or given.
	description string
}

// known reports whether any memory figure could be established.
func (f fitMachine) known() bool { return f.VRAMFreeBytes > 0 || f.RAMBytes > 0 }

// resolveMachine builds the memory a model may claim. An explicit vramGB or ramGB (in
// GB) replaces the probe for that pool, so a fit can be reproduced on any machine; a
// given vramGB with no ramGB means no RAM to spill into, not this machine's RAM.
// Otherwise a detected GPU contributes the VRAM that is free now, not its total: the
// desktop and other programs already hold some, and a model sized to the total would
// fail to load. System RAM contributes what is left after the OS reserve, which is where
// a mixture-of-experts model's overflow experts go.
func resolveMachine(vramGB, ramGB float64) fitMachine {
	ctx := context.Background()
	box := detectBox(ctx)
	var m fitMachine
	var parts []string

	switch {
	case vramGB > 0:
		m.VRAMFreeBytes = int64(vramGB * 1e9)
		parts = append(parts, fmt.Sprintf("%.0fGB budget (given)", vramGB))
	case box.HasGPU():
		m.VRAMFreeBytes = box.VRAMBytes
		desc := fmt.Sprintf("%s, %s VRAM", box.GPUName, humanBytes(box.VRAMBytes))
		if free, ok := probeFreeVRAM(ctx); ok && free >= 0 && free < box.VRAMBytes {
			m.VRAMFreeBytes = free
			desc = fmt.Sprintf("%s, %s free of %s VRAM, %s in use", box.GPUName, humanBytes(free), humanBytes(box.VRAMBytes), humanBytes(box.VRAMBytes-free))
		}
		parts = append(parts, desc+" (detected)")
	}

	switch {
	case ramGB > 0:
		m.RAMBytes = int64(ramGB * 1e9)
		parts = append(parts, fmt.Sprintf("%.0fGB RAM (given)", ramGB))
	case vramGB > 0:
		// A given VRAM figure describes a machine other than this one, so this machine's RAM
		// would only make the answer depend on where the command was run.
		parts = append(parts, "no system RAM (pass --ram to allow a split)")
	case box.HasRAM():
		m.RAMBytes = cpuInferenceBudget(box.RAMBytes)
		parts = append(parts, fmt.Sprintf("%s system RAM, %s usable (detected)", humanBytes(box.RAMBytes), humanBytes(m.RAMBytes)))
	}
	m.description = strings.Join(parts, "; ")
	return m
}

// renderFit is the "what can this machine run" view: every model ranked, with the memory
// arithmetic that placed it. The columns add up (weights plus cache plus the runtime's
// own allowance is the need), so a reader can see which term sank a model and what
// --ctx or --kv would change.
func renderFit(out io.Writer, cat catalog.Catalog, models []catalog.ModelSpec, mc fitMachine, set catalog.FitSettings, profiles harness.ProfileSource) {
	if !mc.known() {
		_, _ = fmt.Fprintln(out, "could not detect GPU or system memory; pass --vram <GB> to judge fit.")
		renderModels(out, cat, models)
		return
	}
	ctxTokens := set.ContextTokens
	if ctxTokens <= 0 {
		ctxTokens = catalog.DefaultContextTokens
	}
	kv := set.KV
	if !kv.Valid() {
		kv = catalog.KVF16
	}
	_, _ = fmt.Fprintf(out, "fit for %s\ncontext %s, KV cache %s (change with --ctx and --kv)\n\n", mc.description, contextCol(ctxTokens), kv)

	ranked := catalog.Rank(models, mc.Machine, set)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tKIND\tWEIGHTS\tKV CACHE\tNEED\tON GPU\tIN RAM\tPLACEMENT\tFIT\tRELIABILITY\tCAPABILITIES\tLICENSE")
	for _, r := range ranked {
		m, p := r.Model, r.Plan
		weights, kvCol, need, onGPU, inRAM := "-", "-", "-", "-", "-"
		if m.Local() && p.Fit != catalog.FitUnknown {
			weights, kvCol, need = humanBytes(p.WeightBytes), humanBytes(p.KVBytes), humanBytes(p.NeedBytes())
			onGPU, inRAM = humanBytes(p.GPUBytes), humanBytes(p.RAMBytes)
			if p.KVEstimated {
				kvCol += " (est.)"
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID, m.Kind, weights, kvCol, need, onGPU, inRAM, placementCol(p), p.Fit, reliabilityCol(m, profiles), capsCol(m.Capabilities), m.License)
	}
	_ = tw.Flush()

	if top, ok := catalog.Top(ranked, true); ok {
		_, _ = fmt.Fprintf(out, "\nrecommended local model: %s (%s)\n", top.Model.ID, placementCol(top.Plan))
		if top.Plan.Placement == catalog.PlacementSplit {
			if alt, ok := catalog.Top(ranked, false); ok {
				_, _ = fmt.Fprintf(out, "largest that stays wholly on the GPU: %s\n", alt.Model.ID)
			}
		}
	}
	_, _ = fmt.Fprintf(out, "\nNEED = WEIGHTS + KV CACHE + %s for the runtime, judged against free memory with 5%% kept spare. "+
		"gpu+ram keeps the dense weights and the cache on the GPU and moves the rest of a mixture-of-experts model's experts to system RAM; the speed is an estimate against all-GPU.\n", humanBytes(runtimeOverhead))
	_, _ = fmt.Fprintln(out, "reliability: a measured score where the model has been probed, otherwise a quant-floor estimate; run `flynn models probe <id>` to measure.")
}

// runtimeOverhead is the runtime's own allowance as the catalog charges it, repeated here
// only so the legend names the figure the NEED column includes.
const runtimeOverhead = 600_000_000

// placementCol says where a plan runs the model and, for a split, what that costs.
func placementCol(p catalog.FitPlan) string {
	switch p.Placement {
	case catalog.PlacementSplit:
		return fmt.Sprintf("gpu+ram (%.0f%% of experts on CPU, ~%.0f%% speed)", p.ExpertsOnCPU*100, p.RelativeSpeed*100)
	case catalog.PlacementGPU, catalog.PlacementCPU:
		return string(p.Placement)
	default:
		return "-"
	}
}
