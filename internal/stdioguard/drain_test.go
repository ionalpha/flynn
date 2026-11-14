package stdioguard

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// startGuard returns a guard draining a pipe the test writes strays into, and a function
// that closes the pipe and waits for the drain to finish, so the counters are final.
func startGuard(t *testing.T, opts ...Option) (g *Guard, strays io.Writer, finish func()) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	g = newGuard(io.Discard, pr, opts)
	var once sync.Once
	finish = func() {
		once.Do(func() {
			_ = pw.Close()
			<-g.done
			_ = pr.Close()
		})
	}
	t.Cleanup(finish)
	return g, pw, finish
}

func TestDrainCountsAndSamplesStrays(t *testing.T) {
	g, strays, finish := startGuard(t)
	_, _ = strays.Write([]byte("hello "))
	_, _ = strays.Write([]byte("world"))
	finish()

	s := g.Stats()
	if s.StrayBytes != 11 || string(s.Sample) != "hello world" {
		t.Fatalf("stats = %d bytes, sample %q", s.StrayBytes, s.Sample)
	}
}

func TestDrainSampleIsBoundedButCountIsNot(t *testing.T) {
	g, strays, finish := startGuard(t)
	total := 0
	chunk := bytes.Repeat([]byte("x"), 1000)
	for range 50 {
		n, _ := strays.Write(chunk)
		total += n
	}
	finish()

	s := g.Stats()
	if s.StrayBytes != int64(total) {
		t.Errorf("counted %d bytes, wrote %d", s.StrayBytes, total)
	}
	if len(s.Sample) != SampleLimit {
		t.Errorf("sample is %d bytes, want exactly %d", len(s.Sample), SampleLimit)
	}
}

func TestStatsSampleIsACopy(t *testing.T) {
	g, strays, finish := startGuard(t)
	_, _ = strays.Write([]byte("abc"))
	finish()

	g.Stats().Sample[0] = 'Z'
	if got := string(g.Stats().Sample); got != "abc" {
		t.Fatalf("mutating a returned sample changed the guard's: %q", got)
	}
}

func TestDrainForwardsToHandlerAndDestination(t *testing.T) {
	var (
		mu   sync.Mutex
		seen strings.Builder
		dest bytes.Buffer
	)
	g, strays, finish := startGuard(t,
		WithStrayHandler(func(p []byte) { mu.Lock(); seen.Write(p); mu.Unlock() }),
		WithStrayDestination(&dest),
	)
	_, _ = strays.Write([]byte("stray"))
	finish()

	mu.Lock()
	defer mu.Unlock()
	if seen.String() != "stray" || dest.String() != "stray" {
		t.Fatalf("handler saw %q, destination got %q", seen.String(), dest.String())
	}
	if g.Stats().StrayBytes != 5 {
		t.Fatalf("stats = %+v", g.Stats())
	}
}

func TestDrainSurvivesAFailingDestination(t *testing.T) {
	// A destination that errors must not stop the drain: a full pipe blocks every writer
	// in the process that believes it is writing to stdout.
	g, strays, finish := startGuard(t, WithStrayDestination(failWriter{}))
	_, _ = strays.Write([]byte("one"))
	_, _ = strays.Write([]byte("two"))
	finish()

	if got := g.Stats().StrayBytes; got != 6 {
		t.Fatalf("counted %d bytes after a failing destination, want 6", got)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("destination is gone") }

func TestNoDefaultDestination(t *testing.T) {
	var c config
	if c.dest != nil || c.onStray != nil {
		t.Fatal("a zero config forwards stray output somewhere")
	}
}
