package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/internal/archetype"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/internal/instance"
	"github.com/ionalpha/flynn/internal/migrate"
	"github.com/ionalpha/flynn/internal/playbook"
	"github.com/ionalpha/flynn/internal/profilestore"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// openStore opens the durable SQLite store at dsn, or an ephemeral in-memory one
// when dsn is empty (used by tests and one-off runs). The same store backs the
// runtime's resources and job queue and the learning loop's skills and memory.
func openStore(ctx context.Context, dsn string, opts ...sqlite.Option) (*sqlite.Store, error) {
	if dsn == "" {
		dsn = ":memory:"
	}
	return sqlite.Open(ctx, dsn, opts...)
}

// dataStoreFile is the path of the durable database file under a data directory, or empty
// for an ephemeral ("" or ":memory:") data dir that has no file on disk. It is the single
// definition of where the store lives, so opening it and checking whether it exists agree.
func dataStoreFile(dataDir string) string {
	if dataDir == "" || dataDir == ":memory:" {
		return ""
	}
	return filepath.Join(dataDir, "flynn.db")
}

// openDataStore opens the durable store under a data directory, creating the
// directory and resolving the database file inside it. An empty or ":memory:"
// dataDir opens an ephemeral store.
func openDataStore(ctx context.Context, dataDir string, opts ...sqlite.Option) (*sqlite.Store, error) {
	dsn := dataStoreFile(dataDir)
	if dsn != "" {
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			return nil, err
		}
	}
	store, err := openStore(ctx, dsn, opts...)
	if err != nil {
		return nil, explainStoreOpenError(err, dataDir)
	}
	if err := reconcileBundledSkills(ctx, store); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// reconcileBundledSkills brings the skills shipped in this binary into line with
// what the store holds, or removes them when the user asked to run without them.
//
// It happens here, where the store is opened, because that is the one place every
// command passes through, and because which skills exist is a property of the store
// rather than of the command reading it. A fresh install is seeded before its first
// turn, an install upgraded in place notices at the first command afterwards, and a
// start with nothing to do costs one list and no writes.
//
// A failure is fatal rather than a warning. The only way it fails is the store
// refusing to be read or written, and a command that cannot do either is not going
// to get further on its own work.
func reconcileBundledSkills(ctx context.Context, store *sqlite.Store) error {
	if bundledSkillsDisabled {
		_, err := bundled.Prune(ctx, store.Skills())
		return err
	}
	_, err := bundled.Seed(ctx, store.Skills())
	return err
}

// explainStoreOpenError turns a store-open failure the user can act on into a clear
// message with the recovery step, and passes anything else through unchanged. Today it
// recognises an incompatible on-disk schema (a database created by a different build):
// rather than a raw migrate error, it names the recovery, so a run never dead-ends on an
// internal message.
func explainStoreOpenError(err error, dataDir string) error {
	var schema *migrate.IncompatibleSchemaError
	if errors.As(err, &schema) && dataDir != "" && dataDir != ":memory:" {
		return fmt.Errorf("the state database in %s was created by an incompatible build (%s %s).\n"+
			"Recover with `flynn db reset` (it backs up the old database first), or run against a fresh `--data-dir`",
			dataDir, schema.Migration, schema.Reason)
	}
	return err
}

// listRuns prints the runs recorded in the durable store: their id, phase, step
// count, and objective, newest first, so a run can be found and then inspected or
// resumed by its id.
func listRuns(out io.Writer, dataDir string) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	goals, err := store.Resources(reg).ListAll(ctx, goal.Kind, nil)
	if err != nil {
		return err
	}
	if len(goals) == 0 {
		_, _ = fmt.Fprintln(out, "no runs yet")
		return nil
	}
	sort.Slice(goals, func(i, j int) bool { return goals[i].UpdatedHLC.Wall > goals[j].UpdatedHLC.Wall })
	for _, g := range goals {
		spec, _ := goal.DecodeSpec(g)
		st, _ := goal.DecodeStatus(g)
		phase := st.Phase
		if phase == "" {
			phase = goal.PhasePending
		}
		_, _ = fmt.Fprintf(out, "  %s  %-9s  step %d  %s\n", g.Name, phase, st.Steps, oneLine(spec.Objective, 60))
	}
	return nil
}

// inspectRun replays a past run's recorded events from the durable spine through
// the same renderer a live run uses, so any run is auditable after the fact by its
// id (printed when the run starts). verbose shows the tool arguments, outputs, and
// per-turn detail; the default view shows the shape of the run.
func inspectRun(out io.Writer, dataDir, runID string, verbose bool) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	events, err := session.History(ctx, store.Log(), runID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no run found with id %q under %s", runID, dataDir)
	}
	var meter usageMeter
	for _, ev := range events {
		renderEvent(out, ev, verbose)
		if ev.Usage != nil {
			meter.add(*ev.Usage)
		}
	}
	renderUsageSummary(out, meter)
	return nil
}

// renderUsageSummary writes the run's running token total as a final line, when any
// turn reported usage. It is the cumulative companion to the per-turn lines, so a
// run ends with one glance at what it cost and how much the prompt cache saved.
func renderUsageSummary(out io.Writer, meter usageMeter) {
	if s := meter.summary(); s != "" {
		_, _ = fmt.Fprintf(out, "%s\n", s)
	}
}

// regradeSkills re-runs every stored skill's check in a sandbox at the working
// directory, re-confirming the ones that still pass and retiring the ones that no
// longer do, then reports the tally.
func regradeSkills(out io.Writer, dataDir string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	verifier := governedVerifier(cwd)
	res, err := learn.Regrade(ctx, store.Skills(), state.Scope{}, verifier)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "regrade: %d checked, %d reconfirmed, %d retired\n",
		res.Checked, len(res.Reconfirmed), len(res.Retired))
	return nil
}

// missionRegistry builds the resource registry the durable store admits against:
// the core kinds plus the Goal kind the runtime drives and the ModelProfile kind a
// reliability measurement is recorded under.
func missionRegistry() (*resource.Registry, error) {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		return nil, err
	}
	if err := goal.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := budgetpkg.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := inbox.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := profilestore.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := archetype.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := instance.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := credential.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := extension.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := service.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := dependency.RegisterKind(reg); err != nil {
		return nil, err
	}
	if err := playbook.RegisterKind(reg); err != nil {
		return nil, err
	}
	return reg, nil
}
