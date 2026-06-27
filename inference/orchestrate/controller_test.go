package orchestrate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
)

// fakeProvider returns a fixed desired state, or an error.
type fakeProvider struct {
	ds  DesiredState
	err error
}

func (f fakeProvider) Desired(context.Context) (DesiredState, error) {
	return f.ds, f.err
}

// recordingServer is an in-memory Server that applies launches and evicts to its own resident
// set and records the order of actions, so a test can assert what the controller did and that
// the loop converges. Optional failures model a runtime that will not start or stop.
type recordingServer struct {
	mu          sync.Mutex
	resident    map[string]Resident
	order       []string
	residentErr error
	evictErr    error
	failLaunch  int // fail the first N launches, then succeed
}

func newRecordingServer(resident ...Resident) *recordingServer {
	m := map[string]Resident{}
	for _, r := range resident {
		m[r.ModelID] = r
	}
	return &recordingServer{resident: m}
}

func (s *recordingServer) Resident(context.Context) ([]Resident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.residentErr != nil {
		return nil, s.residentErr
	}
	out := make([]Resident, 0, len(s.resident))
	for _, r := range s.resident {
		out = append(out, r)
	}
	return out, nil
}

func (s *recordingServer) Launch(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "launch:"+id)
	if s.failLaunch > 0 {
		s.failLaunch--
		return errors.New("runtime failed to start")
	}
	s.resident[id] = Resident{ModelID: id}
	return nil
}

func (s *recordingServer) Evict(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "evict:"+id)
	if s.evictErr != nil {
		return s.evictErr
	}
	delete(s.resident, id)
	return nil
}

func (s *recordingServer) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *recordingServer) isResident(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.resident[id]
	return ok
}

func newTestController(p Provider, s Server) *Controller {
	return NewController(p, s, clock.System{})
}

func TestReconcileLaunchesAndEvicts(t *testing.T) {
	srv := newRecordingServer(Resident{ModelID: "b", Footprint: 10})
	c := newTestController(fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}}, srv)
	if _, err := c.reconcile(context.Background(), serveKey{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !srv.isResident("a") || srv.isResident("b") {
		t.Fatalf("expected a resident and b evicted, actions=%v", srv.actions())
	}
}

func TestReconcileEvictsBeforeLaunch(t *testing.T) {
	// Freeing memory must precede claiming it, so every evict is ordered before every launch.
	srv := newRecordingServer(Resident{ModelID: "old", Footprint: 80})
	c := newTestController(fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "new", Footprint: 80, Priority: 9}}, Budget: 100}}, srv)
	if _, err := c.reconcile(context.Background(), serveKey{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	actions := srv.actions()
	lastEvict, firstLaunch := -1, len(actions)
	for i, a := range actions {
		if strings.HasPrefix(a, "evict:") {
			lastEvict = i
		}
		if strings.HasPrefix(a, "launch:") && i < firstLaunch {
			firstLaunch = i
		}
	}
	if lastEvict >= firstLaunch {
		t.Fatalf("evictions must precede launches, got %v", actions)
	}
}

func TestReconcileConvergedDoesNothing(t *testing.T) {
	srv := newRecordingServer(Resident{ModelID: "a", Footprint: 10})
	c := newTestController(fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}}, srv)
	if _, err := c.reconcile(context.Background(), serveKey{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(srv.actions()) != 0 {
		t.Fatalf("a converged state must take no action, got %v", srv.actions())
	}
}

func TestReconcileProviderErrorIsTransient(t *testing.T) {
	srv := newRecordingServer()
	c := newTestController(fakeProvider{err: errors.New("no desired state")}, srv)
	_, err := c.reconcile(context.Background(), serveKey{})
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("provider error must be transient, got %v", err)
	}
	if len(srv.actions()) != 0 {
		t.Fatal("no action may be taken when the desired state is unavailable")
	}
}

func TestReconcileObserveErrorIsTransient(t *testing.T) {
	srv := newRecordingServer()
	srv.residentErr = errors.New("cannot list servers")
	c := newTestController(fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a"}}, Budget: 100}}, srv)
	_, err := c.reconcile(context.Background(), serveKey{})
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("observe error must be transient, got %v", err)
	}
}

func TestReconcileApplyErrorIsTransient(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 1
	c := newTestController(fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}}, srv)
	_, err := c.reconcile(context.Background(), serveKey{})
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("apply error must be transient so the loop retries, got %v", err)
	}
}

func TestRunConvergesThenStops(t *testing.T) {
	srv := newRecordingServer()
	c := NewController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}},
		srv,
		clock.System{},
		WithResync(5*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for !srv.isResident("a") {
		select {
		case <-deadline:
			t.Fatal("controller did not converge to the desired state")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
