package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/inference/provision"
	"github.com/ionalpha/flynn/localmodel"
	"github.com/ionalpha/flynn/provider"
)

const (
	// initRecommendations is how many catalog models the wizard offers. More than a few
	// turns a choice into a table to study, and the full ranking is one command away.
	initRecommendations = 3
	// initAdoptions is how many models already held by a running server are listed.
	initAdoptions = 5
	// initReliabilityBar is the tool-call and schema score under which a model cannot be
	// relied on to drive a goal unaided. It matches the bar the harness uses to start
	// constraining a model's decoding, so a model that clears it here needs no help there.
	initReliabilityBar = 0.9
)

// initHasTerminal reports whether there is a terminal to ask on. It is a variable so the
// refusal without one is tested the same way on a developer's terminal and in CI.
var initHasTerminal = stdinIsTerminal

// errInitStopped ends the wizard with a non-zero status and nothing further to print: the
// wizard has already said why it stopped, and a script that needs to know the default was
// not set reads the exit code.
var errInitStopped error = silent{}

// initRuntimeState is what the wizard needs to know about the runtime Flynn installs.
type initRuntimeState struct {
	Installed bool
	// Variant is the build this machine would use: cpu, cuda-13.3, vulkan.
	Variant string
	// Problem is non-empty when the installed build fails its integrity check.
	Problem string
	// DownloadBytes is the size of the build to install; zero when unknown.
	DownloadBytes int64
}

// initChoice is one thing the wizard can set up: a catalog model to download, or a model a
// running server already holds.
type initChoice struct {
	// ID is the catalog id, or the provider:model spec of an adopted model.
	ID string
	// Model and Quant are set for a catalog model.
	Model catalog.ModelSpec
	Quant catalog.Quant
	// Adopted is set for a model a running server holds.
	Adopted *provider.LocalModel
	// Detail is the menu text after the id.
	Detail string
}

// initEnv is everything `flynn init --local` reads from or does to the machine. The wizard
// is written against it so every step runs without a GPU, a download or a model server:
// the live values are the same functions the `flynn models` subcommands call, and a test
// substitutes scripted ones.
type initEnv struct {
	in      *bufio.Reader
	out     io.Writer
	yes     bool
	dataDir string

	// machine judges fit; it is [resolveMachine] with no override, so the verdicts match
	// `flynn models --local --fit`.
	machine  func(vramGB, ramGB float64) fitMachine
	catalog  func() (catalog.Catalog, error)
	profiles func(ctx context.Context) harness.ProfileSource
	// discover lists the models running Ollama and LM Studio servers hold.
	discover func(ctx context.Context) []provider.LocalModel
	servers  func() ([]string, error)
	runtime  func(ctx context.Context) (initRuntimeState, error)
	install  func(ctx context.Context) error
	fetch    func(ctx context.Context, c initChoice) error
	// probe serves or reaches the model, runs the reliability battery and records the
	// profile that [initEnv.profiles] then reads back.
	probe func(ctx context.Context, c initChoice) error
	// use records the model as the default.
	use func(ctx context.Context, id string) error
}

// runInit implements `flynn init --local`.
func runInit(args []string, dataDir string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		local = fs.Bool("local", false, "set up a model that runs on this machine")
		yes   = fs.Bool("yes", false, "accept the defaults without asking, including the downloads")
		model = fs.String("model", "", "set up this model (a catalog id, or provider:model for a model a running server holds) instead of choosing")
	)
	fs.BoolVar(yes, "y", false, "short for --yes")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(os.Stderr, "usage: flynn init --local [--yes] [--model <id>]\nSet up a model that runs on this machine and make it the default. Flags:")
		fs.PrintDefaults()
	}
	if done, err := parseModelFlags(fs, args); done {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init: unexpected argument %q (usage: flynn init --local [--yes] [--model <id>])", fs.Arg(0))
	}
	if !*local {
		return errors.New("init: say what to set up; `flynn init --local` sets up a model that runs on this machine")
	}
	if !*yes && !initHasTerminal() {
		return errors.New("init: no terminal to ask on; re-run with --yes to accept the defaults, or --model <id> --yes to name the model")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return liveInitEnv(dataDir, os.Stdin, os.Stdout, *yes).run(ctx, *model)
}

// liveInitEnv wires the wizard to the machine through the functions the `flynn models`
// subcommands use, so a step here cannot behave differently from the same step there.
func liveInitEnv(dataDir string, in io.Reader, out io.Writer, yes bool) initEnv {
	return initEnv{
		in:       bufio.NewReader(in),
		out:      out,
		yes:      yes,
		dataDir:  dataDir,
		machine:  resolveMachine,
		catalog:  loadCatalog,
		profiles: func(ctx context.Context) harness.ProfileSource { return localProfileSource(ctx, dataDir) },
		discover: localDiscovery,
		servers:  func() ([]string, error) { return configuredServerNames(dataDir) },
		runtime: func(ctx context.Context) (initRuntimeState, error) {
			host, err := openHost(dataDir, out)
			if err != nil {
				return initRuntimeState{}, err
			}
			defer func() { _ = host.Close() }()
			st, err := host.RuntimeStatus(ctx)
			if err != nil {
				return initRuntimeState{}, err
			}
			res := initRuntimeState{Installed: st.Installed, Variant: st.Variant, Problem: st.Problem}
			rels := provision.ReleasesFor(localmodel.Runtime, runtime.GOOS, runtime.GOARCH)
			if rel, ok := provision.ByVariant(rels, st.Variant); ok {
				res.DownloadBytes = rel.DownloadBytes()
			}
			return res, nil
		},
		install: func(ctx context.Context) error {
			host, err := openHost(dataDir, out)
			if err != nil {
				return err
			}
			defer func() { _ = host.Close() }()
			return installRuntime(ctx, host, localmodel.Runtime, "", out)
		},
		fetch: func(ctx context.Context, c initChoice) error {
			_, err := fetchWeights(ctx, dataDir, c.Model, c.Quant, out)
			return err
		},
		probe: func(ctx context.Context, c initChoice) error {
			if c.Adopted != nil {
				return probeServerModel(ctx, dataDir, *c.Adopted, out)
			}
			runner := newLocalRunner(dataDir, out)
			defer func() { _ = runner.Close() }()
			// The probe is never pre-approved: --yes covers the downloads the wizard shows,
			// not the admission gate's own questions.
			return modelProbe(ctx, runner, consentPrompt, c.ID, out)
		},
		use: func(_ context.Context, id string) error { return runModelUse([]string{id}, dataDir, out) },
	}
}

// probeServerModel runs the reliability battery against a model a running server holds and
// records the profile under its provider:model spec. A server model has no catalog entry,
// so the entry probed is described from what the server reported.
func probeServerModel(ctx context.Context, dataDir string, lm provider.LocalModel, out io.Writer) error {
	// The plan is dropped: the probe scores the raw model, as it does for a catalog one.
	model, _, err := resolveModel(ctx, lm.Spec(), dataDir)
	if err != nil {
		return err
	}
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg, err := governed.Registry()
	if err != nil {
		return err
	}
	return probeAndStore(ctx, model, serverModelSpec(lm), lm.Server, store.Resources(reg), out)
}

// serverModelSpec describes a model a server reported as the catalog entry the probe
// records its profile against. Only what the server said is filled in.
func serverModelSpec(lm provider.LocalModel) catalog.ModelSpec {
	m := catalog.ModelSpec{ID: lm.Spec(), Name: lm.ID, Kind: catalog.KindLocal, Capabilities: catalog.Capabilities{Tools: lm.Tools}}
	if b, err := strconv.ParseFloat(strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(lm.Params)), "B"), 64); err == nil {
		m.ParamsB = b
	}
	if lm.Quant != "" {
		m.Quants = []catalog.Quant{{Name: lm.Quant}}
	}
	return m
}

// run is the wizard. The default model is written by the last step only, after the model
// has answered the probe, so stopping anywhere earlier (a declined prompt, Ctrl-C, a failed
// download) leaves the previous default as it was. What was already downloaded stays on
// disk and is reused by the next run.
func (e initEnv) run(ctx context.Context, model string) error {
	err := e.setup(ctx, model)
	if err != nil && ctx.Err() != nil && !errors.Is(err, errInitStopped) {
		_, _ = fmt.Fprintln(e.out, "\nInterrupted. The default model is unchanged; anything already downloaded stays on disk.")
		return errInitStopped
	}
	return err
}

func (e initEnv) setup(ctx context.Context, model string) error {
	mc := e.machine(0, 0)
	rt, rtErr := e.runtime(ctx)
	found := e.discover(ctx)
	e.describe(mc, rt, rtErr, found)

	cat, err := e.catalog()
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	profiles := e.profiles(ctx)
	recommended := e.recommend(cat, mc, profiles)
	adoptable := adoptions(found)

	var pick initChoice
	if model != "" {
		pick, err = e.named(model, cat, mc, recommended, adoptable)
	} else {
		pick, err = e.choose(ctx, recommended, adoptable)
	}
	if err != nil {
		return err
	}

	if pick.Adopted == nil {
		if err := e.provision(ctx, pick, rt, rtErr); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(e.out, "\nProbing %s: a fixed battery of tool-call, schema and instruction checks.\n", pick.ID)
	if err := e.probe(ctx, pick); err != nil {
		return fmt.Errorf("init: %s did not complete the probe, so it was not set as the default: %w", pick.ID, err)
	}
	if err := e.vouch(ctx, pick); err != nil {
		return err
	}
	if err := e.use(ctx, pick.ID); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "\n%s is the default model. Try it:\n  flynn goal \"list the files in this directory and say what the project is\"\n", pick.ID)
	return nil
}

// describe prints what was detected: the hardware as the fit judges it, the platform, the
// runtime Flynn installs, and the servers already running models.
func (e initEnv) describe(mc fitMachine, rt initRuntimeState, rtErr error, found []provider.LocalModel) {
	_, _ = fmt.Fprintln(e.out, "Detected on this machine:")
	if mc.known() {
		_, _ = fmt.Fprintf(e.out, "  hardware: %s\n", mc.description)
	} else {
		_, _ = fmt.Fprintln(e.out, "  hardware: could not detect GPU or system memory")
	}
	_, _ = fmt.Fprintf(e.out, "  platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	switch {
	case rtErr != nil:
		_, _ = fmt.Fprintf(e.out, "  runtime:  llama.cpp cannot be installed here (%v)\n", rtErr)
	case rt.Problem != "":
		_, _ = fmt.Fprintf(e.out, "  runtime:  llama.cpp %s build is installed but failed its integrity check (%s)\n", rt.Variant, rt.Problem)
	case rt.Installed:
		_, _ = fmt.Fprintf(e.out, "  runtime:  llama.cpp installed (%s build)\n", rt.Variant)
	default:
		_, _ = fmt.Fprintf(e.out, "  runtime:  llama.cpp not installed (the %s build fits this machine)\n", rt.Variant)
	}
	counts := map[string]int{}
	var names []string
	for _, m := range found {
		if counts[m.Server] == 0 {
			names = append(names, m.Server)
		}
		counts[m.Server]++
	}
	if len(names) == 0 {
		_, _ = fmt.Fprintln(e.out, "  servers:  no Ollama or LM Studio server with a model is running")
		return
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s (%d models)", n, counts[n])
	}
	_, _ = fmt.Fprintf(e.out, "  servers:  %s\n", strings.Join(parts, ", "))
}

// recommend ranks the catalog against the machine exactly as `flynn models --local --fit`
// does and keeps the verified models that run on the GPU (or on the CPU when the machine
// has none), can call tools, and have a pinned single-file download Flynn can fetch.
func (e initEnv) recommend(cat catalog.Catalog, mc fitMachine, profiles harness.ProfileSource) []initChoice {
	if !mc.known() {
		return nil
	}
	set, err := fitSettingsFromFlags(catalog.DefaultContextTokens, string(catalog.KVF16))
	if err != nil {
		return nil
	}
	var out []initChoice
	for _, r := range catalog.Rank(cat.Models, mc.Machine, set) {
		q, ok := downloadable(r.Model)
		if !ok || !r.Plan.Fit.Runnable() || r.CPUFallback || !r.Model.Capabilities.Tools {
			continue
		}
		out = append(out, initChoice{
			ID: r.Model.ID, Model: r.Model, Quant: q,
			Detail: fmt.Sprintf("%s download, %s, %s, reliability %s", humanBytes(q.SizeBytes), placementCol(r.Plan), r.Plan.Fit, reliabilityCol(r.Model, profiles)),
		})
		if len(out) == initRecommendations {
			break
		}
	}
	return out
}

// downloadable returns the quantization Flynn would fetch for m when it is a verified local
// model whose smallest quantization is a single pinned GGUF file, the shape the fetch and
// serve path accepts.
func downloadable(m catalog.ModelSpec) (catalog.Quant, bool) {
	if !m.Local() || m.Unverified() {
		return catalog.Quant{}, false
	}
	q, ok := m.SmallestQuant()
	if !ok || q.Format != catalog.FormatGGUF || q.MultiFile() || q.URL == "" || q.SizeBytes <= 0 {
		return catalog.Quant{}, false
	}
	return q, true
}

// adoptions lists the models running servers hold that could drive a goal. An Ollama model
// that does not list tool calling among its capabilities is left out; LM Studio does not
// report capabilities, so its models are kept and the probe decides.
func adoptions(found []provider.LocalModel) []initChoice {
	var out []initChoice
	for _, m := range found {
		if m.Server == "ollama" && !m.Tools {
			continue
		}
		lm := m
		detail := strings.TrimSpace(lm.Params + " " + lm.Quant)
		if detail == "" {
			detail = "size not reported"
		}
		if !lm.Tools {
			detail += ", tool support not reported"
		}
		out = append(out, initChoice{ID: lm.Spec(), Adopted: &lm, Detail: detail + ", already on this machine, no download"})
		if len(out) == initAdoptions {
			break
		}
	}
	return out
}

// choose shows the options and reads the pick. An empty answer takes the first option, the
// default `--yes` takes without asking.
func (e initEnv) choose(ctx context.Context, recommended, adoptable []initChoice) (initChoice, error) {
	all := slices.Concat(recommended, adoptable)
	if len(all) == 0 {
		_, _ = fmt.Fprintln(e.out, "\nNothing to set up automatically: no verified catalog model fits this machine, and no running server holds a model.")
		_, _ = fmt.Fprintln(e.out, "See `flynn models --local --fit` for how each model fits, or start Ollama or LM Studio with a model and run this again.")
		return initChoice{}, errInitStopped
	}
	if len(recommended) > 0 {
		_, _ = fmt.Fprintln(e.out, "\nModels that fit this machine (from `flynn models --local --fit`):")
	}
	for i, c := range recommended {
		_, _ = fmt.Fprintf(e.out, "  %d) %s  %s\n", i+1, c.ID, c.Detail)
	}
	if len(adoptable) > 0 {
		_, _ = fmt.Fprintln(e.out, "\nAlready running on this machine (use one instead of downloading):")
	}
	for i, c := range adoptable {
		_, _ = fmt.Fprintf(e.out, "  %d) %s  %s\n", len(recommended)+i+1, c.ID, c.Detail)
	}

	if e.yes {
		_, _ = fmt.Fprintf(e.out, "\nTaking the first option: %s\n", all[0].ID)
		return all[0], nil
	}
	for {
		ans, err := e.ask(ctx, fmt.Sprintf("\nChoose 1-%d [1], or q to stop: ", len(all)))
		if err != nil {
			return initChoice{}, err
		}
		switch strings.ToLower(ans) {
		case "":
			return all[0], nil
		case "q", "quit", "n", "no":
			return initChoice{}, e.stopped("No model chosen.")
		}
		if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(all) {
			return all[n-1], nil
		}
		if i := slices.IndexFunc(all, func(c initChoice) bool { return c.ID == ans }); i >= 0 {
			return all[i], nil
		}
		_, _ = fmt.Fprintf(e.out, "%q is not one of the options.\n", ans)
	}
}

// named resolves --model. A model on the menu is taken as listed. Otherwise it must be a
// verified catalog model that fits this machine, or a keyless provider:model spec (a model
// on a local or named server), which the probe then checks is reachable.
func (e initEnv) named(model string, cat catalog.Catalog, mc fitMachine, recommended, adoptable []initChoice) (initChoice, error) {
	for _, c := range slices.Concat(recommended, adoptable) {
		if c.ID == model {
			return c, nil
		}
	}
	if m, ok := findModel(cat, model); ok {
		q, ok := downloadable(m)
		if !ok {
			return initChoice{}, fmt.Errorf("init: %s is not a verified catalog model with a pinned download; use `flynn models use %s` to set it up yourself", model, model)
		}
		set, err := fitSettingsFromFlags(catalog.DefaultContextTokens, string(catalog.KVF16))
		if err != nil {
			return initChoice{}, err
		}
		plan := m.PlanFit(mc.Machine, set)
		if !plan.Fit.Runnable() {
			return initChoice{}, fmt.Errorf("init: %s does not fit this machine (%s); `flynn models --local --fit` shows what does", model, plan.Fit)
		}
		return initChoice{ID: m.ID, Model: m, Quant: q, Detail: humanBytes(q.SizeBytes) + " download"}, nil
	}

	name, rest, ok := strings.Cut(model, ":")
	if !ok || name == "" || rest == "" {
		return initChoice{}, fmt.Errorf("init: %q is not in the catalog and is not a provider:model spec (see `flynn models`)", model)
	}
	if _, keyed := provider.KeyRef(name); keyed {
		return initChoice{}, fmt.Errorf("init: %s is a hosted provider; `flynn init --local` sets up models on this machine (use `flynn models use %s` for a hosted one)", name, model)
	}
	servers, err := e.servers()
	if err != nil {
		return initChoice{}, fmt.Errorf("init: %w", err)
	}
	if !knownProviderSpec(model, servers) {
		return initChoice{}, fmt.Errorf("init: %q names no catalog model and no server this install knows (see `flynn models`)", model)
	}
	lm := provider.LocalModel{Server: name, ID: rest}
	return initChoice{ID: model, Adopted: &lm, Detail: "model on the " + name + " server"}, nil
}

// provision installs the runtime when it is missing and fetches the weights, each after
// showing the download size and asking. With --yes the sizes are shown and nothing is asked.
func (e initEnv) provision(ctx context.Context, c initChoice, rt initRuntimeState, rtErr error) error {
	switch {
	case rtErr != nil:
		return fmt.Errorf("init: %s needs the llama.cpp runtime, which cannot be installed here: %w", c.ID, rtErr)
	case rt.Problem != "":
		return fmt.Errorf("init: the installed llama.cpp runtime fails its integrity check (%s); `flynn models install` replaces it", rt.Problem)
	case !rt.Installed:
		if ok, err := e.approveDownload(ctx, fmt.Sprintf("the llama.cpp runtime (%s build)", rt.Variant), rt.DownloadBytes); err != nil || !ok {
			return e.declined(err)
		}
		if err := e.install(ctx); err != nil {
			return err
		}
	}
	if ok, err := e.approveDownload(ctx, fmt.Sprintf("%s (%s)", c.ID, c.Quant.Name), c.Quant.SizeBytes); err != nil || !ok {
		return e.declined(err)
	}
	if err := e.fetch(ctx, c); err != nil {
		return fmt.Errorf("init: fetch %s: %w", c.ID, err)
	}
	return nil
}

// approveDownload shows what is about to be fetched and how large it is, and asks. The
// default answer is yes, since the size is on screen and the choice was already made.
func (e initEnv) approveDownload(ctx context.Context, what string, size int64) (bool, error) {
	shown := "size unknown"
	if size > 0 {
		shown = humanBytes(size)
	}
	if e.yes {
		_, _ = fmt.Fprintf(e.out, "\nDownloading %s: %s.\n", what, shown)
		return true, nil
	}
	ans, err := e.ask(ctx, fmt.Sprintf("\nDownload %s: %s. Continue? [Y/n]: ", what, shown))
	if err != nil {
		return false, err
	}
	return yesAnswer(ans, true), nil
}

// vouch holds back the default until the probe's scores are read. A model below the bar
// is set only on an explicit yes; --yes does not give one.
func (e initEnv) vouch(ctx context.Context, c initChoice) error {
	prof, ok := e.profiles(ctx).Profile(c.ID)
	if ok && prof.ToolCallReliability >= initReliabilityBar && prof.StructuredOutput >= initReliabilityBar {
		return nil
	}
	if ok {
		_, _ = fmt.Fprintf(e.out, "\n%s scored %.0f%% on tool calls and %.0f%% on schemas; goals need %.0f%% on both to run unaided.\n",
			c.ID, prof.ToolCallReliability*100, prof.StructuredOutput*100, initReliabilityBar*100)
	} else {
		_, _ = fmt.Fprintf(e.out, "\nThe probe recorded no scores for %s, so there is nothing to say it can drive a goal.\n", c.ID)
	}
	if e.yes {
		_, _ = fmt.Fprintf(e.out, "Not set as the default. Use it anyway with `flynn models use %s`.\n", c.ID)
		return errInitStopped
	}
	ans, err := e.ask(ctx, "Set it as the default anyway? [y/N]: ")
	if err != nil {
		return err
	}
	if !yesAnswer(ans, false) {
		return e.stopped("Not set as the default.")
	}
	return nil
}

// ask reads one answer through [promptVisible], the prompt every setup flow in this command
// shares. The read runs apart from the wait so Ctrl-C ends a prompt that is blocked on the
// terminal; the read left behind is abandoned with the process.
func (e initEnv) ask(ctx context.Context, label string) (string, error) {
	type reply struct {
		text string
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		text, err := promptVisible(e.in, e.out, label)
		ch <- reply{text, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.text, r.err
	}
}

// declined ends the wizard after a "no" to a download, or passes on the error that stopped
// the question from being asked.
func (e initEnv) declined(err error) error {
	if err != nil {
		return err
	}
	return e.stopped("Download declined.")
}

// stopped says why the wizard ended on the user's word and that the default is as it was.
func (e initEnv) stopped(why string) error {
	_, _ = fmt.Fprintf(e.out, "%s The default model is unchanged; anything already downloaded stays on disk.\n", why)
	return errInitStopped
}

// yesAnswer reads a y/n answer, taking def for an empty one.
func yesAnswer(ans string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}
