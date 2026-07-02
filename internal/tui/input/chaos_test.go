package input_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/internal/tui/input"
)

var errConsole = errors.New("console torn down")

// drain collects every event until the stream closes or times out.
func drain(t *testing.T, r *input.Reader) []input.Event {
	t.Helper()
	var evs []input.Event
	for {
		select {
		case ev, open := <-r.Events():
			if !open {
				return evs
			}
			evs = append(evs, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("event stream never closed")
		}
	}
}

// TestChaosReaderDeliversThenClosesOnMidStreamFailure: input that flows and
// then dies (a hung console, a torn multiplexer) delivers everything decoded
// so far and closes the stream cleanly; nothing is lost, nothing hangs.
func TestChaosReaderDeliversThenClosesOnMidStreamFailure(t *testing.T) {
	src := testkit.FaultyReader(strings.NewReader("abc"), testkit.FailOnCall(2, errConsole))
	r := input.NewReader(src, clock.System{}, 5*time.Millisecond)
	defer r.Stop()

	evs := drain(t, r)
	want := []input.Event{
		input.Key{Code: 'a'}, input.Key{Code: 'b'}, input.Key{Code: 'c'},
	}
	if len(evs) != len(want) {
		t.Fatalf("events = %#v, want %#v", evs, want)
	}
	for i := range want {
		if evs[i] != want[i] {
			t.Fatalf("event %d = %#v, want %#v", i, evs[i], want[i])
		}
	}
}

// TestChaosReaderSurfacesAPartialSequenceAtFailure: a stream that dies
// mid-escape-sequence flushes the partial bytes as a visible Unknown event
// rather than swallowing them, so a diagnostic view can show what arrived.
func TestChaosReaderSurfacesAPartialSequenceAtFailure(t *testing.T) {
	src := testkit.FaultyReader(strings.NewReader("\x1b[1;5"), testkit.FailOnCall(2, errConsole))
	r := input.NewReader(src, clock.System{}, time.Hour) // idle flush must not race the failure path
	defer r.Stop()

	evs := drain(t, r)
	if len(evs) != 1 {
		t.Fatalf("events = %#v, want exactly the surfaced partial", evs)
	}
	u, isUnknown := evs[0].(input.Unknown)
	if !isUnknown || u.Seq != "\x1b[1;5" {
		t.Fatalf("event = %#v, want Unknown carrying the partial sequence", evs[0])
	}
}

// TestChaosReaderDeadFromTheFirstRead: input that fails immediately closes
// the stream with zero events and no hang, so the session start path
// degrades to its non-interactive fallback instead of wedging.
func TestChaosReaderDeadFromTheFirstRead(t *testing.T) {
	src := testkit.FaultyReader(nil, testkit.Always(errConsole))
	r := input.NewReader(src, clock.System{}, 5*time.Millisecond)
	defer r.Stop()

	if evs := drain(t, r); len(evs) != 0 {
		t.Fatalf("dead input produced events: %#v", evs)
	}
}

// TestChaosReaderRecoversAcrossTransientlySlowReads: a source that trickles
// one byte per read still assembles sequences correctly; framing never
// depends on read sizes.
func TestChaosReaderRecoversAcrossTransientlySlowReads(t *testing.T) {
	src := iotest{data: "\x1b[200~slow paste\x1b[201~\x1b[A"}
	r := input.NewReader(&src, clock.System{}, 50*time.Millisecond)
	defer r.Stop()

	evs := drain(t, r)
	want := []input.Event{
		input.Paste{Text: "slow paste"},
		input.Key{Code: input.KeyUp},
	}
	if len(evs) != len(want) {
		t.Fatalf("events = %#v, want %#v", evs, want)
	}
	for i := range want {
		if evs[i] != want[i] {
			t.Fatalf("event %d = %#v, want %#v", i, evs[i], want[i])
		}
	}
}

// iotest yields one byte per Read, then EOF.
type iotest struct {
	data string
	pos  int
}

func (r *iotest) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}
