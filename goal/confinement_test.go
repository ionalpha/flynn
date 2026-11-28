package goal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

func TestConfinementCovers(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	gone := filepath.Join(dir, "gone")
	at := func(d string, grant ...string) *Confinement { return &Confinement{Workdir: d, Grant: grant} }
	all := &Confinement{Workdir: dir, Unrestricted: true}

	for _, c := range []struct {
		name      string
		have, rec *Confinement
		ok        bool
	}{
		{"neither declares", nil, nil, true},
		{"no record", at(dir), nil, false},
		{"executor declares nothing", nil, at(dir), false},
		{"same", at(dir, "read"), at(dir, "read"), true},
		{"same directory spelled differently", at(dir + string(filepath.Separator) + "."), at(dir), true},
		{"narrower grant", at(dir, "read"), at(dir, "read", "bash"), true},
		{"wider grant", at(dir, "read", "bash"), at(dir, "read"), false},
		{"unrestricted over a restricted goal", all, at(dir, "read"), false},
		{"unrestricted goal", at(dir, "read"), all, true},
		{"another directory", at(other), at(dir), false},
		{"recorded directory gone", at(dir), at(gone), false},
		{"own directory gone", at(gone), at(dir), false},
		{"no directory on either", at(""), at(""), true},
		{"directory only on the record", at(""), at(dir), false},
	} {
		if err := c.have.Covers(c.rec); (err == nil) != c.ok {
			t.Errorf("%s: Covers = %v, want ok %t", c.name, err, c.ok)
		}
	}
}

func TestCheckConfinementRefusesAnUndecodableSpec(t *testing.T) {
	err := CheckConfinement(nil, resource.Resource{Spec: json.RawMessage(`{`)})
	if err == nil || fault.Classify(err) != fault.Terminal {
		t.Fatalf("err = %v, want terminal", err)
	}
}

// unfiltered stands in for a queue that predates claim filters.
type unfiltered struct{ jobs.Queue }

func (q unfiltered) Claim(ctx context.Context, p jobs.ClaimParams) ([]jobs.Job, error) {
	p.Filter = nil
	return q.Queue.Claim(ctx, p)
}

type rejectAll struct{}

func (rejectAll) Accepts(jobs.Job) bool { return false }

// The cost [jobs.Filter] documents: a queue that ignores it spends another
// process's attempts until the job is dead, though no worker ran it.
func TestAQueueIgnoringTheFilterExhaustsAnotherProcessesAttempts(t *testing.T) {
	ctx := context.Background()
	m := clock.NewManual(time.Unix(0, 0))
	q := jobs.NewMemory(jobs.WithClock(m))
	j, err := q.Enqueue(ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: StepJobKind, Payload: []byte("elsewhere"), MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(nil, unfiltered{q}, m, nil, WithClaimFilter(rejectAll{}))
	for range 3 {
		if ran, err := w.ProcessOnce(ctx); ran || err != nil {
			t.Fatalf("ProcessOnce = %t, %v, want nothing run", ran, err)
		}
		m.Advance(24 * time.Hour)
	}
	got, err := q.Get(ctx, j.ID)
	if err != nil || got.State != jobs.StateDead || got.Attempt != 2 {
		t.Fatalf("job = %s after %d attempts, %v, want dead after 2", got.State, got.Attempt, err)
	}
}

func TestWorkerDoesNotRunAStepItsQueueShouldHaveWithheld(t *testing.T) {
	ctx := context.Background()
	m := clock.NewManual(time.Unix(0, 0))
	q := jobs.NewMemory(jobs.WithClock(m))
	j, err := q.Enqueue(ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: StepJobKind, Payload: []byte("elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	// A nil store and executor: touching either panics.
	w := NewWorker(nil, unfiltered{q}, m, nil, WithClaimFilter(rejectAll{}))
	ran, err := w.ProcessOnce(ctx)
	if ran || err != nil {
		t.Fatalf("ProcessOnce = %t, %v, want nothing run", ran, err)
	}
	got, err := q.Get(ctx, j.ID)
	if err != nil || got.State != jobs.StateRunning {
		t.Fatalf("job = %s, %v, want left leased", got.State, err)
	}
}
