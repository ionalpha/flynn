package dispatch_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/sandbox"
)

// openBundle starts a profile bundle in a temp dir. It returns the bundle
// directory and a seal function; the bundle is also sealed at test end, and Stop
// is idempotent, so a test that needs to read the members calls seal early.
// Profiling is process-global, so these tests never run in parallel.
func openBundle(t *testing.T) (dir string, seal func()) {
	t.Helper()
	dir = t.TempDir()
	b, err := diag.Start(diag.Config{Dir: dir, Interval: -1})
	if err != nil {
		t.Fatalf("diag.Start: %v", err)
	}
	seal = func() {
		if err := b.Stop(); err != nil {
			t.Errorf("bundle Stop: %v", err)
		}
	}
	t.Cleanup(seal)
	return dir, seal
}

// TestGovernLabelsTheAction proves the waist attributes its work: inside work,
// the action's identity is readable off the goroutine's own labels, not just off
// a context value the caller happened to pass.
func TestGovernLabelsTheAction(t *testing.T) {
	openBundle(t)
	d := dispatch.New()

	got := map[string]string{}
	err := d.Govern(context.Background(),
		dispatch.Action{Name: "tool:bash", Trust: sandbox.TrustSemi, Goal: "g-1"},
		func(ctx context.Context) (dispatch.Metering, error) {
			pprof.ForLabels(ctx, func(k, v string) bool {
				got[k] = v
				return true
			})
			return dispatch.Metering{}, nil
		})
	if err != nil {
		t.Fatalf("Govern: %v", err)
	}
	for k, want := range map[string]string{"action": "tool:bash", "trust": sandbox.TrustSemi.String(), "goal": "g-1"} {
		if got[k] != want {
			t.Errorf("label %s = %q, want %q (labels: %v)", k, got[k], want, got)
		}
	}
	if got["call"] == "" {
		t.Errorf("no call label, so two invocations of one action cannot be told apart: %v", got)
	}
}

// TestGovernOmitsEmptyGoalLabel keeps a profile grouped by goal from growing a
// bucket holding everything that ran under no goal.
func TestGovernOmitsEmptyGoalLabel(t *testing.T) {
	openBundle(t)
	d := dispatch.New()

	var seen bool
	err := d.Govern(context.Background(), dispatch.Action{Name: "model:call"},
		func(ctx context.Context) (dispatch.Metering, error) {
			_, seen = pprof.Label(ctx, "goal")
			return dispatch.Metering{}, nil
		})
	if err != nil {
		t.Fatalf("Govern: %v", err)
	}
	if seen {
		t.Error("an action with no goal carries a goal label")
	}
}

// TestUnprofiledGovernSetsNoLabels is the other half of the guard: a process with
// no bundle open must not carry a label map at all.
func TestUnprofiledGovernSetsNoLabels(t *testing.T) {
	d := dispatch.New()
	labelled := false
	err := d.Govern(context.Background(), dispatch.Action{Name: "tool:bash"},
		func(ctx context.Context) (dispatch.Metering, error) {
			pprof.ForLabels(ctx, func(string, string) bool { labelled = true; return false })
			return dispatch.Metering{}, nil
		})
	if err != nil {
		t.Fatalf("Govern: %v", err)
	}
	if labelled {
		t.Error("an unprofiled dispatch set pprof labels")
	}
}

// TestLeakedGoroutineNamesItsAction is the reason this exists. A goroutine left
// parked by a tool call inherits that call's labels, so the sealed bundle says
// which action leaked it without anyone bisecting a wall of identical stacks.
func TestLeakedGoroutineNamesItsAction(t *testing.T) {
	dir, seal := openBundle(t)
	d := dispatch.New()

	release := make(chan struct{})
	parked := make(chan struct{})
	t.Cleanup(func() { close(release) })

	err := d.Govern(context.Background(), dispatch.Action{Name: "tool:leaky"},
		func(context.Context) (dispatch.Metering, error) {
			go func() {
				close(parked)
				<-release // never returns before the bundle is sealed
			}()
			<-parked
			return dispatch.Metering{}, nil
		})
	if err != nil {
		t.Fatalf("Govern: %v", err)
	}

	// Seal the bundle here rather than at test end: the assertions read its members.
	// Stop is idempotent, so openBundle's cleanup stays harmless.
	seal()

	labels := readMember(t, dir, diag.MemberGoroutineLbl)
	if !strings.Contains(labels, `"action":"tool:leaky"`) {
		t.Errorf("%s does not attribute the parked goroutine to its action:\n%s", diag.MemberGoroutineLbl, labels)
	}
}

// readMember reads a bundle member as text.
func readMember(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
