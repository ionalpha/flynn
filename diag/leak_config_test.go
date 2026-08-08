package diag

// What Start refuses, and when. Every rejection here lands at Start rather than at the
// first sample, so an operator learns their soak was misconfigured before leaving it
// running for a week, and nothing is opened on the way out: no profile runs and no
// half-written bundle is left for a reader to trust.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// TestNewWatchdogRejectsAConfigThatWouldFireOnNoise. Both rejections are at Start,
// not at the first sample: an operator learns their soak was misconfigured before
// they leave it running for a week.
func TestNewWatchdogRejectsAConfigThatWouldFireOnNoise(t *testing.T) {
	nodump := func(Finding) ([]string, error) { return nil, nil }

	if _, err := newWatchdog(LeakConfig{Window: 3}, nodump); err == nil {
		t.Error("a window of 3 was accepted; three points admit any slope")
	}
	for _, th := range []Threshold{{MinSlope: 0, MinDelta: 4}, {MinSlope: 1, MinDelta: 0}, {MinSlope: -1, MinDelta: -1}} {
		cfg := LeakConfig{Thresholds: map[string]Threshold{CounterGoroutines: th}}
		if _, err := newWatchdog(cfg, nodump); err == nil {
			t.Errorf("threshold %+v was accepted; a zero floor fires on noise", th)
		}
	}
	if _, err := newWatchdog(LeakConfig{}, nodump); err != nil {
		t.Errorf("the default config was rejected: %v", err)
	}
}

// TestStartRejectsAWatchdogWithNoSampler: the watchdog rides the timeline. Without
// one it would watch nothing, silently, which is the one failure an operator would
// never notice.
func TestStartRejectsAWatchdogWithNoSampler(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")

	b, err := Start(Config{Dir: dir, Interval: -1, Clock: clock.NewManual(epoch), Leak: &LeakConfig{}})
	if err == nil {
		_ = b.Stop()
		t.Fatal("Start accepted a leak watch with the sampler disabled")
	}
	if !strings.Contains(err.Error(), "sampler") {
		t.Errorf("error %q does not say the sampler is missing", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a rejected config created the bundle directory")
	}
}

// TestStartRejectsABadThresholdBeforeTouchingTheBundle. The same contract as above:
// nothing is opened, so no CPU profile runs and no half-written bundle is left for
// a reader to trust.
func TestStartRejectsABadThresholdBeforeTouchingTheBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	cfg := Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clock.NewManual(epoch),
		Leak:     &LeakConfig{Thresholds: map[string]Threshold{CounterGoroutines: {}}},
	}

	if b, err := Start(cfg); err == nil {
		_ = b.Stop()
		t.Fatal("Start accepted a zero threshold")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a rejected config created the bundle directory")
	}
}

// TestStartRejectsACounterWithNoRead. A Counter with a nil Read would be called on
// the baseline sample and panic before Start could return. Reject it at Start, with an
// error naming the counter, and touch no bundle.
func TestStartRejectsACounterWithNoRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	cfg := Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clock.NewManual(epoch),
		Counters: []Counter{{Name: "queued"}}, // Read is nil
	}

	b, err := Start(cfg)
	if err == nil {
		_ = b.Stop()
		t.Fatal("Start accepted a counter with no Read function")
	}
	if !strings.Contains(err.Error(), "queued") {
		t.Errorf("error %q does not name the offending counter", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a rejected config created the bundle directory")
	}
}

// TestStartRejectsACounterNameThatEscapesTheBundle. A firing counter's name is spliced
// into a leak dump filename, so a name with a path separator or ".." would write
// outside the bundle or into a directory that does not exist, and the evidence would be
// silently lost. Reject such a name at Start.
func TestStartRejectsACounterNameThatEscapesTheBundle(t *testing.T) {
	for _, name := range []string{"cache/entries", `a\b`, "..", ".", ""} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bundle")
			cfg := Config{
				Dir:      dir,
				Interval: time.Second,
				Clock:    clock.NewManual(epoch),
				Counters: []Counter{{Name: name, Read: func() float64 { return 0 }}},
			}
			if b, err := Start(cfg); err == nil {
				_ = b.Stop()
				t.Fatalf("Start accepted the unsafe counter name %q", name)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Error("a rejected config created the bundle directory")
			}
		})
	}
}

// TestSafeCounterNameAcceptsPlainNamesAndRejectsPaths pins the rule directly: a plain
// name is a valid filename component, and anything that could steer a dump out of the
// bundle is not.
func TestSafeCounterNameAcceptsPlainNamesAndRejectsPaths(t *testing.T) {
	for _, ok := range []string{"queued", "temp_dirs", "event.log.bytes", CounterGoroutines} {
		if !safeCounterName(ok) {
			t.Errorf("safeCounterName(%q) = false, want a plain name accepted", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../escape", "dir/leak"} {
		if safeCounterName(bad) {
			t.Errorf("safeCounterName(%q) = true, want an unsafe name rejected", bad)
		}
	}
}

// TestFromEnvEnablesTheWatchdog: a hosted instance whose command line an operator
// cannot change is still watchable.
func TestFromEnvEnablesTheWatchdog(t *testing.T) {
	t.Setenv(EnvLeakWatch, "1")
	if cfg := FromEnv(Config{}); cfg.Leak == nil {
		t.Error("FLYNN_LEAK_WATCH=1 did not enable the watchdog")
	}

	t.Setenv(EnvLeakWatch, "0")
	if cfg := FromEnv(Config{}); cfg.Leak != nil {
		t.Error("FLYNN_LEAK_WATCH=0 enabled the watchdog")
	}

	// An explicit config wins over the environment, and a false environment value
	// never disables a watchdog the caller asked for.
	explicit := &LeakConfig{Repeat: true}
	if cfg := FromEnv(Config{Leak: explicit}); cfg.Leak != explicit {
		t.Error("the environment overrode an explicit LeakConfig")
	}

	t.Setenv(EnvLeakWatch, "not-a-bool")
	if cfg := FromEnv(Config{}); cfg.Leak != nil {
		t.Error("an unparseable FLYNN_LEAK_WATCH enabled the watchdog")
	}
}
