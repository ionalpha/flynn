package diag

import (
	"bytes"
	"context"
	"runtime/pprof"
	"strings"
	"testing"
)

// TestProfilingIsFalseWithoutABundle is the invariant the whole labelling design
// rests on: a process nobody asked to profile pays for nothing.
func TestProfilingIsFalseWithoutABundle(t *testing.T) {
	if Profiling() {
		t.Fatal("profiling is on with no bundle open")
	}
	b, err := Start(Config{}) // empty Dir: disabled
	if err != nil || b != nil {
		t.Fatalf("Start(disabled) = %v, %v", b, err)
	}
	if Profiling() {
		t.Error("a disabled Start turned labelling on")
	}
}

// TestProfilingTracksTheBundle proves the flag opens with the bundle and closes
// with it, so a process that keeps running after Stop stops paying.
func TestProfilingTracksTheBundle(t *testing.T) {
	b, err := Start(Config{Dir: t.TempDir(), Interval: -1})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !Profiling() {
		t.Error("bundle open but labelling off")
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if Profiling() {
		t.Error("bundle sealed but labelling still on")
	}
}

// TestLabeledIsATransparentNoOpWhenOff pins that fn still runs, with the caller's
// own context, and that nothing is attached to it.
func TestLabeledIsATransparentNoOpWhenOff(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "carried")

	ran := false
	Labeled(ctx, func(inner context.Context) {
		ran = true
		if inner.Value(key{}) != "carried" {
			t.Error("Labeled replaced the caller's context")
		}
		pprof.ForLabels(inner, func(string, string) bool {
			t.Error("Labeled attached a label with no bundle open")
			return false
		})
	}, "action", "tool:bash")
	if !ran {
		t.Error("Labeled did not run fn")
	}
}

// TestLabeledAttachesAndUnwinds proves the labels reach the goroutine (not only
// the context) and do not outlive fn.
func TestLabeledAttachesAndUnwinds(t *testing.T) {
	b, err := Start(Config{Dir: t.TempDir(), Interval: -1})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := b.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	release := make(chan struct{})
	parked := make(chan struct{})

	Labeled(context.Background(), func(inner context.Context) {
		if v, ok := pprof.Label(inner, "action"); !ok || v != "tool:bash" {
			t.Errorf("action label = %q, %v", v, ok)
		}
		// A goroutine started here inherits the labels, which is what makes a leaked
		// goroutine attributable to the action that leaked it. The runtime exposes no
		// reader for another goroutine's labels, so read them where they are meant to
		// be read: out of a goroutine profile.
		go func() {
			close(parked)
			<-release
		}()
		<-parked
		if prof := goroutineProfile(t); !strings.Contains(prof, `"action":"tool:bash"`) {
			t.Errorf("a goroutine started under the label set is unattributed:\n%s", prof)
		}
	}, "action", "tool:bash")
	close(release)

	var after int
	pprof.ForLabels(context.Background(), func(string, string) bool { after++; return true })
	if after != 0 {
		t.Errorf("%d labels survived Labeled", after)
	}
}

// goroutineProfile renders the debug=1 goroutine profile, which is the only text
// form that prints each stack's pprof labels.
func goroutineProfile(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&b, 1); err != nil {
		t.Fatalf("goroutine profile: %v", err)
	}
	return b.String()
}

// TestLabelGoroutineLabelsTheCallerForLife covers the long-lived loops: a
// subscription pump or a queue worker labels itself once and stays attributable
// for as long as it runs.
func TestLabelGoroutineLabelsTheCallerForLife(t *testing.T) {
	b, err := Start(Config{Dir: t.TempDir(), Interval: -1})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := b.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	got := make(chan string, 1)
	go func() {
		ctx := LabelGoroutine(context.Background(), "component", "bus")
		v, _ := pprof.Label(ctx, "component")
		got <- v
	}()
	if v := <-got; v != "bus" {
		t.Errorf("component label = %q, want bus", v)
	}
}

// TestLabelGoroutineIsANoOpWhenOff keeps the disabled path free of a context
// allocation.
func TestLabelGoroutineIsANoOpWhenOff(t *testing.T) {
	ctx := context.Background()
	if got := LabelGoroutine(ctx, "component", "bus"); got != ctx {
		t.Error("LabelGoroutine wrapped the context with no bundle open")
	}
}

// TestOddLabelListDropsTheDanglingKey proves a caller's mistake mislabels a
// profile instead of panicking inside a governed run.
func TestOddLabelListDropsTheDanglingKey(t *testing.T) {
	b, err := Start(Config{Dir: t.TempDir(), Interval: -1})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := b.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	Labeled(context.Background(), func(inner context.Context) {
		if _, ok := pprof.Label(inner, "action"); !ok {
			t.Error("the paired label was dropped along with the dangling key")
		}
		if _, ok := pprof.Label(inner, "dangling"); ok {
			t.Error("a key with no value became a label")
		}
	}, "action", "tool:bash", "dangling")
}
