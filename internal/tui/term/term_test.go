package term_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/term"
)

func TestSetupAndTeardownEmitNothingForZeroOptions(t *testing.T) {
	var b strings.Builder
	if err := term.Setup(&b, term.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := term.Teardown(&b, term.Options{}); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Fatalf("zero options wrote %q", b.String())
	}
}

func TestSetupEnablesEverySelectedMode(t *testing.T) {
	var b strings.Builder
	o := term.Options{BracketedPaste: true, FocusEvents: true, KittyKeyboard: true}
	if err := term.Setup(&b, o); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"\x1b[?2004h", "\x1b[?1004h", "\x1b[>1u"} {
		if !strings.Contains(out, want) {
			t.Fatalf("setup %q missing %q", out, want)
		}
	}
}

// TestProp_TeardownReversesSetup pins the lifecycle contract: for any option
// combination, every mode Setup enables is disabled by Teardown, and the
// disables come in reverse order of the enables, so stacked state (the kitty
// flag push) unwinds correctly.
func TestProp_TeardownReversesSetup(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		o := term.Options{
			BracketedPaste: rapid.Bool().Draw(rt, "paste"),
			FocusEvents:    rapid.Bool().Draw(rt, "focus"),
			KittyKeyboard:  rapid.Bool().Draw(rt, "kitty"),
		}
		var up, down strings.Builder
		if err := term.Setup(&up, o); err != nil {
			rt.Fatalf("setup: %v", err)
		}
		if err := term.Teardown(&down, o); err != nil {
			rt.Fatalf("teardown: %v", err)
		}
		pairs := []struct{ enable, disable string }{
			{"\x1b[?2004h", "\x1b[?2004l"},
			{"\x1b[?1004h", "\x1b[?1004l"},
			{"\x1b[>1u", "\x1b[<1u"},
		}
		lastUp, lastDown := -1, len(down.String())+1
		for _, p := range pairs {
			iUp := strings.Index(up.String(), p.enable)
			iDown := strings.Index(down.String(), p.disable)
			if (iUp >= 0) != (iDown >= 0) {
				rt.Fatalf("unbalanced mode: setup %q teardown %q", up.String(), down.String())
			}
			if iUp < 0 {
				continue
			}
			if iUp < lastUp {
				rt.Fatalf("setup order changed: %q", up.String())
			}
			if iDown > lastDown {
				rt.Fatalf("teardown is not in reverse order: %q", down.String())
			}
			lastUp, lastDown = iUp, iDown
		}
	})
}

func TestWatchResizeReportsOnlyChanges(t *testing.T) {
	var mu sync.Mutex
	w, h := 80, 24
	size := func() (int, int, error) {
		mu.Lock()
		defer mu.Unlock()
		return w, h, nil
	}
	resized := make(chan [2]int, 8)
	watcher := term.WatchResize(clock.System{}, 2*time.Millisecond, size, func(nw, nh int) {
		resized <- [2]int{nw, nh}
	})
	defer watcher.Stop()

	// No change: no report.
	select {
	case r := <-resized:
		t.Fatalf("spurious resize %v", r)
	case <-time.After(20 * time.Millisecond):
	}

	mu.Lock()
	w, h = 120, 40
	mu.Unlock()
	select {
	case r := <-resized:
		if r != [2]int{120, 40} {
			t.Fatalf("resize = %v, want [120 40]", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resize never reported")
	}

	// Stable again: no repeat reports for the same size.
	select {
	case r := <-resized:
		t.Fatalf("duplicate resize %v", r)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestWatcherStopHaltsCallbacks(t *testing.T) {
	calls := make(chan struct{}, 64)
	n := 0
	size := func() (int, int, error) {
		n++
		return n, n, nil // changes every tick
	}
	watcher := term.WatchResize(clock.System{}, time.Millisecond, size, func(int, int) {
		calls <- struct{}{}
	})
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never ticked")
	}
	watcher.Stop()
	// After Stop returns no further callback can fire; drain and confirm
	// silence.
	for {
		select {
		case <-calls:
			continue
		case <-time.After(20 * time.Millisecond):
			return
		}
	}
}

func TestWatchResizeSkipsErrTicks(t *testing.T) {
	var mu sync.Mutex
	fail := true
	size := func() (int, int, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return 0, 0, errSize
		}
		return 100, 50, nil
	}
	resized := make(chan [2]int, 4)
	watcher := term.WatchResize(clock.System{}, 2*time.Millisecond, size, func(w, h int) {
		resized <- [2]int{w, h}
	})
	defer watcher.Stop()
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	fail = false
	mu.Unlock()
	select {
	case r := <-resized:
		if r != [2]int{100, 50} {
			t.Fatalf("resize = %v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("size recovery never reported")
	}
}

var errSize = sizeError("size unavailable")

type sizeError string

func (e sizeError) Error() string { return string(e) }
