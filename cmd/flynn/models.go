package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/hardware"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/reliability"
)

// runModels implements `flynn models`: browse the curated model catalog and filter
// it, so a user can see which models exist, what they cost in size and context, what
// they can do, and where they came from, before choosing or fetching one. It reads
// the embedded catalog only; it makes no network call.
func runModels(args []string, dataDir string, out io.Writer) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		local     = fs.Bool("local", false, "only local (downloadable open-weight) models")
		api       = fs.Bool("api", false, "only hosted API models")
		capName   = fs.String("cap", "", "require a capability: tools, vision, or reasoning")
		maxSizeGB = fs.Float64("max-size", 0, "only models whose smallest quant fits in N GB (local models)")
		maxParamB = fs.Float64("max-params", 0, "only models at or below N billion parameters")
		publisher = fs.String("publisher", "", "only models from this publisher")
		safeOnly  = fs.Bool("safe", false, "drop models whose only weights use a code-executing format")
		fit       = fs.Bool("fit", false, "show how each model fits this machine's GPU memory")
		vramGB    = fs.Float64("vram", 0, "GPU memory budget in GB to judge fit against (default: auto-detect)")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(out, "usage: flynn models [filters]\nShow the model catalog. Filters:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h is not a failure; flag already printed the usage
		}
		return err
	}

	cat, err := catalog.Load()
	if err != nil {
		return err
	}

	q := catalog.Query{Capability: *capName, MaxParamsB: *maxParamB, Publisher: *publisher, SafeOnly: *safeOnly}
	switch {
	case *local && !*api:
		q.Kind = catalog.KindLocal
	case *api && !*local:
		q.Kind = catalog.KindAPI
	}
	models := cat.Find(q)
	if *maxSizeGB > 0 {
		models = withinSize(models, int64(*maxSizeGB*1e9))
	}

	if len(models) == 0 {
		_, _ = fmt.Fprintln(out, "no models match")
		return nil
	}
	if *fit || *vramGB > 0 {
		// The fit view is two-axis: whether the model fits this machine, and whether it clears
		// the agentic-reliability bar. The reliability axis reads a measured profile when one
		// has been recorded, so the lookup is best-effort and never blocks the listing.
		profiles := localProfileSource(context.Background(), dataDir)
		renderFit(out, cat, models, resolveBudget(*vramGB), profiles)
		return nil
	}
	renderModels(out, cat, models)
	return nil
}

// budget is the memory ceiling fit is judged against, and how it was determined.
type budget struct {
	bytes  int64
	source string // human description: "given", a detected GPU, or empty when unknown
}

// resolveBudget takes an explicit GB budget when given, otherwise probes the machine. A
// detected GPU sets the budget to its VRAM (a GPU run is bound by it). With no usable
// GPU it falls back to system RAM, the bound on a CPU-only run, reserving headroom for
// the OS and the runtime so the verdict reflects what is actually free to a model. The
// budget is left unknown (zero) only when nothing could be detected, so fit degrades to
// "?" rather than a wrong verdict.
func resolveBudget(vramGB float64) budget {
	if vramGB > 0 {
		return budget{bytes: int64(vramGB * 1e9), source: fmt.Sprintf("%.0fGB budget (given)", vramGB)}
	}
	box := hardware.Detect(context.Background())
	if box.HasGPU() {
		return budget{bytes: box.VRAMBytes, source: fmt.Sprintf("%s, %s VRAM (detected)", box.GPUName, humanBytes(box.VRAMBytes))}
	}
	if box.HasRAM() {
		avail := cpuInferenceBudget(box.RAMBytes)
		return budget{bytes: avail, source: fmt.Sprintf("%s system RAM (detected, CPU inference, %s usable)", humanBytes(box.RAMBytes), humanBytes(avail))}
	}
	return budget{}
}

// cpuInferenceBudget is the share of system RAM a CPU-only model run can claim. The OS,
// other processes, and the runtime's own working set need room, so a fixed reserve is
// held back (the larger of 2GB or a quarter of RAM) rather than offering the whole
// total to the weights. The result never goes below zero.
func cpuInferenceBudget(ramBytes int64) int64 {
	reserve := ramBytes / 4
	if floor := int64(2_000_000_000); reserve < floor {
		reserve = floor
	}
	if reserve >= ramBytes {
		return 0
	}
	return ramBytes - reserve
}

// renderFit lists the models with a fit verdict against the budget and a single
// recommendation, the "what can this machine actually run" view. When no budget is
// known it says so and falls back to the plain listing.
func renderFit(out io.Writer, cat catalog.Catalog, models []catalog.ModelSpec, b budget, profiles harness.ProfileSource) {
	if b.bytes <= 0 {
		_, _ = fmt.Fprintln(out, "could not detect GPU or system memory; pass --vram <GB> to judge fit.")
		renderModels(out, cat, models)
		return
	}
	_, _ = fmt.Fprintf(out, "fit for %s:\n\n", b.source)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tKIND\tSIZE\tFIT\tRELIABILITY\tCAPABILITIES\tLICENSE")
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID, m.Kind, sizeCol(m), m.FitFor(b.bytes), reliabilityCol(m, profiles), capsCol(m.Capabilities), m.License)
	}
	_ = tw.Flush()
	if rec, ok := recommend(models, b.bytes); ok {
		_, _ = fmt.Fprintf(out, "\nrecommended local model: %s\n", rec.ID)
	}
	_, _ = fmt.Fprintln(out, "\nreliability: a measured score where the model has been probed, otherwise a quant-floor estimate; run `flynn models probe <id>` to measure.")
}

// reliabilityCol renders a local model's standing on the agentic-reliability axis: the measured
// tool-call reliability when the model has been probed, otherwise the a-priori reading from its
// smallest quant against the quant floor. A hosted API model is not probed locally, so it shows a
// dash rather than a verdict it has not earned.
func reliabilityCol(m catalog.ModelSpec, profiles harness.ProfileSource) string {
	if !m.Local() {
		return "-"
	}
	if p, ok := profiles.Profile(m.ID); ok {
		return fmt.Sprintf("%.0f%% measured", p.ToolCallReliability*100)
	}
	q, ok := m.SmallestQuant()
	if !ok {
		return "unprobed"
	}
	below, reason := reliability.QuantFloor(q.Name, m.ParamsB)
	switch {
	case below:
		return "below floor"
	case reason != "":
		return "borderline"
	default:
		return "unprobed"
	}
}

// recommend picks a single default local model for the budget: the largest one that
// fits comfortably (more parameters generally means more capable), so the suggestion
// is the strongest model the machine can run without crowding memory.
func recommend(models []catalog.ModelSpec, availBytes int64) (catalog.ModelSpec, bool) {
	var best catalog.ModelSpec
	found := false
	for _, m := range models {
		if !m.Local() || m.FitFor(availBytes) != catalog.FitFeasible {
			continue
		}
		if !found || m.ParamsB > best.ParamsB {
			best, found = m, true
		}
	}
	return best, found
}

// withinSize keeps API models (no download) and local models whose smallest quant is
// at most maxBytes, the "does it fit my disk" filter.
func withinSize(models []catalog.ModelSpec, maxBytes int64) []catalog.ModelSpec {
	var out []catalog.ModelSpec
	for _, m := range models {
		if !m.Local() {
			out = append(out, m)
			continue
		}
		if q, ok := m.SmallestQuant(); ok && q.SizeBytes <= maxBytes {
			out = append(out, m)
		}
	}
	return out
}

// renderModels writes the matching entries as an aligned table, with a header noting
// the catalog version so a stale list is visible.
func renderModels(out io.Writer, cat catalog.Catalog, models []catalog.ModelSpec) {
	_, _ = fmt.Fprintf(out, "catalog v%s (updated %s), %d of %d models\n\n", cat.Version, cat.Updated, len(models), len(cat.Models))
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tKIND\tSIZE\tCONTEXT\tCAPABILITIES\tTRUST\tLICENSE")
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID, m.Kind, sizeCol(m), contextCol(m.ContextTokens), capsCol(m.Capabilities), m.Trust, m.License)
	}
	_ = tw.Flush()
}

// sizeCol is the on-disk footprint of a local model's smallest quant, or "-" for a
// hosted API model that downloads nothing.
func sizeCol(m catalog.ModelSpec) string {
	q, ok := m.SmallestQuant()
	if !ok {
		return "-"
	}
	return humanBytes(q.SizeBytes)
}

// contextCol renders a context window compactly (32768 -> "32k"), or "?" when unknown.
func contextCol(tokens int) string {
	switch {
	case tokens <= 0:
		return "?"
	case tokens >= 1000:
		return strconv.Itoa(tokens/1000) + "k"
	default:
		return strconv.Itoa(tokens)
	}
}

// capsCol joins a model's capabilities into a short list, or "-" if it declares none.
func capsCol(c catalog.Capabilities) string {
	var s []string
	if c.Tools {
		s = append(s, "tools")
	}
	if c.Vision {
		s = append(s, "vision")
	}
	if c.Reasoning {
		s = append(s, "reasoning")
	}
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ",")
}

// humanBytes renders a byte count in GB to one decimal (the scale model weights live
// at), or MB below a gigabyte.
func humanBytes(n int64) string {
	const gb = 1_000_000_000
	if n >= gb {
		return fmt.Sprintf("%.1fG", float64(n)/gb)
	}
	return fmt.Sprintf("%dM", n/1_000_000)
}
