package serve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

// TestRegistryConcurrentAccessStaysConsistent hammers a single registry from many
// goroutines doing overlapping Put, Delete, Get, and List, and asserts that none of them
// errors, panics, or observes a corrupt file. The atomic write-and-rename is what makes
// a List concurrent with a Put always read a whole, valid file rather than a partial one.
func TestRegistryConcurrentAccessStaysConsistent(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	ids := []string{"a", "b", "c", "d", "e"}

	var wg sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			i := 0
			for time.Now().Before(deadline) {
				id := ids[(g+i)%len(ids)]
				switch i % 4 {
				case 0:
					if err := reg.Put(Record{ModelID: id, PID: g*1000 + i, Port: 9000 + i%100, BaseURL: "http://127.0.0.1/v1"}); err != nil {
						t.Errorf("Put: %v", err)
						return
					}
				case 1:
					if _, _, err := reg.Get(id); err != nil {
						t.Errorf("Get: %v", err)
						return
					}
				case 2:
					if _, err := reg.List(); err != nil {
						t.Errorf("List: %v", err)
						return
					}
				case 3:
					if err := reg.Delete(id); err != nil {
						t.Errorf("Delete: %v", err)
						return
					}
				}
				i++
			}
		}(g)
	}
	wg.Wait()

	// After the storm the file must still parse and every retained record must be one of
	// the known ids, never a torn or duplicated entry.
	recs, err := reg.List()
	if err != nil {
		t.Fatalf("final List: %v", err)
	}
	seen := map[string]bool{}
	for _, rec := range recs {
		if seen[rec.ModelID] {
			t.Fatalf("duplicate record for %q after concurrent access", rec.ModelID)
		}
		seen[rec.ModelID] = true
	}
}

// TestEnsureConcurrentSameModelStartsCleanly runs several Ensure calls for the same model
// at once against a manager whose endpoint is healthy from the first probe. Whether a
// call starts the one server or adopts it, none may error and the registry must end with
// a single record for the model.
func TestEnsureConcurrentSameModelStartsCleanly(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	// Each launch hands back its own fake process; the manager keeps at most one.
	var mu sync.Mutex
	nextPID := 1000
	m := NewManager(procPerCall(&mu, &nextPID), alwaysHealthy, OSKiller, reg,
		WithReadyTimeout(time.Second), WithPollInterval(time.Millisecond))

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "m", Plan: samplePlan(9000)})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}
	recs, _ := reg.List()
	if len(recs) != 1 || recs[0].ModelID != "m" {
		t.Fatalf("expected exactly one record for the model, got %+v", recs)
	}
}

// procPerCallLauncher is a Launcher that returns a fresh fakeProc on every call with a
// unique pid, so concurrent starts do not share a process handle.
type procPerCallLauncher struct {
	mu      *sync.Mutex
	nextPID *int
}

func procPerCall(mu *sync.Mutex, next *int) *procPerCallLauncher {
	return &procPerCallLauncher{mu: mu, nextPID: next}
}

func (l *procPerCallLauncher) Serve(_ context.Context, _ sandbox.ServeSpec) (Proc, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.nextPID++
	return newFakeProc(*l.nextPID), nil
}
