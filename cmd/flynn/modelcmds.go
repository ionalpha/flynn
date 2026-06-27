package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/inference/modelsource"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/profilestore"
	"github.com/ionalpha/flynn/reliability"
	"github.com/ionalpha/flynn/resource"
)

// dispatchModels routes a `flynn models <sub>` invocation to its handler. The bare
// `models` command browses the catalog; the named subcommands cover the local model
// lifecycle from provisioning a runtime through running and stopping a server.
func dispatchModels(sub []string, dataDir string) error {
	if len(sub) == 0 {
		return runModels(sub, os.Stdout)
	}
	switch sub[0] {
	case "fetch":
		return runModelFetch(sub[1:], dataDir, os.Stdout)
	case "check":
		return runRuntimeCheck(os.Stdout)
	case "install":
		return runRuntimeInstall(sub[1:], dataDir, os.Stdout)
	case "inspect":
		return runModelInspect(sub[1:], dataDir, os.Stdout)
	case "run":
		return runModelRun(sub[1:], dataDir, os.Stdout)
	case "probe":
		return runModelProbe(sub[1:], dataDir, os.Stdout)
	case "use":
		return runModelUse(sub[1:], dataDir, os.Stdout)
	case "status":
		return runModelStatus(sub[1:], dataDir, os.Stdout)
	case "stop":
		return runModelStop(sub[1:], dataDir, os.Stdout)
	case "pool":
		return runModelPool(sub[1:], dataDir, os.Stdout)
	default:
		return runModels(sub, os.Stdout)
	}
}

// runModelRun implements `flynn models run <id> [prompt...]`: make a local model ready
// and reachable, then either send a one-shot prompt to it or report that it is up. It
// provisions the runtime and weights, starts the server inside the sandbox, and reuses an
// already-running server. The server is left running so a later run, or `flynn goal`,
// reuses it; `flynn models stop <id>` ends it.
func runModelRun(args []string, dataDir string, out io.Writer) error {
	autoApprove, args := takeFlag(args, "--yes", "-y")
	if len(args) == 0 || args[0] == "" {
		return errors.New("models run: a model id or source is required (see `flynn models`)")
	}
	id := args[0]
	prompt := strings.TrimSpace(strings.Join(args[1:], " "))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := newLocalRunner(dataDir, out)

	// Classify the source, refuse an unsafe weight format, record its provenance, and gate
	// it against the isolation this host can provide, before anything is fetched or run.
	src, err := modelsource.Parse(id, isLocalModelID)
	if err != nil {
		return fmt.Errorf("models run: %w", err)
	}
	class, err := runner.classifySource(src)
	if err != nil {
		return fmt.Errorf("models run: %w", err)
	}

	// Surface the trust, isolation, integrity, and network posture in plain language
	// before anything happens, so a refusal below is explained rather than bare, and gate
	// the run on the isolation this host can provide.
	rs := runner.riskSurface(src, class)
	printRiskSurface(out, rs)
	if err := runner.admitOnly(class.Trust); err != nil {
		return fmt.Errorf("models run: %w", err)
	}

	// Require explicit consent for anything that is not a vetted catalog model. The safe
	// answer is the default, and a non-interactive session refuses rather than assumes yes.
	if err := requireConsent(rs, stdinIsTerminal(), autoApprove, os.Stdin, out); err != nil {
		return fmt.Errorf("models run: %w", err)
	}

	if src.Kind != modelsource.KindCatalog {
		// The source is admitted by the isolation gate and consented to, but is not a
		// curated catalog entry. Serving an arbitrary downloaded model is delivered with the
		// strong isolation tier it requires; until then the gate above refuses an
		// uncontained run.
		return fmt.Errorf("models run: %s is %s and would run, but serving a non-catalog model is not wired yet; only catalog models serve today", src.Raw, class.Trust)
	}

	m, err := findLocalModel(id)
	if err != nil {
		return fmt.Errorf("models run: %w", err)
	}
	ep, err := runner.serveModel(ctx, m, 0, false)
	if err != nil {
		return fmt.Errorf("models run: %w", err)
	}
	if ep.Reused {
		_, _ = fmt.Fprintf(out, "reusing the running server for %s at %s\n", id, ep.BaseURL)
	} else {
		_, _ = fmt.Fprintf(out, "serving %s at %s (pid %d), confined to the sandbox\n", id, ep.BaseURL, ep.PID)
	}

	if prompt == "" {
		_, _ = fmt.Fprintf(out, "the model is ready. Send a prompt with `flynn models run %s \"your question\"`, or stop it with `flynn models stop %s`.\n", id, id)
		return nil
	}

	client := localModelClient(ep, m.ID, localModelPlan(ctx, m, dataDir))
	resp, err := client.Generate(ctx, llm.Request{
		Messages:  []llm.Message{llm.Text(llm.RoleUser, prompt)},
		MaxTokens: 1024,
	})
	if err != nil {
		return fmt.Errorf("models run: the model failed to answer: %w", err)
	}
	_, _ = fmt.Fprintln(out, strings.TrimSpace(resp.Message.TextContent()))
	return nil
}

// runModelProbe implements `flynn models probe <id>`: measure whether a local model is dependable
// enough to drive an agent loop, and record the result so later runs scaffold it accordingly. It
// serves the model through the same gated, sandboxed path as `models run`, runs the reliability
// battery against the raw model (no harness scaffolding, so the score reflects the model itself),
// and writes the measured profile to the durable store keyed by the model, quant, and runtime.
func runModelProbe(args []string, dataDir string, out io.Writer) error {
	autoApprove, args := takeFlag(args, "--yes", "-y")
	if len(args) == 0 || args[0] == "" {
		return errors.New("models probe: a model id is required (see `flynn models`)")
	}
	id := args[0]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := newLocalRunner(dataDir, out)
	src, err := modelsource.Parse(id, isLocalModelID)
	if err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	class, err := runner.classifySource(src)
	if err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	rs := runner.riskSurface(src, class)
	printRiskSurface(out, rs)
	if err := runner.admitOnly(class.Trust); err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	if err := requireConsent(rs, stdinIsTerminal(), autoApprove, os.Stdin, out); err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	if src.Kind != modelsource.KindCatalog {
		return fmt.Errorf("models probe: %s is %s; only catalog models can be probed today", src.Raw, class.Trust)
	}

	m, err := findLocalModel(id)
	if err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	ep, err := runner.serveModel(ctx, m, 0, false)
	if err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	_, _ = fmt.Fprintf(out, "probing %s at %s; this runs a fixed battery of tool-call, schema, and instruction probes\n", id, ep.BaseURL)

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}

	// Probe the raw model: no plan, so no grammar or other scaffolding shapes the answer and the
	// score measures the model's own reliability.
	client := localModelClient(ep, m.ID, harness.Plan{})
	return probeAndStore(ctx, client, m, selfProvisionedRuntime, store.Resources(reg), out)
}

// probeAndStore runs the reliability battery against a served model, records the resulting profile
// for the model, quant, and runtime under test, and reports the scores with the quant-floor
// caveat. It is the core of `flynn models probe`, separated from the serving setup so it is
// exercised directly with a scripted model and an in-memory store.
func probeAndStore(ctx context.Context, model llm.Model, m catalog.ModelSpec, runtimeName string, rs resource.Store, out io.Writer) error {
	rep, err := reliability.Score(ctx, model)
	if err != nil {
		return fmt.Errorf("models probe: %w", err)
	}
	prof := rep.Profile()

	quant := ""
	if q, ok := m.SmallestQuant(); ok {
		quant = q.Name
	}
	spec := profilestore.Spec{
		ModelID:              m.ID,
		Quant:                quant,
		Runtime:              runtimeName,
		BatteryVersion:       rep.Version,
		ToolCallReliability:  prof.ToolCallReliability,
		StructuredOutput:     prof.StructuredOutput,
		InstructionFollowing: prof.InstructionFollowing,
		EffectiveContext:     prof.EffectiveContext,
	}
	if err := profilestore.Write(ctx, rs, spec); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "reliability for %s (%s, battery %s):\n", m.ID, quantLabel(quant), rep.Version)
	_, _ = fmt.Fprintf(out, "  tool calls:   %3.0f%%\n", prof.ToolCallReliability*100)
	_, _ = fmt.Fprintf(out, "  schema:       %3.0f%%\n", prof.StructuredOutput*100)
	_, _ = fmt.Fprintf(out, "  instructions: %3.0f%%\n", prof.InstructionFollowing*100)
	if below, reason := reliability.QuantFloor(quant, m.ParamsB); reason != "" {
		mark := "note"
		if below {
			mark = "warning"
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", mark, reason)
	}
	_, _ = fmt.Fprintln(out, "recorded; future runs of this model are scaffolded from this measurement.")
	return nil
}

// quantLabel renders a quantization for display, naming an unrecorded one rather than printing a
// blank.
func quantLabel(quant string) string {
	if quant == "" {
		return "default quant"
	}
	return quant
}

// runModelInspect implements `flynn models inspect <id-or-source>`: classify any model
// reference and show, in plain language, how far it is trusted, the isolation a run
// requires, what is known about its integrity, its network posture, and whether this host
// could run it. It never fetches or runs anything, so a user can understand the risk of a
// model before committing to it.
func runModelInspect(args []string, dataDir string, out io.Writer) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("models inspect: a model id or source is required")
	}
	ref := args[0]
	runner := newLocalRunner(dataDir, out)

	src, err := modelsource.Parse(ref, isLocalModelID)
	if err != nil {
		return fmt.Errorf("models inspect: %w", err)
	}
	class := modelsource.Classify(src, runner.knownPublisher)
	rs := runner.riskSurface(src, class)
	printRiskSurface(out, rs)

	// Name the weight-format verdict where a concrete file is known.
	if name := sourceFileName(src); name != "" {
		if err := modelsource.CheckRunnableFormat(name); err != nil {
			_, _ = fmt.Fprintln(out, "format:    refused ("+err.Error()+")")
		} else {
			_, _ = fmt.Fprintln(out, "format:    a safe-parse weight format")
		}
	}

	// State whether this host could run it, without running it.
	if err := runner.admitOnly(class.Trust); err != nil {
		_, _ = fmt.Fprintln(out, "this host: would REFUSE to run it ("+err.Error()+")")
	} else if rs.Risky() {
		_, _ = fmt.Fprintln(out, "this host: could run it, but only after explicit consent (it is not a vetted catalog model)")
	} else {
		_, _ = fmt.Fprintln(out, "this host: runs it (a vetted catalog model)")
	}
	return nil
}

// takeFlag removes the first occurrence of any of the given flag spellings from args and
// reports whether it was present, so a boolean flag can appear anywhere in the argument
// list without a full flag parser reordering the positional model id and prompt.
func takeFlag(args []string, names ...string) (bool, []string) {
	out := args[:0:0]
	found := false
	for _, a := range args {
		isFlag := false
		for _, n := range names {
			if a == n {
				isFlag = true
				break
			}
		}
		if isFlag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return found, out
}

// runModelUse implements `flynn models use <id>`: provision a local model's runtime and
// weights and record it as the default, without starting it. A later `flynn goal` with no
// explicit model uses the recorded selection and starts it on demand.
func runModelUse(args []string, dataDir string, out io.Writer) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("models use: a model id is required (see `flynn models`)")
	}
	id := args[0]
	m, err := findLocalModel(id)
	if err != nil {
		return fmt.Errorf("models use: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := newLocalRunner(dataDir, out)
	q, _ := m.SmallestQuant()
	if q.URL == "" {
		return fmt.Errorf("models use: %q has no pinned direct download, so it cannot be provisioned for local serving yet", id)
	}
	if _, err := runner.ensureRuntime(ctx, selfProvisionedRuntime); err != nil {
		return fmt.Errorf("models use: %w", err)
	}
	if _, err := runner.ensureWeights(ctx, m, q); err != nil {
		return fmt.Errorf("models use: %w", err)
	}
	if err := writeActiveModel(dataDir, id); err != nil {
		return fmt.Errorf("models use: record selection: %w", err)
	}
	_, _ = fmt.Fprintf(out, "%s is provisioned and set as the default model. `flynn goal` will use it.\n", id)
	return nil
}

// runModelStatus implements `flynn models status`: list the local model servers that are
// currently running and answering, pruning any stale record on the way.
func runModelStatus(_ []string, dataDir string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runner := newLocalRunner(dataDir, out)
	live, err := runner.manager.Status(ctx)
	if err != nil {
		return fmt.Errorf("models status: %w", err)
	}
	if len(live) == 0 {
		_, _ = fmt.Fprintln(out, "no local model servers are running.")
		if active, ok := readActiveModel(dataDir); ok {
			_, _ = fmt.Fprintf(out, "default model: %s (start it with `flynn models run %s`)\n", active, active)
		}
		return nil
	}
	_, _ = fmt.Fprintf(out, "%-32s %-10s %-26s %s\n", "MODEL", "RUNTIME", "ENDPOINT", "PID")
	for _, rec := range live {
		_, _ = fmt.Fprintf(out, "%-32s %-10s %-26s %d\n", rec.ModelID, rec.Runtime, rec.BaseURL, rec.PID)
	}
	return nil
}

// runModelStop implements `flynn models stop <id>`: stop a running local model server and
// drop its record.
func runModelStop(args []string, dataDir string, out io.Writer) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("models stop: a model id is required")
	}
	id := args[0]
	runner := newLocalRunner(dataDir, out)
	stopped, err := runner.manager.Stop(id)
	if err != nil {
		return fmt.Errorf("models stop: %w", err)
	}
	if !stopped {
		_, _ = fmt.Fprintf(out, "no running server found for %s.\n", id)
		return nil
	}
	_, _ = fmt.Fprintf(out, "stopped the server for %s.\n", id)
	return nil
}

// activeModelPath is where the default local model selection is recorded, a single line
// under the data directory.
func activeModelPath(dataDir string) string {
	return filepath.Join(dataDir, "active-model")
}

// writeActiveModel records the default model id.
func writeActiveModel(dataDir, id string) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(activeModelPath(dataDir), []byte(id+"\n"), 0o600)
}

// readActiveModel returns the recorded default model id, and whether one is set.
func readActiveModel(dataDir string) (string, bool) {
	data, err := os.ReadFile(activeModelPath(dataDir))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(data))
	return id, id != ""
}
