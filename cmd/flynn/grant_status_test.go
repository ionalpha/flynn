package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
)

func TestGrantStatusText(t *testing.T) {
	tests := []struct {
		name string
		p    sandbox.GrantProgress
		want string
	}{
		{"start", sandbox.GrantProgress{Dir: `C:\repo`}, `granting sandbox access (Esc cancels): C:\repo`},
		{"waited", sandbox.GrantProgress{Dir: `C:\repo`, Elapsed: 65200 * time.Millisecond}, `granting sandbox access (1m5s, Esc cancels): C:\repo`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantStatusText(tc.p, "Esc cancels", 0); got != tc.want {
				t.Fatalf("grantStatusText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFitPathLeft keeps the last element of a path whole, which is the name that
// tells two directories apart.
func TestFitPathLeft(t *testing.T) {
	const long = `~\Documents\Work\creator-projects-hub`
	tests := []struct {
		name  string
		path  string
		width int
		want  string
	}{
		{"fits", long, len(long), long},
		{"drops from the left", long, len(long) - 1, `...\Work\creator-projects-hub`},
		{"only the last element", long, 24, `...\creator-projects-hub`},
		{"narrower than the name", long, 5, `creator-projects-hub`},
		{"no width left", long, -3, `creator-projects-hub`},
		{"forward slashes", "~/Documents/Work/creator-projects-hub", 30, ".../Work/creator-projects-hub"},
		{"one element", "creator-projects-hub", 5, "creator-projects-hub"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fitPathLeft(tc.path, tc.width); got != tc.want {
				t.Fatalf("fitPathLeft(%q, %d) = %q, want %q", tc.path, tc.width, got, tc.want)
			}
		})
	}
}

// TestGrantStatusFitsTheTerminal cuts the path rather than letting the terminal
// cut its end off.
func TestGrantStatusFitsTheTerminal(t *testing.T) {
	p := sandbox.GrantProgress{Dir: `D:\one\two\three\four\five\creator-projects-hub`, Elapsed: 104 * time.Second}
	got := grantStatusText(p, "Esc cancels", 80)
	if !strings.HasSuffix(got, `\creator-projects-hub`) || !strings.Contains(got, "...\\") {
		t.Fatalf("status = %q", got)
	}
	if len(got) > 80-liveMargin {
		t.Fatalf("status is %d cells in 80: %q", len(got), got)
	}
}

// TestGrantWaitsAreRecorded checks each outcome reaches the record the transcript
// replays, with the directory in full, and that the host still sees every report.
func TestGrantWaitsAreRecorded(t *testing.T) {
	const dir = `C:\Work\creator-projects-hub`
	tests := []struct {
		name  string
		err   error
		state session.GrantState
		want  string
	}{
		{"finished", nil, session.GrantFinished, "sandbox access finished after 4m2s: " + dir},
		{"cancelled", context.Canceled, session.GrantCancelled, "sandbox access cancelled after 4m2s: " + dir},
		{"deadline", context.DeadlineExceeded, session.GrantCancelled, "sandbox access cancelled after 4m2s: " + dir},
		{"failed", errors.New("refused"), session.GrantFailed, "sandbox access failed after 4m2s: " + dir + " (refused)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := spine.NewMemoryLog()
			sess := session.New(log, bus.NewMemory())
			shown := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			report := grantReporter(ctx, sess, func(sandbox.GrantProgress) { shown++ })

			report(sandbox.GrantProgress{Dir: dir, Elapsed: time.Second, First: true})
			report(sandbox.GrantProgress{Dir: dir, Elapsed: 2 * time.Second})
			cancel() // Esc: the closing record is written after the context ends.
			report(sandbox.GrantProgress{Dir: dir, Elapsed: 242 * time.Second, Done: true, Err: tc.err})

			if shown != 3 {
				t.Fatalf("the host saw %d reports, want 3", shown)
			}
			evs, err := session.History(context.Background(), log, sess.ID())
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 2 || evs[0].SandboxGrant == nil || evs[1].SandboxGrant == nil {
				t.Fatalf("records = %+v, want a start and an end", evs)
			}
			if got := evs[1].SandboxGrant.State; got != tc.state {
				t.Fatalf("state = %q, want %q", got, tc.state)
			}
			var plain, verbose strings.Builder
			for _, ev := range evs {
				renderEvent(&plain, ev, false)
				renderEvent(&verbose, ev, true)
			}
			const started = "sandbox access started, waited 1s so far: " + dir
			if got := plain.String(); !strings.Contains(got, tc.want) || strings.Contains(got, started) {
				t.Errorf("inspect = %q, want only %q", got, tc.want)
			}
			if got := verbose.String(); !strings.Contains(got, tc.want) || !strings.Contains(got, started) {
				t.Errorf("inspect -v = %q, want %q and %q", got, started, tc.want)
			}
		})
	}
}

// TestPrintGrantProgressRepeatsSparingly keeps a run with no status line from
// printing a line a second, and from going silent for minutes.
func TestPrintGrantProgressRepeatsSparingly(t *testing.T) {
	var out bytes.Buffer
	show := printGrantProgress(&out)
	for s := 1; s <= 25; s++ {
		show(sandbox.GrantProgress{Dir: "repo", Elapsed: time.Duration(s) * time.Second})
	}
	show(sandbox.GrantProgress{Dir: "repo", Elapsed: 26 * time.Second, Done: true})
	show(sandbox.GrantProgress{Dir: "repo", Elapsed: time.Second})

	want := "granting sandbox access (1s, Ctrl-C cancels): repo\n" +
		"granting sandbox access (11s, Ctrl-C cancels): repo\n" +
		"granting sandbox access (21s, Ctrl-C cancels): repo\n" +
		"granting sandbox access (1s, Ctrl-C cancels): repo\n"
	if got := out.String(); got != want {
		t.Fatalf("printed:\n%swant:\n%s", got, want)
	}
}

// TestGrantAheadStops returns once the background grant has, so a closing session
// leaves no goroutine behind.
func TestGrantAheadStops(t *testing.T) {
	stop := grantAhead(context.Background(), governed.ShellPolicy{}, t.TempDir())
	stop()
}

// TestOnGrantTakesOverTheLiveLineUntilDone shows the wait on the live line and
// puts the line the turn had back when the grant ends.
func TestOnGrantTakesOverTheLiveLineUntilDone(t *testing.T) {
	host, _ := newSlashHost(t, nil)
	status := host.onGrant("thinking...")
	live := func() string { return strings.Join(host.live.Render(80), "\n") }

	status(sandbox.GrantProgress{Dir: "repo"})
	if got := live(); !strings.Contains(got, "granting sandbox access (Esc cancels): repo") {
		t.Fatalf("live line during a grant = %q", got)
	}
	status(sandbox.GrantProgress{Dir: "repo", Done: true})
	if got := live(); !strings.Contains(got, "thinking...") || strings.Contains(got, "granting") {
		t.Fatalf("live line after the grant = %q", got)
	}
}

// TestLineGrantStatusAnnouncesOncePerGrant keeps a minutes-long grant from
// printing a line a second, while the next grant is still announced.
func TestLineGrantStatusAnnouncesOncePerGrant(t *testing.T) {
	var out bytes.Buffer
	s := &replSession{out: &out}
	status := s.lineGrantStatus()
	announced := func() int { return strings.Count(out.String(), "granting sandbox access (Ctrl-C cancels): repo") }
	report := sandbox.GrantProgress{Dir: "repo"}

	status(report)
	status(report)
	status(sandbox.GrantProgress{Dir: "repo", Done: true})
	if got := announced(); got != 1 {
		t.Fatalf("one grant was announced %d times:\n%s", got, out.String())
	}
	status(report)
	if got := announced(); got != 2 {
		t.Fatalf("a later grant was not announced:\n%s", out.String())
	}
}
