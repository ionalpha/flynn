package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/dependency"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/playbook"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/service"
)

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
