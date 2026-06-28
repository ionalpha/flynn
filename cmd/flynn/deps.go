package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/ionalpha/flynn/dependency"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/sandbox"
)

// runDeps implements `flynn deps <ls|check|install>`: the external command-line programs
// Flynn can provision for itself (a hosting provider's CLI, for example). Listing shows the
// catalog; check reports what is present on the host and whether it meets the version floor;
// install satisfies a dependency, using a present build that meets the floor or fetching and
// verifying the pinned one.
//
//	flynn deps ls                 list the dependency catalog
//	flynn deps check [name]       report detected vs needed for one or all
//	flynn deps install <name>     ensure a dependency is present, provisioning if needed
func runDeps(args []string, dataDir string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	ctx := context.Background()
	switch args[0] {
	case "ls", "list":
		return depsList(ctx, dataDir)
	case "check":
		return depsCheck(ctx, dataDir, args[1:])
	case "install", "ensure":
		return depsInstall(ctx, dataDir, args[1:])
	default:
		return fmt.Errorf("deps: unknown subcommand %q (want ls, check, or install)", args[0])
	}
}

// depsRuntime bundles the dependency store and manager over the durable store, with the
// official catalog synced in, so every deps command sees the same set of programs.
type depsRuntime struct {
	store  *dependency.Store
	mgr    *dependency.Manager
	closer func() error
}

func openDepsRuntime(ctx context.Context, dataDir string) (*depsRuntime, error) {
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	reg, err := missionRegistry()
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	store := dependency.NewStore(durable.Resources(reg))
	if _, err := dependency.Sync(ctx, store); err != nil {
		_ = durable.Close()
		return nil, err
	}
	// The version probe runs a present program through the sandbox at the working directory,
	// so detection is confined like any other command rather than spawning a process directly.
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
	mgr := dependency.NewManager(store, fetch.New(), dataDir, dependency.WithProber(dependency.NewSandboxProber(sb)))
	return &depsRuntime{store: store, mgr: mgr, closer: durable.Close}, nil
}

func depsList(ctx context.Context, dataDir string) error {
	rt, err := openDepsRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	deps, err := rt.store.List(ctx)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no dependencies in the catalog")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPIN\tMIN\tPLATFORMS\tDESCRIPTION")
	for _, d := range deps {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			d.Name, orDash(d.Spec.Pin), orDash(d.Spec.MinVersion), len(d.Spec.Releases), oneLine(d.Spec.Description, 50))
	}
	return tw.Flush()
}

func depsCheck(ctx context.Context, dataDir string, args []string) error {
	rt, err := openDepsRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	names := args
	if len(names) == 0 {
		deps, err := rt.store.List(ctx)
		if err != nil {
			return err
		}
		for _, d := range deps {
			names = append(names, d.Name)
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tVERSION\tPATH")
	for _, name := range names {
		rep, err := rt.mgr.Check(ctx, name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, depStatus(rep), orDash(rep.Version), orDash(rep.Path))
	}
	return tw.Flush()
}

// depStatus turns a check report into a short status word for the listing.
func depStatus(rep dependency.Report) string {
	switch {
	case rep.Present && rep.MeetsFloor:
		return "ok"
	case rep.Present && !rep.MeetsFloor && rep.CanProvision:
		return "below floor (installable)"
	case rep.Present && !rep.MeetsFloor:
		return "below floor"
	case rep.CanProvision:
		return "not installed (installable)"
	default:
		return "not installed"
	}
}

func depsInstall(ctx context.Context, dataDir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn deps install <name>")
	}
	rt, err := openDepsRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	name := args[0]
	if _, err := rt.store.Get(ctx, name); err != nil {
		if errors.Is(err, dependency.ErrNotFound) {
			return fmt.Errorf("deps: unknown dependency %q (see flynn deps ls)", name)
		}
		return err
	}
	res, err := rt.mgr.Resolve(ctx, name)
	if err != nil {
		return err
	}
	switch res.Source {
	case dependency.SourceSystem:
		_, _ = fmt.Fprintf(os.Stdout, "%s is present at %s", name, res.Path)
		if res.Version != "" {
			_, _ = fmt.Fprintf(os.Stdout, " (version %s)", res.Version)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	default:
		_, _ = fmt.Fprintf(os.Stdout, "Installed %s %s -> %s\n", name, res.Version, res.Path)
	}
	return nil
}
