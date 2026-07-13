package orchestrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/internal/inference/serve"
)

// brokenRegistryDir returns a directory whose server records cannot be read: a directory
// sits where the records file belongs.
func brokenRegistryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "servers.json"), 0o750); err != nil {
		t.Fatalf("make the registry path unreadable: %v", err)
	}
	return dir
}

// adapterManager builds a real serve.Manager over a registry seeded with the given records,
// with a prober that reports every recorded endpoint as answering, so the adapter observes
// exactly the resident set the registry holds.
func adapterManager(t *testing.T, recs ...serve.Record) (*serve.Manager, *serve.Registry, *[]int) {
	t.Helper()
	reg := serve.NewRegistry(t.TempDir())
	for _, rec := range recs {
		if err := reg.Put(rec); err != nil {
			t.Fatalf("seed the registry: %v", err)
		}
	}
	killed := &[]int{}
	m := serve.NewManager(
		nil, // no launcher: the adapter never starts a server itself
		func(context.Context, string) error { return nil },
		func(pid int) error { *killed = append(*killed, pid); return nil },
		reg,
	)
	return m, reg, killed
}

// TestServeAdapterObservesTheResidentSet checks the adapter reports what the manager has
// running, with the footprint the catalog gives it. A runtime whose load cannot be read is
// reported idle rather than busy: idle is the safe default for freeing memory, and a
// scheduler that read an unreadable runtime as busy could never evict it.
func TestServeAdapterObservesTheResidentSet(t *testing.T) {
	mgr, _, _ := adapterManager(t,
		serve.Record{ModelID: "big", BaseURL: "http://127.0.0.1:8080/v1", PID: 11, StartedAt: 100},
		serve.Record{ModelID: "small", BaseURL: "http://127.0.0.1:8081/v1", PID: 12, StartedAt: 200},
	)
	a := NewServeAdapter(mgr, nil, func(id string) int64 {
		if id == "big" {
			return 8 << 30
		}
		return 1 << 30
	})

	got, err := a.Resident(context.Background())
	if err != nil {
		t.Fatalf("Resident: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Resident = %d models, want 2", len(got))
	}
	byID := map[string]Resident{}
	for _, r := range got {
		byID[r.ModelID] = r
	}
	if byID["big"].Footprint != 8<<30 {
		t.Errorf("big footprint = %d, want the catalog's size", byID["big"].Footprint)
	}
	if byID["small"].LastUsed != 200 {
		t.Errorf("small last used = %d, want the recorded start time", byID["small"].LastUsed)
	}
	for _, r := range got {
		if r.Active {
			t.Errorf("%s reported active, but no stats source can say so", r.ModelID)
		}
	}
}

// TestServeAdapterReadsEveryModelAsZeroSizeWithoutACatalog checks a nil footprint lookup is a
// zero size rather than a panic: the adapter degrades to no size information.
func TestServeAdapterReadsEveryModelAsZeroSizeWithoutACatalog(t *testing.T) {
	mgr, _, _ := adapterManager(t, serve.Record{ModelID: "m", BaseURL: "http://127.0.0.1:8080/v1", PID: 11})
	a := NewServeAdapter(mgr, nil, nil)

	got, err := a.Resident(context.Background())
	if err != nil {
		t.Fatalf("Resident: %v", err)
	}
	if len(got) != 1 || got[0].Footprint != 0 {
		t.Fatalf("Resident = %+v, want one model of unknown size", got)
	}
}

// TestServeAdapterSurfacesAnObservationFailure checks a manager that cannot report what is
// running is an error, not an empty resident set: an empty set would have the scheduler
// launch a second copy of a model that is already up.
func TestServeAdapterSurfacesAnObservationFailure(t *testing.T) {
	// A registry whose file cannot be read: Status fails rather than reporting nothing.
	broken := serve.NewRegistry(brokenRegistryDir(t))

	a := NewServeAdapter(serve.NewManager(nil,
		func(context.Context, string) error { return nil },
		func(int) error { return nil }, broken), nil, nil)

	if _, err := a.Resident(context.Background()); err == nil {
		t.Fatal("an unreadable registry must be reported, not read as no models")
	}
}

// TestServeAdapterLaunchIsANoOpWithoutALauncher checks a nil launcher is a no-op rather than
// a failure: the adapter can be wired for observation only.
func TestServeAdapterLaunchIsANoOpWithoutALauncher(t *testing.T) {
	mgr, _, _ := adapterManager(t)
	a := NewServeAdapter(mgr, nil, nil)

	if err := a.Launch(context.Background(), "m", LaunchDegraded); err != nil {
		t.Fatalf("Launch with no launcher = %v, want a no-op", err)
	}
}

// TestServeAdapterLaunchPassesTheLevelDown checks the launch level reaches the injected
// launcher, so a recovery relaunch really does ask for a smaller footprint, and a launch
// failure is surfaced.
func TestServeAdapterLaunchPassesTheLevelDown(t *testing.T) {
	mgr, _, _ := adapterManager(t)
	var gotID string
	var gotLevel LaunchLevel
	want := errors.New("out of memory")
	a := NewServeAdapter(mgr, func(_ context.Context, id string, level LaunchLevel) error {
		gotID, gotLevel = id, level
		return want
	}, nil)

	if err := a.Launch(context.Background(), "m", LaunchMinimal); !errors.Is(err, want) {
		t.Fatalf("Launch error = %v, want %v", err, want)
	}
	if gotID != "m" || gotLevel != LaunchMinimal {
		t.Errorf("launcher called with (%q, %v), want (m, LaunchMinimal)", gotID, gotLevel)
	}
}

// TestServeAdapterEvictStopsTheServer checks eviction goes through the manager, killing the
// recorded process and dropping the record, and that evicting a model that is not running is
// a safe no-op.
func TestServeAdapterEvictStopsTheServer(t *testing.T) {
	mgr, reg, killed := adapterManager(t,
		serve.Record{ModelID: "m", BaseURL: "http://127.0.0.1:8080/v1", PID: 4242})
	a := NewServeAdapter(mgr, nil, nil)

	if err := a.Evict(context.Background(), "m"); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0] != 4242 {
		t.Errorf("killed = %v, want the recorded pid", *killed)
	}
	if _, ok, err := reg.Get("m"); err != nil || ok {
		t.Error("the evicted model kept its record")
	}
	if err := a.Evict(context.Background(), "m"); err != nil {
		t.Fatalf("a redundant evict = %v, want a no-op", err)
	}
}

// TestFootprintReadsANegativeSizeAsZero checks the budget accounting cannot be skewed by a
// malformed size: a negative footprint counts as nothing rather than as a credit against the
// budget, which would let extra models be admitted.
func TestFootprintReadsANegativeSizeAsZero(t *testing.T) {
	if got := footprint(-1); got != 0 {
		t.Errorf("footprint(-1) = %d, want 0", got)
	}
	if got := footprint(1 << 30); got != 1<<30 {
		t.Errorf("footprint(1GiB) = %d, want it unchanged", got)
	}
}
