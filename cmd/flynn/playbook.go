package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/playbook"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/sandbox"
)

// flyAppMaxLen is the length budget for a derived Fly app name. A Fly app name is a DNS
// label (it becomes "<app>.fly.dev"); this stays well inside the label limit and keeps the
// name readable while leaving room for the identity-derived suffix.
const flyAppMaxLen = 30

// fillDerivedNames supplies a globally-unique resource name for a playbook input the
// operator left blank, deriving it from this instance's identity so the name is the same on
// every redeploy (and so two instances never collide). Today this covers the one input that
// needs a globally-unique, provider-valid name: a Fly app. The derivation policy lives in
// one place (controlplane.ResolveName); this only supplies the provider's rules (a DNS
// label) and the input key, which are the parts that genuinely differ per provider. An
// operator-supplied "app" is left untouched: an explicit name always wins.
func fillDerivedNames(ctx context.Context, dataDir string, pb playbook.Playbook, config map[string]any) error {
	if pb.Spec.Service == nil || pb.Spec.Service.Provider != "fly" {
		return nil
	}
	if v, ok := config["app"]; ok {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			return nil
		}
	}
	// Load the instance's stable identity to derive from; if the vault is locked or has no
	// identity yet, ResolveName falls back to a one-off name and reports that, rather than
	// failing the deploy.
	var id *controlplane.Identity
	if loaded, err := controlplane.LoadOrCreateIdentity(ctx, vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), ""); err == nil {
		id = loaded
	}
	name, err := controlplane.ResolveName(id, "flynn-agent", "fly-app", "", controlplane.DNSName(flyAppMaxLen))
	if err != nil {
		return fmt.Errorf("playbook: deriving a Fly app name: %w", err)
	}
	config["app"] = name.Value
	switch name.Source {
	case controlplane.NameIdentity:
		_, _ = fmt.Fprintf(os.Stderr, "Using derived app name %q (from this instance's identity; stable across redeploys).\n", name.Value)
	case controlplane.NameEphemeral:
		_, _ = fmt.Fprintf(os.Stderr, "No instance identity available, so using a one-off app name %q (it will differ next run). Unlock the vault for a stable name.\n", name.Value)
	case controlplane.NameOverride:
		// Unreachable here (no override is passed), but listed so the cases are exhaustive.
	}
	return nil
}

// terminalConfirmer asks the operator to approve a playbook's confirm steps at the
// terminal: it prints the step's message and waits for a line on stdin. An empty line (just
// Enter) approves; "n"/"no" declines; no input available (a non-interactive run) fails
// closed with an instruction, so a confirmation never proceeds silently or hangs.
type terminalConfirmer struct{}

func (terminalConfirmer) Confirm(_ context.Context, message string) error {
	_, _ = fmt.Fprintln(os.Stderr, message)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fault.New(fault.Cancelled, "playbook_confirm_noinput",
			"playbook: this step needs your confirmation but no input is available; run it in an interactive terminal, or set the relevant token to skip the step")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "n", "no":
		return fault.New(fault.Cancelled, "playbook_confirm_declined", "playbook: cancelled at the confirmation step")
	default:
		return nil
	}
}

// terminalProgress shows a playbook's steps as they run, so a long procedure (provisioning
// a tool, deploying an app) is visible at the terminal rather than silent until it finishes.
// It writes to stderr, leaving stdout for the run's final result. Each command is printed as
// it starts, then its output and exit status as it ends; a dependency step shows what is
// being made available. Failures are not printed here: the run's error carries the command
// output, so it is reported once, by the caller.
type terminalProgress struct{ w io.Writer }

func (p terminalProgress) Step(ev flow.StepEvent) {
	switch ev.Phase {
	case flow.StepBegin:
		switch ev.Op {
		case flow.OpExec:
			_, _ = fmt.Fprintf(p.w, "\n$ %s\n", ev.Detail)
		case flow.OpDependency:
			_, _ = fmt.Fprintf(p.w, "\n* ensuring %s is available\n", ev.Detail)
		case flow.OpSecret:
			// ev.Detail is "sink:ref", a sink name and a reference name, never the value.
			_, _ = fmt.Fprintf(p.w, "\n* materializing secret %s\n", ev.Detail)
		default:
			// Other ops carry no command to show; only exec, dependency, and secret are reported.
		}
	case flow.StepEnd:
		if ev.Err != nil {
			return
		}
		switch ev.Op {
		case flow.OpExec:
			if out := strings.TrimSpace(ev.Output); out != "" {
				_, _ = fmt.Fprintln(p.w, indentLines(out, "  "))
			}
			_, _ = fmt.Fprintf(p.w, "  [exit %d]\n", ev.ExitCode)
		case flow.OpDependency:
			_, _ = fmt.Fprintln(p.w, "  ready")
		case flow.OpSecret:
			_, _ = fmt.Fprintln(p.w, "  staged")
		default:
			// Other ops carry no command to show; only exec, dependency, and secret are reported.
		}
	}
}

// indentLines prefixes every line of s with prefix, so multi-line command output is set off
// from the progress markers around it.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

// runPlaybook implements `flynn playbook <ls|run>`: the multi-step procedures Flynn can
// carry out. Listing shows the catalog; run executes a playbook's flow with the effect
// ports wired (a sandboxed command runner and the dependency manager) and, on success,
// registers the supervised service the playbook produced.
//
//	flynn playbook ls                       list the playbook catalog
//	flynn playbook run <name> [json-input]  run a playbook with the given config
func runPlaybook(args []string, dataDir string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	ctx := context.Background()
	switch args[0] {
	case "ls", "list":
		return playbookList(ctx, dataDir)
	case "run":
		return playbookRun(ctx, dataDir, args[1:])
	default:
		return fmt.Errorf("playbook: unknown subcommand %q (want ls or run)", args[0])
	}
}

// playbookRuntime bundles the playbook store and a wired runner over the durable store,
// with the playbook and dependency catalogs synced in.
type playbookRuntime struct {
	store  *playbook.Store
	runner *playbook.Runner
	closer func() error
}

func openPlaybookRuntime(ctx context.Context, dataDir string) (*playbookRuntime, error) {
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	reg, err := missionRegistry()
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	rstore := durable.Resources(reg)

	pbStore := playbook.NewStore(rstore)
	if _, err := playbook.Sync(ctx, pbStore); err != nil {
		_ = durable.Close()
		return nil, err
	}
	// The dependency catalog must be present so a playbook's dependency steps resolve.
	depStore := dependency.NewStore(rstore)
	if _, err := dependency.Sync(ctx, depStore); err != nil {
		_ = durable.Close()
		return nil, err
	}

	// A playbook's commands and version probes run through one sandbox at the working
	// directory, so everything it executes is confined there.
	cwd, err := os.Getwd()
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	sb, err := sandbox.NewLocal(cwd)
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	depMgr := dependency.NewManager(depStore, fetch.New(), dataDir, dependency.WithProber(dependency.NewSandboxProber(sb)))
	runner := playbook.NewRunner(
		playbook.NewSandboxExecer(sb),
		playbook.NewManagerResolver(depMgr),
		service.NewStore(rstore),
		playbook.WithConfirmer(terminalConfirmer{}),
		playbook.WithObserver(terminalProgress{w: os.Stderr}),
		// The secret sink resolves a credential from the same vault-then-env chain the rest
		// of the agent uses, and materializes it into a provider through the sandbox, so a
		// playbook can provision a deployed workload's secrets without the value ever being
		// rendered into a command or the progress output.
		playbook.WithCredentialSink(playbook.NewCredentialSink(credentialSource(dataDir), sb)),
	)
	return &playbookRuntime{store: pbStore, runner: runner, closer: durable.Close}, nil
}

func playbookList(ctx context.Context, dataDir string) error {
	rt, err := openPlaybookRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	pbs, err := rt.store.List(ctx)
	if err != nil {
		return err
	}
	if len(pbs) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no playbooks in the catalog")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tREGISTERS\tDESCRIPTION")
	for _, p := range pbs {
		registers := "-"
		if p.Spec.Service != nil {
			registers = p.Spec.Service.Provider + " service"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, registers, oneLine(p.Spec.Description, 60))
	}
	return tw.Flush()
}

func playbookRun(ctx context.Context, dataDir string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: flynn playbook run <name> [json-input]")
	}
	name := args[0]
	config := map[string]any{}
	if rest := strings.TrimSpace(strings.Join(args[1:], " ")); rest != "" {
		if err := json.Unmarshal([]byte(rest), &config); err != nil {
			return fmt.Errorf("playbook: input is not valid JSON: %w", err)
		}
	}

	rt, err := openPlaybookRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	pb, err := rt.store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, playbook.ErrNotFound) {
			return fmt.Errorf("playbook: unknown playbook %q (see flynn playbook ls)", name)
		}
		return err
	}

	if err := fillDerivedNames(ctx, dataDir, pb, config); err != nil {
		return err
	}

	res, err := rt.runner.Run(ctx, pb, config)
	if err != nil {
		return err
	}
	if res.Service != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Playbook %q done. Registered service %q", name, res.Service.Name)
		if res.Service.Spec.URL != "" {
			_, _ = fmt.Fprintf(os.Stdout, " -> %s", res.Service.Spec.URL)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "Playbook %q done.\n", name)
	}
	if out, err := json.MarshalIndent(res.Output, "", "  "); err == nil {
		_, _ = fmt.Fprintf(os.Stdout, "Result:\n%s\n", out)
	}
	return nil
}
